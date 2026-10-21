// Parallel Search 适配器（免密钥优先通道）。
//
// 设计要点：
//   - 使用 Parallel 托管的 Streamable HTTP MCP 端点，无需 API key
//   - 匿名层面向个人代理，限流按时间窗口计，不公布具体阈值
//   - 每次固定返回 10 条结果，附带按目标筛选的摘录；按 num_results 自行截断
//
// 能力边界：匿名层没有 SLA，触发限流时服务端返回 HTTP 429 且不带
// Retry-After，只能进入冷却后重试。上层需要准备兜底通道。
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nichuanfang/WebSearch/internal/textcut"
)

const (
	// ParallelName 是 Parallel 平台的标识。
	ParallelName = "parallel"
	// ParallelEndpoint 是 Parallel 托管的免密钥 MCP 端点。
	ParallelEndpoint = "https://search.parallel.ai/mcp"
	// parallelMaxResults 是 Parallel 单次返回的结果上限。
	parallelMaxResults = 10
	// parallelSnippetChars 是单条摘要的字符上限，避免撑爆工具输出。
	parallelSnippetChars = 600
)

// Parallel 是 Parallel Search 适配器。
type Parallel struct {
	Client *http.Client
	// Endpoint 允许测试时替换为假服务器。
	Endpoint string
	// Timeout 是单次搜索的超时上限。
	Timeout time.Duration
	// ProtocolVersion 可选；为空时使用 SDK 默认协议版本。
	ProtocolVersion string

	mu      sync.Mutex
	session *mcp.ClientSession
	// sessionID 是稳定的会话标识，用于免费层限流与日志关联。
	sessionID string
}

// NewParallel 构造 Parallel 适配器。
func NewParallel(client *http.Client) *Parallel {
	return &Parallel{
		Client:    client,
		Endpoint:  ParallelEndpoint,
		Timeout:   25 * time.Second,
		sessionID: newSessionID(),
	}
}

// Name 返回平台标识。
func (p *Parallel) Name() string { return ParallelName }

// CostOf 免密钥通道不消耗付费额度。
func (p *Parallel) CostOf(Query) float64 { return 0 }

// parallelResult 是 Parallel 返回的单条结果。
type parallelResult struct {
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	PublishDate *string  `json:"publish_date"`
	Excerpts    []string `json:"excerpts"`
}

type parallelPayload struct {
	Results []parallelResult `json:"results"`
	Errors  json.RawMessage  `json:"errors"`
}

// Search 执行一次 Parallel 搜索。
func (p *Parallel) Search(ctx context.Context, q Query) (*Response, error) {
	count := q.NumResults
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}
	if count > parallelMaxResults {
		count = parallelMaxResults
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	text, err := p.callTool(callCtx, "web_search", map[string]any{
		"objective":      q.Text,
		"search_queries": []string{q.Text},
		"session_id":     p.sessionID,
	})
	if err != nil {
		return nil, err
	}

	var payload parallelPayload
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, NewError(p.Name(), KindTransient, 0, "解析响应失败: "+err.Error())
	}
	results := make([]Result, 0, len(payload.Results))
	for _, r := range payload.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		date := ""
		if r.PublishDate != nil {
			date = normalizeParallelDate(*r.PublishDate)
		}
		results = append(results, Result{
			Title:   strings.TrimSpace(r.Title),
			URL:     strings.TrimSpace(r.URL),
			Snippet: parallelSnippet(r.Excerpts),
			Date:    date,
		})
		if len(results) >= count {
			break
		}
	}
	return &Response{Provider: p.Name(), Results: results}, nil
}

// callTool 调用 Parallel MCP 工具，必要时重建会话。
func (p *Parallel) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	session, err := p.getSession(ctx)
	if err != nil {
		return "", err
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		p.resetSession()
		return "", classifyParallelError(err)
	}
	if res.IsError {
		return "", NewError(p.Name(), KindTransient, 0, toolText(res))
	}
	text := toolText(res)
	if text == "" {
		return "", NewError(p.Name(), KindTransient, 0, "响应为空")
	}
	return text, nil
}

// getSession 复用已建立的 MCP 会话；失败时重连一次。
func (p *Parallel) getSession(ctx context.Context) (*mcp.ClientSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		return p.session, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "websearch", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             p.Endpoint,
		HTTPClient:           p.Client,
		DisableStandaloneSSE: true,
	}, &mcp.ClientSessionOptions{ProtocolVersion: p.ProtocolVersion})
	if err != nil {
		return nil, classifyParallelError(err)
	}
	p.session = session
	return session, nil
}

func (p *Parallel) resetSession() {
	p.mu.Lock()
	session := p.session
	p.session = nil
	p.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}

func (p *Parallel) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 25 * time.Second
}

// toolText 取出工具结果的文本内容。
func toolText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// classifyParallelError 把 SDK 的文本错误归类。SDK 不透出状态码。
func classifyParallelError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "401"), strings.Contains(msg, "Unauthorized"):
		return NewError(ParallelName, KindAuth, 401, msg)
	case strings.Contains(msg, "Too Many Requests"), strings.Contains(msg, "429"):
		return NewError(ParallelName, KindRateLimited, 429, msg)
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(ParallelName, KindTransient, 0, "请求超时")
	default:
		return NewError(ParallelName, KindTransient, 0, msg)
	}
}

// parallelSnippet 把多段摘录压成单行摘要。
func parallelSnippet(excerpts []string) string {
	joined := textcut.Collapse(strings.Join(excerpts, " "))
	if len(joined) <= parallelSnippetChars {
		return joined
	}
	return textcut.Truncate(joined, parallelSnippetChars) + "…"
}

// normalizeParallelDate 把 ISO8601 时间归一化成日期。
func normalizeParallelDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if len(raw) >= 10 && raw[4] == '-' && raw[7] == '-' {
		return raw[:10]
	}
	return raw
}

// newSessionID 生成稳定的会话标识，用于免费层限流关联。
func newSessionID() string {
	return fmt.Sprintf("websearch-%d", time.Now().UnixNano())
}
