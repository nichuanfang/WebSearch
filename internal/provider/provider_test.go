package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExaRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"results":[{"title":"T","url":"https://e.com","highlights":["h1","h2"],"publishedDate":"2026-01-01"}],"costDollars":{"total":0.004}}`))
	}))
	defer srv.Close()

	e := NewExa("k", srv.Client())
	e.BaseURL = srv.URL
	resp, err := e.Search(context.Background(), Query{Text: "go", NumResults: 3})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if gotKey != "k" {
		t.Errorf("认证头 = %q, 期望 k", gotKey)
	}
	if gotBody["type"] != "lite" {
		t.Errorf("type = %v, 期望 lite", gotBody["type"])
	}
	// 绝不能请求 contents.summary，那是按页额外计费的 LLM 项。
	contents, ok := gotBody["contents"].(map[string]any)
	if !ok {
		t.Fatal("contents 应为对象")
	}
	if _, bad := contents["summary"]; bad {
		t.Error("请求体不得包含 contents.summary（会额外计费）")
	}
	if contents["highlights"] != true {
		t.Error("应请求 highlights")
	}
	if resp.CostUSD != 0.004 {
		t.Errorf("花费 = %v, 期望 0.004", resp.CostUSD)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("结果数 = %d, 期望 1", len(resp.Results))
	}
	if resp.Results[0].Snippet != "h1 … h2" {
		t.Errorf("摘要 = %q, 期望 highlights 以 … 拼接", resp.Results[0].Snippet)
	}
}

func TestExaTruncatesNumResultsToTen(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	e := NewExa("k", srv.Client())
	e.BaseURL = srv.URL
	if _, err := e.Search(context.Background(), Query{Text: "go", NumResults: 20}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if got := gotBody["numResults"].(float64); got != 10 {
		t.Errorf("numResults = %v, 期望被截断到 10", got)
	}
}

func TestExaErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{402, KindQuotaExhausted},
		{429, KindRateLimited},
		{401, KindAuth},
		{400, KindInvalidRequest},
		{500, KindTransient},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(`{"error":"boom","tag":"X"}`))
		}))
		e := NewExa("k", srv.Client())
		e.BaseURL = srv.URL
		_, err := e.Search(context.Background(), Query{Text: "go", NumResults: 1})
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d 应返回错误", tc.status)
		}
		pe := AsError(err)
		if pe == nil {
			t.Fatalf("HTTP %d 错误类型应为 *Error", tc.status)
		}
		if pe.Kind != tc.want {
			t.Errorf("HTTP %d 分类 = %q, 期望 %q", tc.status, pe.Kind, tc.want)
		}
	}
}

func TestBraveRequestShape(t *testing.T) {
	var gotQuery, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotToken = r.Header.Get("X-Subscription-Token")
		w.Header().Set("x-ratelimit-limit", "50, 0")
		w.Header().Set("x-ratelimit-remaining", "49, 0")
		w.Header().Set("x-ratelimit-reset", "1, 1021268")
		w.Write([]byte(`{"web":{"results":[{"title":"T","url":"https://e.com","description":"D","age":"2026-01-01"}]}}`))
	}))
	defer srv.Close()

	b := NewBrave("k", srv.Client())
	b.BaseURL = srv.URL
	resp, err := b.Search(context.Background(), Query{Text: "go", NumResults: 5})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if gotToken != "k" {
		t.Errorf("认证头 = %q, 期望 k", gotToken)
	}
	if !strings.Contains(gotQuery, "q=go") {
		t.Errorf("查询参数 = %q, 期望包含 q=go", gotQuery)
	}
	if resp.Results[0].Snippet != "D" {
		t.Errorf("摘要 = %q, 期望 D", resp.Results[0].Snippet)
	}
	// 第二个窗口值是月度窗口：limit=0 表示不限量。
	if resp.ServerLimit != 0 {
		t.Errorf("服务端上限 = %v, 期望 0（不限量）", resp.ServerLimit)
	}
	if resp.ServerRemaining != 0 {
		t.Errorf("服务端剩余 = %v, 期望 0", resp.ServerRemaining)
	}
	if resp.ServerResetAt.IsZero() {
		t.Error("应解析出重置时刻")
	}
}

func TestBraveRequestHasNoNewsFreshness(t *testing.T) {
	// 工具层不再暴露 topic，Brave 请求不应带 freshness 参数。
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	b := NewBrave("k", srv.Client())
	b.BaseURL = srv.URL
	if _, err := b.Search(context.Background(), Query{Text: "go", NumResults: 1}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if strings.Contains(gotQuery, "freshness") {
		t.Errorf("请求不应包含 freshness 参数, 实际 %q", gotQuery)
	}
}

func TestBraveRateLimitHeadersMultiWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-limit", "50, 1000")
		w.Header().Set("x-ratelimit-remaining", "49, 997")
		w.Header().Set("x-ratelimit-reset", "1, 3600")
		w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	b := NewBrave("k", srv.Client())
	b.BaseURL = srv.URL
	resp, err := b.Search(context.Background(), Query{Text: "go", NumResults: 1})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	// 应取逗号分隔的第二个值。
	if resp.ServerLimit != 1000 {
		t.Errorf("月度上限 = %v, 期望 1000", resp.ServerLimit)
	}
	if resp.ServerRemaining != 997 {
		t.Errorf("月度剩余 = %v, 期望 997", resp.ServerRemaining)
	}
}

func TestBraveRetryAfterParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer srv.Close()

	b := NewBrave("k", srv.Client())
	b.BaseURL = srv.URL
	_, err := b.Search(context.Background(), Query{Text: "go", NumResults: 1})
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	pe := AsError(err)
	if pe == nil || pe.Kind != KindRateLimited {
		t.Fatalf("错误分类异常: %v", err)
	}
	if pe.RetryAfter.Seconds() != 45 {
		t.Errorf("Retry-After = %v, 期望 45s", pe.RetryAfter)
	}
}

func TestTavilyRequestShape(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"results":[{"title":"T","url":"https://e.com","content":"C","score":0.8}]}`))
	}))
	defer srv.Close()

	tv := NewTavily("k", srv.Client())
	tv.BaseURL = srv.URL
	resp, err := tv.Search(context.Background(), Query{Text: "go", NumResults: 5})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("认证头 = %q, 期望 Bearer k", gotAuth)
	}
	if gotBody["search_depth"] != "basic" {
		t.Errorf("search_depth = %v, 期望 basic", gotBody["search_depth"])
	}
	if resp.CostUnits != 1 {
		t.Errorf("消耗 = %v, 期望 1", resp.CostUnits)
	}
	if resp.Results[0].Score != 0.8 {
		t.Errorf("评分 = %v, 期望 0.8", resp.Results[0].Score)
	}
}

