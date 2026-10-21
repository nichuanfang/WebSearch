package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nichuanfang/WebSearch/internal/provider"
)

const doctorRSS = `<?xml version="1.0" encoding="utf-8" ?><rss version="2.0"><channel>` +
	`<item><title>check</title><link>https://example.com/check</link>` +
	`<description>ok</description></item></channel></rss>`

func TestCheckBingReportsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Write([]byte(doctorRSS))
	}))
	defer srv.Close()

	bing := provider.NewBing(srv.Client())
	bing.BaseURL = srv.URL
	var out bytes.Buffer
	checkFallback(context.Background(), bing, &out)

	got := out.String()
	if !strings.Contains(got, "bing") || !strings.Contains(got, "正常") {
		t.Errorf("输出 = %q, 应报告 bing 正常", got)
	}
	if !strings.Contains(got, "不扣额度") {
		t.Errorf("输出 = %q, 应说明不扣额度", got)
	}
}

func TestCheckBingReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("blocked"))
	}))
	defer srv.Close()

	bing := provider.NewBing(srv.Client())
	bing.BaseURL = srv.URL
	var out bytes.Buffer
	checkFallback(context.Background(), bing, &out)

	got := out.String()
	if !strings.Contains(got, "失败") {
		t.Errorf("输出 = %q, 应报告失败", got)
	}
	if !strings.Contains(got, "rate_limited") {
		t.Errorf("输出 = %q, 应包含错误分类", got)
	}
}
