package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const tinyFishBody = `{
  "query": "Go 泛型 使用教程",
  "results": [
    {"position":1,"site_name":"go.dev","title":"Tutorial: Getting started with generics",
     "snippet":"This tutorial introduces the basics of generics in Go.","url":"https://go.dev/doc/tutorial/generics","date":"Oct 18, 2023"},
    {"position":2,"site_name":"runoob.com","title":"Go 语言泛型",
     "snippet":"泛型是 Go 语言在 1.18 版本中引入的重要特性。","url":"https://www.runoob.com/go/go-generics.html"},
    {"position":3,"site_name":"example.com","title":"第三条","snippet":"三","url":"https://example.com/3"}
  ],
  "total_results": 3
}`

func TestTinyFishSearchParsesResults(t *testing.T) {
	var gotKey, gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		gotQuery = r.URL.Query().Get("query")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(tinyFishBody))
	}))
	defer srv.Close()

	tf := NewTinyFish("test-key", srv.Client())
	tf.BaseURL = srv.URL
	resp, err := tf.Search(context.Background(), Query{Text: "Go 泛型 使用教程", NumResults: 2})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("X-API-Key = %q", gotKey)
	}
	if gotQuery != "Go 泛型 使用教程" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotPath != "/" {
		t.Errorf("路径 = %q, 期望根路径", gotPath)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("结果数 = %d, 期望按 num_results 截断到 2", len(resp.Results))
	}
	first := resp.Results[0]
	if first.Title != "Tutorial: Getting started with generics" {
		t.Errorf("标题 = %q", first.Title)
	}
	if first.URL != "https://go.dev/doc/tutorial/generics" {
		t.Errorf("链接 = %q", first.URL)
	}
	if first.Date != "2023-10-18" {
		t.Errorf("日期 = %q, 期望归一化为 2023-10-18", first.Date)
	}
	if resp.Results[1].Date != "" {
		t.Errorf("缺省日期应为空, 实际 %q", resp.Results[1].Date)
	}
	if resp.CostUnits != 0 {
		t.Errorf("免密钥通道不应消耗额度, 实际 %v", resp.CostUnits)
	}
}

func TestTinyFishMissingKeyIsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"MISSING_API_KEY","message":"X-API-Key header is required"}}`))
	}))
	defer srv.Close()

	tf := NewTinyFish("", srv.Client())
	tf.BaseURL = srv.URL
	_, err := tf.Search(context.Background(), Query{Text: "q"})
	if err == nil {
		t.Fatal("缺少密钥应返回错误")
	}
	pe := AsError(err)
	if pe == nil || pe.Kind != KindAuth {
		t.Fatalf("错误分类 = %v, 期望 auth_error", err)
	}
}

func TestTinyFishRateLimitIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"too many requests"}}`))
	}))
	defer srv.Close()

	tf := NewTinyFish("k", srv.Client())
	tf.BaseURL = srv.URL
	_, err := tf.Search(context.Background(), Query{Text: "q"})
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

func TestTinyFishInsufficientCreditsIsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"error":{"code":"INSUFFICIENT_CREDITS","message":"wallet balance exhausted"}}`))
	}))
	defer srv.Close()

	tf := NewTinyFish("k", srv.Client())
	tf.BaseURL = srv.URL
	_, err := tf.Search(context.Background(), Query{Text: "q"})
	pe := AsError(err)
	if pe == nil || pe.Kind != KindRateLimited {
		t.Fatalf("余额耗尽应归为限流类, 实际 %v", err)
	}
}

func TestTinyFishDoesNotClobberLongSnippet(t *testing.T) {
	long := strings.Repeat("很长的摘要", 300)
	body := `{"results":[{"title":"t","url":"https://e.com","snippet":"` + long + `"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	tf := NewTinyFish("k", srv.Client())
	tf.BaseURL = srv.URL
	resp, err := tf.Search(context.Background(), Query{Text: "q", NumResults: 1})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if got := len(resp.Results[0].Snippet); got > maxSnippetChars+3 {
		t.Errorf("摘要长度 = %d, 应被限制在 %d 左右", got, maxSnippetChars)
	}
}

func TestNormalizeTinyFishDate(t *testing.T) {
	cases := map[string]string{
		"Oct 18, 2023": "2023-10-18",
		"2026-10-20":   "2026-10-20",
		"3 months ago": "3 months ago",
		"":             "",
	}
	for raw, want := range cases {
		if got := normalizeTinyFishDate(raw); got != want {
			t.Errorf("normalizeTinyFishDate(%q) = %q, 期望 %q", raw, got, want)
		}
	}
}
