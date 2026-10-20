package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nichuanfang/WebSearch/internal/fetch"
)

const fetchSampleHTML = `<!DOCTYPE html>
<html><head><title>抓取测试页</title></head>
<body><nav>导航</nav><article><h1>文章标题</h1><p>这是正文第一段。</p></article><footer>页脚</footer></body></html>`

// newFetchClient 建立一个连接测试 MCP 服务端的客户端会话。
func newFetchClient(t *testing.T, fetcher *fetch.Fetcher) *mcp.ClientSession {
	t.Helper()
	server := NewServer(nil, fetcher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("连接服务端失败: %v", err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("连接客户端失败: %v", err)
	}
	t.Cleanup(func() { clientSession.Close() })
	return clientSession
}

func TestWebFetchToolRegistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fetchSampleHTML))
	}))
	defer srv.Close()

	session := newFetchClient(t, fetch.New(srv.Client(), 0))
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("列出工具失败: %v", err)
	}
	var found *mcp.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == "web_fetch" {
			found = tools.Tools[i]
		}
	}
	if found == nil {
		t.Fatalf("未注册 web_fetch 工具, 现有: %v", tools.Tools)
	}
	schema, _ := json.Marshal(found.InputSchema)
	var sm map[string]any
	json.Unmarshal(schema, &sm)
	props := sm["properties"].(map[string]any)
	if _, has := props["url"]; !has {
		t.Error("schema 缺少 url 参数")
	}
	required := sm["required"].([]any)
	if len(required) == 0 || required[0] != "url" {
		t.Errorf("url 应为必填, 实际 required=%v", required)
	}
}

func TestWebFetchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fetchSampleHTML))
	}))
	defer srv.Close()

	session := newFetchClient(t, fetch.New(srv.Client(), 0))
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_fetch",
		Arguments: map[string]any{"url": srv.URL},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("返回错误: %+v", res.Content)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["title"] != "抓取测试页" {
		t.Errorf("title = %v", sc["title"])
	}
	content := sc["content"].(string)
	if !strings.Contains(content, "文章标题") {
		t.Errorf("content 异常: %s", content)
	}
	if strings.Contains(content, "页脚") {
		t.Errorf("应剔除页脚: %s", content)
	}
}

func TestWebFetchMaxChars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fetchSampleHTML))
	}))
	defer srv.Close()

	session := newFetchClient(t, fetch.New(srv.Client(), 1000))
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_fetch",
		Arguments: map[string]any{"url": srv.URL, "max_chars": 5},
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	sc := res.StructuredContent.(map[string]any)
	if len(sc["content"].(string)) > 5 {
		t.Errorf("内容应被截断到 5 字符, 实际 %d", len(sc["content"].(string)))
	}
	if sc["truncated"] != true {
		t.Errorf("truncated 应为 true, 实际 %v", sc["truncated"])
	}
}

func TestWebFetchInvalidURLReturnsErrorResult(t *testing.T) {
	session := newFetchClient(t, fetch.New(http.DefaultClient, 0))
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_fetch",
		Arguments: map[string]any{"url": "ftp://example.com/x"},
	})
	if err != nil {
		t.Fatalf("工具级错误不应导致协议异常: %v", err)
	}
	if !res.IsError {
		t.Error("非法 URL 应返回 IsError")
	}
	if len(res.Content) == 0 {
		t.Error("错误结果应包含说明文本")
	}
}
