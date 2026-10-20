package fetch

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleHTML = `<!DOCTYPE html>
<html>
<head>
  <title>示例标题 - 站点名</title>
  <script>var x = "should not appear";</script>
  <style>body { color: red; }</style>
</head>
<body>
  <nav>导航 首页 关于</nav>
  <article>
    <h1>主标题</h1>
    <p>第一段正文内容。</p>
    <h2>二级标题</h2>
    <p>第二段正文内容，包含更多细节。</p>
    <ul><li>列表项一</li><li>列表项二</li></ul>
    <pre><code>func main() {}</code></pre>
  </article>
  <aside>侧边栏广告</aside>
  <footer>页脚版权</footer>
</body>
</html>`

func TestExtractTitleAndContent(t *testing.T) {
	title, content := Extract(sampleHTML)
	if title != "示例标题 - 站点名" {
		t.Errorf("标题 = %q, 期望 %q", title, "示例标题 - 站点名")
	}
	for _, want := range []string{"主标题", "第一段正文内容。", "二级标题", "列表项一"} {
		if !strings.Contains(content, want) {
			t.Errorf("正文缺少 %q\n实际: %s", want, content)
		}
	}
}

func TestExtractKeepsHeadingLevels(t *testing.T) {
	_, content := Extract(sampleHTML)
	if !strings.Contains(content, "# 主标题") {
		t.Errorf("应保留 h1 的 Markdown 标记\n实际: %s", content)
	}
	if !strings.Contains(content, "## 二级标题") {
		t.Errorf("应保留 h2 的 Markdown 标记\n实际: %s", content)
	}
}

func TestExtractSkipsNoise(t *testing.T) {
	_, content := Extract(sampleHTML)
	for _, unwanted := range []string{"should not appear", "color: red", "导航 首页 关于", "侧边栏广告", "页脚版权"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("正文不应包含 %q\n实际: %s", unwanted, content)
		}
	}
}

func TestFetchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("应设置 User-Agent")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(sampleHTML))
	}))
	defer srv.Close()

	f := New(srv.Client(), 0)
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if res.Title != "示例标题 - 站点名" {
		t.Errorf("标题 = %q", res.Title)
	}
	if !strings.Contains(res.Content, "主标题") {
		t.Errorf("正文异常: %s", res.Content)
	}
	if res.Truncated {
		t.Error("短内容不应被标记为截断")
	}
}

func TestFetchFollowsRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(sampleHTML))
	}))
	defer final.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/final", http.StatusFound)
	}))
	defer redirect.Close()

	f := New(redirect.Client(), 0)
	res, err := f.Fetch(context.Background(), redirect.URL+"/start")
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if !strings.HasSuffix(res.URL, "/final") {
		t.Errorf("最终 URL = %q, 期望以 /final 结尾", res.URL)
	}
}

func TestFetchTruncates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(sampleHTML))
	}))
	defer srv.Close()

	f := New(srv.Client(), 10)
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("抓取失败: %v", err)
	}
	if len(res.Content) > 10 {
		t.Errorf("内容长度 = %d, 期望不超过 10", len(res.Content))
	}
	if !res.Truncated {
		t.Error("被截断时应标记 Truncated")
	}
}

func TestFetchRejectsBadScheme(t *testing.T) {
	f := New(http.DefaultClient, 0)
	_, err := f.Fetch(context.Background(), "ftp://example.com/file")
	if err == nil {
		t.Fatal("非 http(s) 协议应报错")
	}
	if !strings.Contains(err.Error(), "http") {
		t.Errorf("错误信息 = %q, 应说明仅支持 http/https", err.Error())
	}
}

func TestFetchRejectsNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4"))
	}))
	defer srv.Close()

	f := New(srv.Client(), 0)
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("非 HTML 应报错")
	}
	if !strings.Contains(err.Error(), "HTML") {
		t.Errorf("错误信息 = %q, 应说明不是 HTML", err.Error())
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := New(srv.Client(), 0)
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("404 应报错")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误信息 = %q, 应包含状态码", err.Error())
	}
}

func TestFetchEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>空</title></head><body><script>x</script></body></html>`))
	}))
	defer srv.Close()

	f := New(srv.Client(), 0)
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("无正文应报错")
	}
	if !strings.Contains(err.Error(), "正文") {
		t.Errorf("错误信息 = %q, 应说明没有正文", err.Error())
	}
}

func TestFetchGzip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(sampleHTML))
		gz.Close()
	}))
	defer srv.Close()

	f := New(srv.Client(), 0)
	res, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("gzip 响应抓取失败: %v", err)
	}
	if !strings.Contains(res.Content, "主标题") {
		t.Errorf("gzip 解压后正文异常: %s", res.Content)
	}
}
