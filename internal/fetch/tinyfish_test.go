package fetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nichuanfang/WebSearch/internal/provider"
)

func TestTinyFishFetcherReturnsMarkdown(t *testing.T) {
	var gotKey, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []any{map[string]any{
				"url":       "https://www.feishu.cn/pricing",
				"final_url": "https://www.feishu.cn/pricing",
				"title":     "版本对比 | 飞书",
				"text":      "# 定价\n\n免费版 ¥0",
				"format":    "markdown",
			}},
			"errors": []any{},
		})
	}))
	defer srv.Close()

	f := NewTinyFishFetcher("tf-key", srv.Client())
	f.BaseURL = srv.URL
	res, err := f.Fetch(context.Background(), "https://www.feishu.cn/pricing")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if gotKey != "tf-key" {
		t.Errorf("X-API-Key = %q", gotKey)
	}
	if !strings.Contains(gotBody, "https://www.feishu.cn/pricing") {
		t.Errorf("请求体未包含目标 URL: %s", gotBody)
	}
	if res.Title != "版本对比 | 飞书" || !strings.Contains(res.Content, "免费版") {
		t.Errorf("结果异常: %+v", res)
	}
	if res.URL != "https://www.feishu.cn/pricing" {
		t.Errorf("URL = %q", res.URL)
	}
}

func TestTinyFishFetcherEmptyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[],"errors":[{"url":"https://x.com","message":"blocked"}]}`))
	}))
	defer srv.Close()

	f := NewTinyFishFetcher("k", srv.Client())
	f.BaseURL = srv.URL
	_, err := f.Fetch(context.Background(), "https://x.com")
	if err == nil {
		t.Fatal("空结果应返回错误")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("错误应带上上游原因, 实际 %v", err)
	}
}

func TestTinyFishFetcherRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"slow down"}}`))
	}))
	defer srv.Close()

	f := NewTinyFishFetcher("k", srv.Client())
	f.BaseURL = srv.URL
	_, err := f.Fetch(context.Background(), "https://x.com")
	pe := provider.AsError(err)
	if pe == nil || pe.Kind != provider.KindRateLimited {
		t.Fatalf("错误分类 = %v, 期望 rate_limited", err)
	}
}

func TestTinyFishFetcherInvalidScheme(t *testing.T) {
	f := NewTinyFishFetcher("k", http.DefaultClient)
	if _, err := f.Fetch(context.Background(), "ftp://x/y"); err == nil {
		t.Fatal("非法协议应报错")
	}
}
