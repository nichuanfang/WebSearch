// Tavily 平台适配器。
//
// 计费要点（实测校准）：
//   - basic/fast/ultra-fast 消耗 1 credit，advanced 消耗 2 credits
//   - /usage 接口实测不实时（多次搜索后 plan_usage 恒为 0），不能作为真值
//   - 额度耗尽返回 432（计划额度超限）或 433（PayGo 超限）
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Tavily 是 Tavily Search 适配器。
type Tavily struct {
	APIKey string
	Client *http.Client
	// BaseURL 允许测试时替换为假服务器。
	BaseURL string
}

// NewTavily 构造 Tavily 适配器。
func NewTavily(apiKey string, client *http.Client) *Tavily {
	return &Tavily{APIKey: apiKey, Client: client, BaseURL: "https://api.tavily.com"}
}

// Name 返回平台标识。
func (t *Tavily) Name() string { return "tavily" }

// CostOf Tavily 固定使用 basic 深度，消耗 1 credit。
func (t *Tavily) CostOf(Query) float64 {
	return 1
}

type tavilyRequest struct {
	Query       string `json:"query"`
	MaxResults  int    `json:"max_results"`
	SearchDepth string `json:"search_depth"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string  `json:"title"`
		URL     string  `json:"url"`
		Content string  `json:"content"`
		Score   float64 `json:"score"`
	} `json:"results"`
}

// Search 执行一次 Tavily 搜索。
func (t *Tavily) Search(ctx context.Context, q Query) (*Response, error) {
	max := q.NumResults
	if max <= 0 {
		max = 5
	}
	if max > 20 {
		max = 20
	}
	// 固定使用 basic 深度：advanced 会消耗 2 倍额度，收益有限。
	const depth = "basic"
	body := tavilyRequest{
		Query:       q.Text,
		MaxResults:  max,
		SearchDepth: depth,
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, NewError(t.Name(), KindInvalidRequest, 0, "构造请求失败: "+err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/search", bytes.NewReader(raw))
	if err != nil {
		return nil, NewError(t.Name(), KindInvalidRequest, 0, err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, NewError(t.Name(), KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewError(t.Name(), KindTransient, 0, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyWithHeaders(t.Name(), resp, payload)
	}
	var parsed tavilyResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, NewError(t.Name(), KindTransient, resp.StatusCode, "解析响应失败: "+err.Error())
	}
	results := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
			Score:   r.Score,
		})
	}
	return &Response{
		Provider:  t.Name(),
		Results:   results,
		CostUnits: t.CostOf(q),
	}, nil
}

// UsageSnapshot 是 /usage 接口的快照。实测该接口上报不实时，仅作参考。
type UsageSnapshot struct {
	AccountPlan   string
	PlanUsage     float64
	PlanLimit     float64
	SearchUsage   float64
	PaygoUsage    float64
	PaygoLimit    float64
	HasPaygoLimit bool
}

// Usage 查询账号用量。上报不实时，仅用于 doctor / quota 命令展示参考值。
func (t *Tavily) Usage(ctx context.Context) (*UsageSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+"/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, NewError(t.Name(), ErrorKindForStatus(resp.StatusCode), resp.StatusCode,
			truncate(strings.TrimSpace(string(payload)), 200))
	}
	var parsed struct {
		Account struct {
			CurrentPlan string   `json:"current_plan"`
			PlanUsage   float64  `json:"plan_usage"`
			PlanLimit   float64  `json:"plan_limit"`
			SearchUsage float64  `json:"search_usage"`
			PaygoUsage  float64  `json:"paygo_usage"`
			PaygoLimit  *float64 `json:"paygo_limit"`
		} `json:"account"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, err
	}
	snap := &UsageSnapshot{
		AccountPlan: parsed.Account.CurrentPlan,
		PlanUsage:   parsed.Account.PlanUsage,
		PlanLimit:   parsed.Account.PlanLimit,
		SearchUsage: parsed.Account.SearchUsage,
		PaygoUsage:  parsed.Account.PaygoUsage,
	}
	if parsed.Account.PaygoLimit != nil {
		snap.HasPaygoLimit = true
		snap.PaygoLimit = *parsed.Account.PaygoLimit
	}
	return snap, nil
}
