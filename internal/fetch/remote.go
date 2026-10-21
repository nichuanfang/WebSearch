// Jina Reader 远程抓取适配器。
//
// 设计要点：
//   - 无 key 可用，匿名层限流为 20 次/分钟（按出口 IP）
//   - 能渲染依赖 JavaScript 的页面，弥补纯本地抓取的短板
//   - 只依赖标准库，产物仍是单一二进制
//
// 能力边界：需要联网访问 r.jina.ai；匿名层触发 429 后应进入冷却，
// 交由上层回落到本地抓取。
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nichuanfang/WebSearch/internal/provider"
)

const (
	// JinaName 是 Jina Reader 通道的标识。
	JinaName = "jina"
	// JinaEndpoint 是 Jina Reader 的公开入口。
	JinaEndpoint = "https://r.jina.ai"
	// jinaMaxBytes 是远程正文的读取上限。
	jinaMaxBytes = 16 << 20
)

// JinaReader 通过 r.jina.ai 把网页转换成 Markdown。
type JinaReader struct {
	Client *http.Client
	// BaseURL 允许测试时替换为假服务器。
	BaseURL string
	// APIKey 可选；带上后匿名限流会提升到 500 次/分钟。
	APIKey string
}

// NewJinaReader 构造 Jina Reader 适配器。
func NewJinaReader(client *http.Client) *JinaReader {
	return &JinaReader{Client: client, BaseURL: JinaEndpoint}
}

// Name 返回通道标识。
func (j *JinaReader) Name() string { return JinaName }

// Fetch 抓取指定 URL 并返回 Markdown 正文。
func (j *JinaReader) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedScheme, u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("URL 缺少主机名")
	}

	base := j.BaseURL
	if base == "" {
		base = JinaEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	if j.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+j.APIKey)
	}

	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, provider.NewError(JinaName, provider.KindTransient, 0, err.Error())
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, jinaMaxBytes))
	if err != nil {
		return nil, provider.NewError(JinaName, provider.KindTransient, resp.StatusCode, "读取响应失败: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		kind := provider.ErrorKindForStatus(resp.StatusCode)
		return nil, provider.NewError(JinaName, kind, resp.StatusCode,
			strings.TrimSpace(truncateText(string(body), 300)))
	}

	title, content := splitJinaMarkdown(string(body))
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("%w（标题: %q）", ErrEmptyContent, title)
	}
	return &Result{
		URL:         u.String(),
		Title:       title,
		Content:     content,
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// splitJinaMarkdown 拆出 Jina 返回的标题与正文。
// 返回格式为 "Title: xxx\n\nURL Source: xxx\n\nMarkdown Content:\n<body>"。
func splitJinaMarkdown(raw string) (title, content string) {
	body := raw
	if idx := strings.Index(body, "Markdown Content:"); idx >= 0 {
		body = strings.TrimSpace(body[idx+len("Markdown Content:"):])
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "Title: ") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "Title: "))
			break
		}
	}
	return title, body
}

// truncateText 截断文本用于错误信息。
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
