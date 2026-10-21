// Brave Search 平台适配器。
//
// 额度要点（实测校准）：
//   - 响应头 x-ratelimit-* 为逗号分隔的多窗口格式，第二个值是月度窗口
//   - 实测 x-ratelimit-limit: 50, 0 —— 第二值为 0 表示月度不限量
//   - x-ratelimit-reset: 1, 1021268 —— 第二个值是距月度重置的秒数
//   - 官方明确只有成功请求才计入额度
package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nichuanfang/WebSearch/internal/textcut"
)

// Brave 是 Brave Search 适配器。
type Brave struct {
	APIKey string
	Client *http.Client
	// BaseURL 允许测试时替换为假服务器。
	BaseURL string
}

// NewBrave 构造 Brave 适配器。
func NewBrave(apiKey string, client *http.Client) *Brave {
	return &Brave{APIKey: apiKey, Client: client, BaseURL: "https://api.search.brave.com"}
}

// Name 返回平台标识。
func (b *Brave) Name() string { return "brave" }

// CostOf Brave 单次固定消耗 1 credit。
func (b *Brave) CostOf(Query) float64 { return 1 }

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Age         string `json:"age"`
		} `json:"results"`
	} `json:"web"`
}

// Search 执行一次 Brave 搜索。
func (b *Brave) Search(ctx context.Context, q Query) (*Response, error) {
	count := q.NumResults
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}
	endpoint := b.BaseURL + "/res/v1/web/search"
	form := url.Values{}
	form.Set("q", q.Text)
	form.Set("count", strconv.Itoa(count))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+form.Encode(), nil)
	if err != nil {
		return nil, NewError(b.Name(), KindInvalidRequest, 0, err.Error())
	}
	req.Header.Set("X-Subscription-Token", b.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, NewError(b.Name(), KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewError(b.Name(), KindTransient, 0, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyWithHeaders(b.Name(), resp, payload)
	}
	var parsed braveResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, NewError(b.Name(), KindTransient, resp.StatusCode, "解析响应失败: "+err.Error())
	}
	results := make([]Result, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		results = append(results, Result{
			Title:   textcut.Truncate(r.Title, maxResultFieldChars),
			URL:     r.URL,
			Snippet: textcut.Truncate(textcut.Collapse(r.Description), maxSnippetChars),
			Date:    r.Age,
		})
	}
	return &Response{
		Provider:        b.Name(),
		Results:         results,
		CostUnits:       1,
		ServerRemaining: braveServerRemaining(resp),
		ServerLimit:     braveServerLimit(resp),
		ServerResetAt:   braveServerResetAt(resp),
	}, nil
}

// braveWindow 解析逗号分隔的多窗口头的第二个值（月度窗口）。
func braveWindow(h string) (float64, bool) {
	parts := strings.Split(h, ",")
	if len(parts) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// braveServerLimit 返回月度窗口上限，第二值为 0 表示不限量。
func braveServerLimit(resp *http.Response) float64 {
	v, ok := braveWindow(resp.Header.Get("x-ratelimit-limit"))
	if !ok {
		return -1
	}
	return v
}

// braveServerRemaining 返回月度窗口剩余额度。
func braveServerRemaining(resp *http.Response) float64 {
	v, ok := braveWindow(resp.Header.Get("x-ratelimit-remaining"))
	if !ok {
		return -1
	}
	return v
}

// braveServerResetAt 返回月度窗口的下次重置时刻。
func braveServerResetAt(resp *http.Response) time.Time {
	v, ok := braveWindow(resp.Header.Get("x-ratelimit-reset"))
	if !ok || v <= 0 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(v) * time.Second)
}

// classifyWithHeaders 依据状态码与响应头分类错误，429 时读取 Retry-After。
func classifyWithHeaders(name string, resp *http.Response, payload []byte) error {
	kind := ErrorKindForStatus(resp.StatusCode)
	err := NewError(name, kind, resp.StatusCode, strings.TrimSpace(truncate(string(payload), 300)))
	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, convErr := strconv.Atoi(strings.TrimSpace(ra)); convErr == nil && secs > 0 {
				err.RetryAfter = time.Duration(secs) * time.Second
			}
		}
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
