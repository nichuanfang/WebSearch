package fetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nichuanfang/WebSearch/internal/provider"
)

// fakeRemote 是可控的远程抓取替身。
type fakeRemote struct {
	name       string
	title      string
	emptyTitle bool
	content    string
	err        error
	calls      int
}

func (f *fakeRemote) Name() string { return f.name }
func (f *fakeRemote) Fetch(context.Context, string) (*Result, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	title := f.title
	if title == "" && !f.emptyTitle {
		title = "远程标题"
	}
	return &Result{URL: "https://example.com", Title: title, Content: f.content}, nil
}

func newManagerForTest(remote []RemoteFetcher, local *Fetcher) *Manager {
	return NewManager(remote, local, 5*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestManagerPrefersRemote(t *testing.T) {
	remote := &fakeRemote{name: "jina", content: "远程正文"}
	m := newManagerForTest([]RemoteFetcher{remote}, nil)
	res, err := m.Fetch(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if res.Content != "远程正文" {
		t.Errorf("应优先使用远程通道, 实际 %+v", res)
	}
	if remote.calls != 1 {
		t.Errorf("远程调用次数 = %d, 期望 1", remote.calls)
	}
}

func TestManagerFallsBackToLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>本地标题</title></head><body><p>本地正文</p></body></html>`))
	}))
	defer srv.Close()

	remote := &fakeRemote{name: "jina", err: errors.New("远程不可用")}
	m := newManagerForTest([]RemoteFetcher{remote}, New(srv.Client(), 0))
	res, err := m.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("应回落到本地抓取: %v", err)
	}
	if res.Content == "" || !strings.Contains(res.Content, "本地正文") {
		t.Errorf("本地正文异常: %+v", res)
	}
}

func TestManagerRemoteRateLimitEntersCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>T</title></head><body><p>本地</p></body></html>`))
	}))
	defer srv.Close()

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.Local)
	remote := &fakeRemote{name: "jina", err: provider.NewError("jina", provider.KindRateLimited, 429, "rate limited")}
	m := newManagerForTest([]RemoteFetcher{remote}, New(srv.Client(), 0))
	m.now = func() time.Time { return now }

	if _, err := m.Fetch(context.Background(), srv.URL); err != nil {
		t.Fatalf("首次应回落到本地: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := m.Fetch(context.Background(), srv.URL); err != nil {
		t.Fatalf("冷却期内应回落到本地: %v", err)
	}
	if remote.calls != 1 {
		t.Errorf("冷却期内不应再请求远程通道, 实际 %d 次", remote.calls)
	}
}

func TestManagerRejectsRawHTMLFromRemote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>本地标题</title></head><body><p>本地正文</p></body></html>`))
	}))
	defer srv.Close()

	remote := &fakeRemote{name: "jina", content: "<!DOCTYPE html><html><body>raw</body></html>"}
	m := newManagerForTest([]RemoteFetcher{remote}, New(srv.Client(), 0))
	// 远程结果缺少标题，应改走本地提取。
	_, _ = m.Fetch(context.Background(), srv.URL)

	remote2 := &Result{Title: "标题", Content: "<!DOCTYPE html><html><body>raw</body></html>"}
	if reason := remoteQualityIssue(remote2); reason != "远程返回原始 HTML" {
		t.Errorf("原始 HTML 应被识别, 实际 %q", reason)
	}
	if reason := remoteQualityIssue(&Result{Title: "", Content: "正文"}); reason != "" {
		t.Errorf("缺少标题不应被拒绝, 实际 %q", reason)
	}
	if reason := remoteQualityIssue(&Result{Title: "T", Content: "# 正文\n内容"}); reason != "" {
		t.Errorf("正常结果不应被拒绝, 实际 %q", reason)
	}
}

func TestManagerFillsTitleFromMarkdown(t *testing.T) {
	remote := &fakeRemote{name: "tinyfish", emptyTitle: true, content: "# Streaming API responses\n\n正文内容"}
	m := newManagerForTest([]RemoteFetcher{remote}, nil)
	res, err := m.Fetch(context.Background(), "https://example.com/a.md")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if res.Title != "Streaming API responses" {
		t.Errorf("应从首个标题行补位, 实际 %q", res.Title)
	}
}

func TestManagerReportsAllCauses(t *testing.T) {
	remote := &fakeRemote{name: "jina", err: errors.New("远程失败")}
	m := newManagerForTest([]RemoteFetcher{remote}, nil)
	_, err := m.Fetch(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("全部失败应返回错误")
	}
	if !strings.Contains(err.Error(), "jina") {
		t.Errorf("错误应包含远程通道原因, 实际 %q", err.Error())
	}
}

func TestSplitJinaMarkdown(t *testing.T) {
	raw := "Title: The Server and Client Boundary\n\nURL Source: https://nextjs.org/docs\n\nMarkdown Content:\n# 正文\n内容"
	title, content := splitJinaMarkdown(raw)
	if title != "The Server and Client Boundary" {
		t.Errorf("标题 = %q", title)
	}
	if content != "# 正文\n内容" {
		t.Errorf("正文 = %q", content)
	}
}

func TestJinaReaderFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/https://example.com/") {
			t.Errorf("请求路径 = %q, 应包含目标 URL", r.URL.Path)
		}
		w.Write([]byte("Title: 示例\n\nURL Source: https://example.com\n\nMarkdown Content:\n正文内容"))
	}))
	defer srv.Close()

	j := NewJinaReader(srv.Client())
	j.BaseURL = srv.URL
	res, err := j.Fetch(context.Background(), "https://example.com/page")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if res.Title != "示例" || res.Content != "正文内容" {
		t.Errorf("结果异常: %+v", res)
	}
}

func TestJinaReaderRateLimitClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	j := NewJinaReader(srv.Client())
	j.BaseURL = srv.URL
	_, err := j.Fetch(context.Background(), "https://example.com")
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	pe := provider.AsError(err)
	if pe == nil || pe.Kind != provider.KindRateLimited {
		t.Fatalf("错误分类 = %v, 期望 rate_limited", err)
	}
}
