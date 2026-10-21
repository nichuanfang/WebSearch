// Exa 平台适配器。
//
// 计费要点（实测校准）：
//   - type=lite 且 numResults<=10 时单次 $0.004
//   - 超出 10 条的部分按 $1/1k 另计（实测 numResults=15 为 $0.009）
//   - contents.summary 是按页额外计费的 LLM 项，绝不请求
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nichuanfang/WebSearch/internal/textcut"
)

// exaMaxResults 是 Exa 单次请求返回条数上限，超出部分按 $1/1k 另计。
const exaMaxResults = 10

// Exa 是 Exa 搜索适配器。
type Exa struct {
	APIKey string
	Client *http.Client
	// BaseURL 允许测试时替换为假服务器。
	BaseURL string
}

// NewExa 构造 Exa 适配器。
func NewExa(apiKey string, client *http.Client) *Exa {
	return &Exa{APIKey: apiKey, Client: client, BaseURL: "https://api.exa.ai"}
}

// Name 返回平台标识。
func (e *Exa) Name() string { return "exa" }

// CostOf Exa 单次固定消耗 1 credit。
func (e *Exa) CostOf(Query) float64 { return 1 }

type exaRequest struct {
	Query      string      `json:"query"`
	NumResults int         `json:"numResults"`
	Type       string      `json:"type"`
	Contents   exaContents `json:"contents"`
}

type exaContents struct {
	Highlights bool `json:"highlights"`
}

type exaResponse struct {
	RequestID string `json:"requestId"`
	Results   []struct {
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		Published  string   `json:"publishedDate"`
		Highlights []string `json:"highlights"`
		Summary    string   `json:"summary"`
	} `json:"results"`
	CostDollars struct {
		Total  float64 `json:"total"`
		Search struct {
			Neural float64 `json:"neural"`
		} `json:"search"`
	} `json:"costDollars"`
	RequestIDAlt string `json:"request_id"`
}

// Search 执行一次 Exa 搜索。
func (e *Exa) Search(ctx context.Context, q Query) (*Response, error) {
	num := q.NumResults
	if num <= 0 {
		num = 5
	}
	if num > exaMaxResults {
		// 超出 10 条按 $1/1k 另计，会侵蚀免费额度，强制截断。
		num = exaMaxResults
	}
	body := exaRequest{
		Query:      q.Text,
		NumResults: num,
		Type:       "lite",
		Contents:   exaContents{Highlights: true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, NewError(e.Name(), KindInvalidRequest, 0, "构造请求失败: "+err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/search", bytes.NewReader(raw))
	if err != nil {
		return nil, NewError(e.Name(), KindInvalidRequest, 0, err.Error())
	}
	req.Header.Set("x-api-key", e.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, NewError(e.Name(), KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewError(e.Name(), KindTransient, 0, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, e.classify(resp.StatusCode, payload)
	}
	var parsed exaResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, NewError(e.Name(), KindTransient, resp.StatusCode, "解析响应失败: "+err.Error())
	}
	results := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		results = append(results, Result{
			Title:   textcut.Truncate(r.Title, maxResultFieldChars),
			URL:     r.URL,
			Snippet: textcut.Truncate(textcut.Collapse(strings.Join(r.Highlights, " … ")), maxSnippetChars),
			Date:    r.Published,
		})
	}
	return &Response{
		Provider:  e.Name(),
		Results:   results,
		CostUnits: 1,
		CostUSD:   parsed.CostDollars.Total,
	}, nil
}

// classify 把非 200 响应映射为带分类的错误。
func (e *Exa) classify(status int, payload []byte) error {
	msg := strings.TrimSpace(string(payload))
	var env struct {
		Error string `json:"error"`
		Tag   string `json:"tag"`
	}
	if json.Unmarshal(payload, &env) == nil && env.Error != "" {
		msg = env.Error
		if env.Tag != "" {
			msg = fmt.Sprintf("%s (%s)", env.Error, env.Tag)
		}
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return NewError(e.Name(), ErrorKindForStatus(status), status, msg)
}