func TestTavilyAlwaysCostsOne(t *testing.T) {
	// 工具层固定 basic 深度，advanced（2 credits）已不再暴露。
	tv := NewTavily("k", http.DefaultClient)
	if c := tv.CostOf(Query{}); c != 1 {
		t.Errorf("消耗 = %v, 期望 1", c)
	}
}

func TestTavilyQuotaErrorMapping(t *testing.T) {
	for _, status := range []int{432, 433} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"detail":{"error":"limit"}}`))
		}))
		tv := NewTavily("k", srv.Client())
		tv.BaseURL = srv.URL
		_, err := tv.Search(context.Background(), Query{Text: "go", NumResults: 1})
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d 应返回错误", status)
		}
		pe := AsError(err)
		// Tavily 用 432/433 表示额度超限，映射到额度耗尽。
		if pe == nil || pe.Kind != KindQuotaExhausted {
			t.Errorf("HTTP %d 分类 = %v, 期望 quota_exhausted", status, pe.Kind)
		}
	}
}

func TestErrorKindForStatus(t *testing.T) {
	cases := map[int]ErrorKind{
		402: KindQuotaExhausted,
		429: KindRateLimited,
		401: KindAuth,
		403: KindAuth,
		400: KindInvalidRequest,
		500: KindTransient,
		503: KindTransient,
	}
	for status, want := range cases {
		if got := ErrorKindForStatus(status); got != want {
			t.Errorf("HTTP %d 分类 = %q, 期望 %q", status, got, want)
		}
	}
}
