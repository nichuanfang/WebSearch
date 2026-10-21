// TinyFish 搜索适配器。
//
// 设计要点：
//   - 官方 Search API，需 API key（X-API-Key 头）
//   - 免费额度 12,000 次/天，超出后需要账户余额
//   - 返回结构化结果（title/snippet/url/date），每次约 9-10 条
//
// 能力边界：中文语义长查询弱于 Parallel，英文与技术类查询较强且快；
// 因此放在 Parallel 之后作为第二优先级。
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

const (
	// TinyFishName 是 TinyFish 通道的标识。
	TinyFishName = "tinyfish"
	// TinyFishSearchEndpoint 是搜索 API 入口。
	TinyFishSearchEndpoint = "https://api.search.tinyfish.ai"
	// TinyFishFetchEndpoint 是抓取 API 入口。
	TinyFishFetchEndpoint = "https://api.fetch.tinyfish.ai"
	// tinyFishMaxBytes 是响应读取上限。
	tinyFishMaxBytes = 4 << 20
)

// TinyFish 是 TinyFish 搜索适配器。
type TinyFish struct {
	APIKey string
	Client *http.Client
	// BaseURL 允许测试时替换为假服务器。
	BaseURL string
}

// NewTinyFish 构造 TinyFish 搜索适配器。
func NewTinyFish(apiKey string, client *http.Client) *TinyFish {
	return &TinyFish{APIKey: apiKey, Client: client, BaseURL: TinyFishSearchEndpoint}
}

// Name 返回平台标识。
func (t *TinyFish) Name() string { return TinyFishName }

// CostOf 免密钥通道不消耗付费额度。
func (t *TinyFish) CostOf(Query) float64 { return 0 }

type tinyFishResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
		Date    string `json:"date"`
	} `json:"results"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Search 执行一次 TinyFish 搜索。
func (t *TinyFish) Search(ctx context.Context, q Query) (*Response, error) {
	count := q.NumResults
	if count <= 0 {
		count = 5
	}
	if count > 20 {
		count = 20
	}

	endpoint := t.BaseURL
	if endpoint == "" {
		endpoint = TinyFishSearchEndpoint
	}
	form := url.Values{}
	form.Set("query", q.Text)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+form.Encode(), nil)
	if err != nil {
		return nil, NewError(t.Name(), KindInvalidRequest, 0, err.Error())
	}
	req.Header.Set("X-API-Key", t.APIKey)
	req.Header.Set("Accept", "application/json")

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, NewError(t.Name(), KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, tinyFishMaxBytes))
	if err != nil {
		return nil, NewError(t.Name(), KindTransient, resp.StatusCode, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyTinyFishError(resp.StatusCode, payload, resp.Header)
	}

	var parsed tinyFishResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, NewError(t.Name(), KindTransient, resp.StatusCode, "解析响应失败: "+err.Error())
	}
	if parsed.Error != nil {
		return nil, classifyTinyFishCode(parsed.Error.Code, parsed.Error.Message)
	}

	results := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		results = append(results, Result{
			Title:   textcut.Truncate(strings.TrimSpace(r.Title), maxResultFieldChars),
			URL:     strings.TrimSpace(r.URL),
			Snippet: textcut.Truncate(textcut.Collapse(r.Snippet), maxSnippetChars),
			Date:    normalizeTinyFishDate(r.Date),
		})
		if len(results) >= count {
			break
		}
	}
	return &Response{Provider: t.Name(), Results: results}, nil
}

// classifyTinyFishError 把 HTTP 错误映射到统一分类。
func classifyTinyFishError(status int, payload []byte, hdr http.Header) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(truncate(string(payload), 300))
	code := ""
	if json.Unmarshal(payload, &env) == nil && env.Error.Message != "" {
		code = env.Error.Code
		msg = env.Error.Message
	}
	// 401/403 且是密钥类错误，归为鉴权失败；余额/限流类归为限流。
	kind := ErrorKindForStatus(status)
	switch {
	case code == "MISSING_API_KEY", code == "INVALID_API_KEY", code == "UNAUTHORIZED":
		kind = KindAuth
	case code == "INSUFFICIENT_CREDITS", code == "RATE_LIMITED", strings.Contains(strings.ToUpper(code), "RATE"):
		kind = KindRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = KindAuth
	}
	err := NewError(TinyFishName, kind, status, msg)
	if status == http.StatusTooManyRequests {
		if ra := hdr.Get("Retry-After"); ra != "" {
			if secs, convErr := strconv.Atoi(strings.TrimSpace(ra)); convErr == nil && secs > 0 {
				err.RetryAfter = time.Duration(secs) * time.Second
			}
		}
	}
	return err
}

// classifyTinyFishCode 处理 HTTP 200 但业务层返回错误的情况。
func classifyTinyFishCode(code, message string) error {
	switch {
	case code == "INSUFFICIENT_CREDITS", strings.Contains(strings.ToUpper(code), "RATE"):
		return NewError(TinyFishName, KindRateLimited, 0, message)
	case code == "MISSING_API_KEY", code == "INVALID_API_KEY", code == "UNAUTHORIZED":
		return NewError(TinyFishName, KindAuth, 0, message)
	default:
		return NewError(TinyFishName, KindTransient, 0, message)
	}
}

// normalizeTinyFishDate 把 "Oct 18, 2023" 或 "3 months ago" 归一化。
// 相对时间无法精确换算，保留原文，避免给出错误日期。
func normalizeTinyFishDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	layouts := []string{"Jan 2, 2006", "Jan 02, 2006", "2006-01-02", time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return raw
}
