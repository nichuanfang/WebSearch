package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// legacyVersion 让测试走经典 initialize 握手，避开 server/discover 协商。
const legacyVersion = "2025-06-18"

// fakeMCPServer 模拟 Parallel 的 Streamable HTTP MCP 端点。
type fakeMCPServer struct {
	calls     atomic.Int32
	lastArgs  map[string]any
	results   string
	status    int
	callError string
}

func (f *fakeMCPServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		method, _ := body["method"].(string)
		if method == "server/discover" {
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": body["id"],
				"result": map[string]any{
					"resultType":        "complete",
					"supportedVersions": []string{"2026-07-28"},
					"capabilities":      map[string]any{"tools": map[string]any{}},
				},
			})
			return
		}
		if method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "test-session")
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": body["id"],
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
				},
			})
			return
		}
		if method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if method == "tools/list" {
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": body["id"],
				"result": map[string]any{
					"tools": []any{map[string]any{
						"name":        "web_search",
						"description": "fake",
						"inputSchema": map[string]any{"type": "object"},
					}},
				},
			})
			return
		}
		if method != "tools/call" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		f.calls.Add(1)
		if f.status != 0 && f.status != http.StatusOK {
			w.WriteHeader(f.status)
			w.Write([]byte("Too Many Requests"))
			return
		}
		params, _ := body["params"].(map[string]any)
		f.lastArgs, _ = params["arguments"].(map[string]any)

		payload := f.results
		if f.callError != "" {
			payload = f.callError
		}
		result := map[string]any{
			"content": []any{map[string]any{"type": "text", "text": payload}},
		}
		// 只有合法 JSON 才放进 structuredContent，避免编码失败。
		var probe any
		if f.callError == "" && json.Unmarshal([]byte(payload), &probe) == nil {
			result["structuredContent"] = probe
		}
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": body["id"],
			"result": result,
		})
	}
}

const parallelPayloadSample = `{
  "search_id": "search_x",
  "results": [
    {"url": "https://go.dev/doc/tutorial/generics", "title": "Go 泛型教程",
     "publish_date": "2026-10-20T10:00:00Z",
     "excerpts": ["This tutorial introduces the basics of generics.", "第二步"]},
    {"url": "https://gobyexample.com/generics", "title": "Go by Example",
     "publish_date": null, "excerpts": ["Generics example"]},
    {"url": "https://example.com/3", "title": "第三条", "excerpts": ["三"]}
  ]
}`

func TestParallelSearchParsesResults(t *testing.T) {
	fake := &fakeMCPServer{results: parallelPayloadSample}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	p := NewParallel(srv.Client())
	p.ProtocolVersion = legacyVersion
	p.Endpoint = srv.URL
	resp, err := p.Search(context.Background(), Query{Text: "Go 泛型 使用教程", NumResults: 2})
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("结果数 = %d, 期望按 num_results 截断到 2", len(resp.Results))
	}
	first := resp.Results[0]
	if first.Title != "Go 泛型教程" || first.URL != "https://go.dev/doc/tutorial/generics" {
		t.Errorf("首条结果异常: %+v", first)
	}
	if first.Date != "2026-10-20" {
		t.Errorf("日期 = %q, 期望归一化为 2026-10-20", first.Date)
	}
	if first.Snippet != "This tutorial introduces the basics of generics. 第二步" {
		t.Errorf("摘要 = %q", first.Snippet)
	}
	if resp.Results[1].Date != "" {
		t.Errorf("空日期应保持为空, 实际 %q", resp.Results[1].Date)
	}
	if resp.CostUnits != 0 {
		t.Errorf("免密钥通道不应消耗额度, 实际 %v", resp.CostUnits)
	}
}

func TestParallelSendsObjectiveAndQueries(t *testing.T) {
	fake := &fakeMCPServer{results: parallelPayloadSample}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	p := NewParallel(srv.Client())
	p.ProtocolVersion = legacyVersion
	p.Endpoint = srv.URL
	if _, err := p.Search(context.Background(), Query{Text: "飞书多维表格 定价", NumResults: 3}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if fake.lastArgs["objective"] != "飞书多维表格 定价" {
		t.Errorf("objective = %v", fake.lastArgs["objective"])
	}
	queries, _ := fake.lastArgs["search_queries"].([]any)
	if len(queries) != 1 || queries[0] != "飞书多维表格 定价" {
		t.Errorf("search_queries = %v", fake.lastArgs["search_queries"])
	}
	if fake.lastArgs["session_id"] == "" {
		t.Error("应传递稳定的 session_id")
	}
}

func TestParallelRateLimitIsClassified(t *testing.T) {
	fake := &fakeMCPServer{status: http.StatusTooManyRequests}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	p := NewParallel(srv.Client())
	p.ProtocolVersion = legacyVersion
	p.Endpoint = srv.URL
	_, err := p.Search(context.Background(), Query{Text: "Go generics"})
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	pe := AsError(err)
	if pe == nil || pe.Kind != KindRateLimited {
		t.Fatalf("错误分类 = %v, 期望 rate_limited", err)
	}
}

func TestParallelToolErrorIsTransient(t *testing.T) {
	fake := &fakeMCPServer{callError: "internal error"}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	p := NewParallel(srv.Client())
	p.ProtocolVersion = legacyVersion
	p.Endpoint = srv.URL
	_, err := p.Search(context.Background(), Query{Text: "Go generics"})
	if err == nil {
		t.Fatal("应返回错误")
	}
	pe := AsError(err)
	if pe == nil || pe.Kind != KindTransient {
		t.Fatalf("错误分类 = %v, 期望 transient", err)
	}
}

func TestParallelMalformedPayload(t *testing.T) {
	fake := &fakeMCPServer{results: "not json"}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	p := NewParallel(srv.Client())
	p.ProtocolVersion = legacyVersion
	p.Endpoint = srv.URL
	_, err := p.Search(context.Background(), Query{Text: "Go generics"})
	if err == nil {
		t.Fatal("非法响应应返回错误")
	}
	if !strings.Contains(err.Error(), "解析响应失败") {
		t.Errorf("错误 = %v, 应说明解析失败", err)
	}
}

func TestParallelSnippetTruncatesToLimit(t *testing.T) {
	long := strings.Repeat("字", 1000)
	got := parallelSnippet([]string{long})
	if len([]rune(got)) > parallelSnippetChars+1 {
		t.Errorf("摘要长度 = %d 字符, 应被限制", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("截断后应带省略号")
	}
}
