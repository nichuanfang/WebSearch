// TinyFish Fetch 远程抓取通道。
//
// 设计要点：
//   - 官方 Fetch API，需 API key（X-API-Key 头）
//   - 免费额度 1,000 URL/天
//   - 会按需渲染 JavaScript 页面，返回干净的 Markdown
//
// 能力边界：依赖外部服务与网络；不可用时由 Manager 回落到 Jina 与本地。
package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nichuanfang/WebSearch/internal/provider"
)

// TinyFishFetcher 通过 TinyFish Fetch API 抓取网页。
type TinyFishFetcher struct {
	Client *http.Client
	APIKey string
	// BaseURL 允许测试时替换为假服务器。
	BaseURL string
}

// NewTinyFishFetcher 构造 TinyFish 抓取通道。
func NewTinyFishFetcher(apiKey string, client *http.Client) *TinyFishFetcher {
	return &TinyFishFetcher{APIKey: apiKey, Client: client, BaseURL: provider.TinyFishFetchEndpoint}
}

// Name 返回通道标识。
func (t *TinyFishFetcher) Name() string { return provider.TinyFishName }

type tinyFishFetchResponse struct {
	Results []struct {
		URL         string `json:"url"`
		FinalURL    string `json:"final_url"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Text        string `json:"text"`
	} `json:"results"`
	Errors []struct {
		URL     string `json:"url"`
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"errors"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Fetch 抓取指定 URL 并返回 Markdown 正文。
func (t *TinyFishFetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedScheme, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL 缺少主机名")
	}

	body, err := json.Marshal(map[string]any{"urls": []string{u.String()}, "format": "markdown"})
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	endpoint := t.BaseURL
	if endpoint == "" {
		endpoint = provider.TinyFishFetchEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("X-API-Key", t.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, provider.NewError(provider.TinyFishName, provider.KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, provider.NewError(provider.TinyFishName, provider.KindTransient, resp.StatusCode, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyTinyFishFetch(resp.StatusCode, payload, resp.Header)
	}

	var parsed tinyFishFetchResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, provider.NewError(provider.TinyFishName, provider.KindTransient, resp.StatusCode, "解析响应失败: "+err.Error())
	}
	if parsed.Error != nil {
		return nil, provider.NewError(provider.TinyFishName, kindFromCode(parsed.Error.Code), 0, parsed.Error.Message)
	}
	if len(parsed.Results) == 0 {
		msg := "目标未返回内容"
		if len(parsed.Errors) > 0 && parsed.Errors[0].Message != "" {
			msg = parsed.Errors[0].Message
		}
		return nil, fmt.Errorf("%w（%s）", ErrEmptyContent, msg)
	}

	r := parsed.Results[0]
	if strings.TrimSpace(r.Text) == "" {
		return nil, fmt.Errorf("%w（标题: %q）", ErrEmptyContent, r.Title)
	}
	finalURL := r.FinalURL
	if finalURL == "" {
		finalURL = r.URL
	}
	if finalURL == "" {
		finalURL = u.String()
	}
	return &Result{
		URL:         finalURL,
		Title:       strings.TrimSpace(r.Title),
		Content:     strings.TrimSpace(r.Text),
		ContentType: "text/markdown",
	}, nil
}

func classifyTinyFishFetch(status int, payload []byte, hdr http.Header) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(truncateText(string(payload), 300))
	code := ""
	if json.Unmarshal(payload, &env) == nil && env.Error.Message != "" {
		code = env.Error.Code
		msg = env.Error.Message
	}
	kind := provider.ErrorKindForStatus(status)
	if code != "" {
		kind = kindFromCode(code)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		kind = provider.KindAuth
	}
	return provider.NewError(provider.TinyFishName, kind, status, msg)
}

func kindFromCode(code string) provider.ErrorKind {
	upper := strings.ToUpper(code)
	switch {
	case upper == "INSUFFICIENT_CREDITS", strings.Contains(upper, "RATE"):
		return provider.KindRateLimited
	case upper == "MISSING_API_KEY", upper == "INVALID_API_KEY", upper == "UNAUTHORIZED":
		return provider.KindAuth
	default:
		return provider.KindTransient
	}
}
