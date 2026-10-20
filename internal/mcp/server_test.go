package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/quota"
	"github.com/nichuanfang/WebSearch/internal/router"
	"github.com/nichuanfang/WebSearch/internal/search"
	"github.com/nichuanfang/WebSearch/internal/store"
)

// newTestService 组装一个指向假 Exa 端点的服务。
func newTestService(t *testing.T, body string, status int) *search.Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(body))
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	qm := quota.NewManager(s, time.Now)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	e := provider.NewExa("k", srv.Client())
	e.BaseURL = srv.URL
	r := router.New([]router.Target{{
		Provider: e,
		Config: config.ProviderConfig{
			Enabled: true, MonthlyLimit: 100, PeriodModel: config.PeriodCalendarMonth,
		},
		HasKey: true,
	}}, qm, s, log)
	return search.New(r, log, 3)
}

func TestToolRegistered(t *testing.T) {
	svc := newTestService(t, `{"results":[]}`, 200)
	srv := NewServer(svc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx := context.Background()
	session, err := srv.Connect(ctx, &mcp.StdioTransport{}, nil)
	if err != nil {
		// stdio 在测试中不可用，改用内存传输校验工具注册。
		t.Skipf("无法建立 stdio 会话: %v", err)
	}
	defer session.Close()
	_ = ctx
}

// TestToolListAndCall 通过客户端会话验证工具注册与调用。
func TestToolListAndCall(t *testing.T) {
	svc := newTestService(t,
		`{"results":[{"title":"标题","url":"https://e.com","highlights":["摘要"]}],"costDollars":{"total":0.004}}`, 200)
	server := NewServer(svc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 用 in-process 传输连接客户端与服务端。
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("连接服务端失败: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("连接客户端失败: %v", err)
	}
	defer clientSession.Close()

	tools, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("列出工具失败: %v", err)
	}
	if len(tools.Tools) != 1 {
		t.Fatalf("工具数 = %d, 期望 1", len(tools.Tools))
	}
	if tools.Tools[0].Name != "web_search" {
		t.Errorf("工具名 = %q, 期望 web_search", tools.Tools[0].Name)
	}
	// 入参 schema 必须包含 query 且必填。
	schema, err := json.Marshal(tools.Tools[0].InputSchema)
	if err != nil {
		t.Fatalf("序列化 schema 失败: %v", err)
	}
	var sm map[string]any
	json.Unmarshal(schema, &sm)
	props, ok := sm["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema 缺少 properties")
	}
	if _, has := props["query"]; !has {
		t.Error("schema 缺少 query 参数")
	}
	if _, has := props["num_results"]; !has {
		t.Error("schema 缺少 num_results 参数")
	}
	// 只保留 query 与 num_results：provider/topic/search_depth 均已移除。
	for _, p := range []string{"provider", "topic", "search_depth"} {
		if _, has := props[p]; has {
			t.Errorf("schema 不应包含 %s 参数", p)
		}
	}
	required, _ := sm["required"].([]any)
	found := false
	for _, r := range required {
		if r == "query" {
			found = true
		}
	}
	if !found {
		t.Error("query 应为必填参数")
	}

	// 调用工具。
	res, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "web_search",
		Arguments: map[string]any{
			"query":       "golang",
			"num_results": 3,
		},
	})
	if err != nil {
		t.Fatalf("调用工具失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("工具返回错误: %+v", res.Content)
	}
	if res.StructuredContent == nil {
		t.Fatal("缺少结构化输出")
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("结构化输出类型异常: %T", res.StructuredContent)
	}
	if _, has := sc["provider"]; has {
		t.Error("出参不应包含 provider 字段")
	}
	results, ok := sc["results"].([]any)
	if !ok || len(results) != 1 {
		t.Errorf("results 异常: %v", sc["results"])
	}
}

func TestToolReturnsErrorResultNotCrash(t *testing.T) {
	svc := newTestService(t, `{"error":"NO_MORE_CREDITS","tag":"NO_MORE_CREDITS"}`, 402)
	server := NewServer(svc, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("连接服务端失败: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("连接客户端失败: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_search",
		Arguments: map[string]any{"query": "golang"},
	})
	if err != nil {
		t.Fatalf("工具级错误不应导致协议异常: %v", err)
	}
	if !res.IsError {
		t.Error("额度耗尽应返回 IsError 结果")
	}
	if len(res.Content) == 0 {
		t.Error("错误结果应包含说明文本")
	}
}
