package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const bingRSSSample = `<?xml version="1.0" encoding="utf-8" ?><rss version="2.0"><channel>` +
	`<title>必应：飞书 多维表格</title><link>https://cn.bing.com/search?q=x</link>` +
	`<item><title>飞书多维表格官网</title><link>https://www.feishu.cn/product/base</link>` +
	`<description>AI 驱动的表格与业务系统搭建平台</description>` +
	`<pubDate>Mon, 20 Oct 2026 13:07:00 GMT</pubDate></item>` +
	`<item><title>快速上手多维表格</title><link>https://www.feishu.cn/hc/base</link>` +
	`<description>从入门到精通</description></item>` +
	`<item><title>第三篇</title><link>https://example.com/3</link>` +
	`<description>第三条</description></item>` +
	`</channel></rss>`

func newBingTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestBingSendsRSSRequestToChinaMarketForCJK(t *testing.T) {
	var gotPath, gotQuery, gotFormat, gotLang string
	srv := newBingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		gotFormat = r.URL.Query().Get("format")
		gotLang = r.Header.Get("Accept-Language")
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Write([]byte(bingRSSSample))
	})

	b := NewBing(srv.Client())
	// 测试服务器无法区分主机名，这里验证路径与查询参数；
	// 市场选择由 TestBingEndpointByQueryLanguage 单独覆盖。
	b.BaseURL = srv.URL
	resp, err := b.Search(context.Background(), Query{Text: "飞书 多维表格", NumResults: 2})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if gotPath != "/search" {
		t.Errorf("请求路径 = %q, 期望 /search", gotPath)
	}
	if gotQuery != "飞书 多维表格" {
		t.Errorf("q = %q, 期望原样传递", gotQuery)
	}
	if gotFormat != "rss" {
		t.Errorf("format = %q, 期望 rss", gotFormat)
	}
	if !strings.Contains(gotLang, "zh-CN") {
		t.Errorf("Accept-Language = %q, 应包含 zh-CN", gotLang)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("结果数 = %d, 期望按 num_results 截断到 2", len(resp.Results))
	}
	if resp.Results[0].Title != "飞书多维表格官网" {
		t.Errorf("标题 = %q", resp.Results[0].Title)
	}
	if resp.Results[0].URL != "https://www.feishu.cn/product/base" {
		t.Errorf("链接 = %q", resp.Results[0].URL)
	}
	if resp.Results[0].Snippet != "AI 驱动的表格与业务系统搭建平台" {
		t.Errorf("摘要 = %q", resp.Results[0].Snippet)
	}
	if resp.Results[0].Date != "2026-10-20" {
		t.Errorf("日期 = %q, 期望归一化为 2026-10-20", resp.Results[0].Date)
	}
	if resp.CostUnits != 0 {
		t.Errorf("兜底不应消耗额度, 实际 %v", resp.CostUnits)
	}
}

func TestBingEndpointByQueryLanguage(t *testing.T) {
	cases := map[string]string{
		"飞书 多维表格":        bingChinaHost,
		"OpenAI pricing": bingGlobalHost,
		"こんにちは":          bingChinaHost,
		"안녕하세요":          bingChinaHost,
	}
	for query, want := range cases {
		b := NewBing(http.DefaultClient)
		if got := b.endpoint(query); got != want {
			t.Errorf("查询 %q 的端点 = %q, 期望 %q", query, got, want)
		}
	}
}

func TestBingTruncatesNumResultsToTwenty(t *testing.T) {
	var items strings.Builder
	for i := 0; i < 25; i++ {
		items.WriteString(`<item><title>T</title><link>https://example.com/`)
		items.WriteString(string(rune('a' + i)))
		items.WriteString(`</link><description>D</description></item>`)
	}
	body := `<?xml version="1.0"?><rss version="2.0"><channel>` + items.String() + `</channel></rss>`
	srv := newBingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
	b := NewBing(srv.Client())
	b.BaseURL = srv.URL
	resp, err := b.Search(context.Background(), Query{Text: "q", NumResults: 100})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(resp.Results) != 20 {
		t.Errorf("结果数 = %d, 期望截断到 20", len(resp.Results))
	}
}

func TestBingRateLimitIsClassified(t *testing.T) {
	srv := newBingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("too many requests"))
	})
	b := NewBing(srv.Client())
	b.BaseURL = srv.URL
	_, err := b.Search(context.Background(), Query{Text: "q"})
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	pe := AsError(err)
	if pe == nil || pe.Kind != KindRateLimited {
		t.Fatalf("错误分类 = %v, 期望 rate_limited", err)
	}
	if pe.RetryAfter.Seconds() != 30 {
		t.Errorf("Retry-After = %v, 期望 30s", pe.RetryAfter)
	}
}

func TestBingChallengePageIsClassified(t *testing.T) {
	srv := newBingTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Captcha</title></head><body>verify</body></html>`))
	})
	b := NewBing(srv.Client())
	b.BaseURL = srv.URL
	_, err := b.Search(context.Background(), Query{Text: "q"})
	if err == nil {
		t.Fatal("挑战页应返回错误")
	}
	pe := AsError(err)
	if pe == nil || pe.Kind != KindRateLimited {
		t.Fatalf("错误分类 = %v, 期望 rate_limited", err)
	}
}

func TestNormalizeBingDate(t *testing.T) {
	cases := map[string]string{
		"周二, 20 10月 2026 23:01:00 GMT":    "2026-10-20",
		"周一, 19 10月 2026 00:01:00 GMT":    "2026-10-19",
		"Mon, 20 Oct 2026 13:07:00 GMT":   "2026-10-20",
		"Tue, 20 Oct 2026 13:07:00 +0800": "2026-10-20",
		"":                                "",
		"unknown":                         "unknown",
	}
	for raw, want := range cases {
		if got := normalizeBingDate(raw); got != want {
			t.Errorf("normalizeBingDate(%q) = %q, 期望 %q", raw, got, want)
		}
	}
}
