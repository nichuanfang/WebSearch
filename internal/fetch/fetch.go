// Package fetch 实现纯本地的网页抓取与正文提取。
//
// 设计取舍：
//   - 不引入无头浏览器：单二进制分发，不拖 Chromium
//   - 不消耗搜索平台额度：抓取与搜索的额度完全隔离
//   - 依赖仅 x/net/html：解析 HTML 并保留标题/段落结构
//
// 能力边界：能抓到服务端渲染的正文。对 JS 渲染或强反爬（Cloudflare 挑战页、
// 登录墙）的页面会如实报错，不做静默降级。
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// 默认限制。
const (
	// DefaultMaxChars 是返回正文的默认字符上限。
	DefaultMaxChars = 20000
	// MaxAllowedChars 是允许的最大字符上限。
	MaxAllowedChars = 200000
	// maxBodyBytes 是下载的 HTML 最大字节数。
	maxBodyBytes = 5 << 20
	// defaultTimeout 是单个请求的默认超时。
	defaultTimeout = 20 * time.Second
)

// userAgent 伪装成常见桌面浏览器。部分站点会拒绝空 UA 或 Go 默认 UA。
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// ErrUnsupportedScheme 表示 URL 协议不受支持。
var ErrUnsupportedScheme = errors.New("仅支持 http/https 协议的 URL")

// ErrNotHTML 表示目标不是 HTML 文档。
var ErrNotHTML = errors.New("目标不是 HTML 文档")

// ErrEmptyContent 表示页面没有可提取的正文。
var ErrEmptyContent = errors.New("页面中没有可提取的正文内容")

// Fetcher 抓取网页并提取正文。
type Fetcher struct {
	Client *http.Client
	// MaxChars 是返回正文的字符上限，0 表示用 DefaultMaxChars。
	MaxChars int
}

// New 构造 Fetcher。
func New(client *http.Client, maxChars int) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	return &Fetcher{Client: client, MaxChars: maxChars}
}

// Result 是一次抓取的结果。
type Result struct {
	// URL 是最终 URL（跟随重定向后）。
	URL string
	// Title 是页面标题。
	Title string
	// Content 是提取出的正文，Markdown 风格。
	Content string
	// ContentType 是响应的 Content-Type。
	ContentType string
	// Truncated 表示正文是否被截断。
	Truncated bool
}

// Fetch 抓取并提取指定 URL 的正文。
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip")

	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("目标返回 HTTP %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "html") &&
		!strings.Contains(strings.ToLower(contentType), "xml") {
		return nil, fmt.Errorf("%w: %s", ErrNotHTML, contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	// gzip 解压：Go 的 Transport 默认不会自动解压我们手动声明的 Accept-Encoding。
	body, err = maybeDecompress(body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return nil, fmt.Errorf("解压响应失败: %w", err)
	}

	title, content := Extract(string(body))
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("%w（标题: %q）", ErrEmptyContent, title)
	}

	limit := f.MaxChars
	if limit <= 0 {
		limit = DefaultMaxChars
	}
	truncated := false
	if len(content) > limit {
		content = content[:limit]
		truncated = true
	}
	return &Result{
		URL:         resp.Request.URL.String(),
		Title:       title,
		Content:     content,
		ContentType: contentType,
		Truncated:   truncated,
	}, nil
}

// skipTags 是提取正文时跳过的标签。
var skipTags = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Svg: true, atom.Head: true, atom.Nav: true, atom.Footer: true,
	atom.Aside: true, atom.Form: true, atom.Iframe: true, atom.Template: true,
	atom.Canvas: true, atom.Object: true, atom.Embed: true,
}

// Extract 从 HTML 文档中提取标题与 Markdown 风格的正文。
// 保留标题层级与段落结构，剔除导航、脚本等噪音。
func Extract(doc string) (title string, content string) {
	node, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return "", ""
	}
	var sb strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inSkip bool) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Title && title == "" {
			var t strings.Builder
			collectText(n, &t)
			title = strings.TrimSpace(t.String())
		}
		skip := inSkip
		if n.Type == html.ElementNode {
			if skipTags[n.DataAtom] {
				skip = true
			} else if !skip {
				switch n.DataAtom {
				case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
					sb.WriteString("\n\n" + strings.Repeat("#", headingLevel(n.Data)) + " ")
				case atom.P, atom.Li, atom.Br, atom.Tr, atom.Blockquote, atom.Pre, atom.Dd, atom.Dt:
					sb.WriteString("\n")
				}
			}
		}
		if n.Type == html.TextNode && !skip {
			if t := strings.TrimSpace(n.Data); t != "" {
				sb.WriteString(t + " ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, skip)
		}
	}
	walk(node, false)
	return title, normalize(sb.String())
}

// headingLevel 从 "h3" 这类标签名推出 Markdown 标题级别。
func headingLevel(tag string) int {
	if len(tag) < 2 || tag[0] != 'h' {
		return 2
	}
	level := int(tag[1] - '0')
	if level < 1 {
		return 1
	}
	if level > 6 {
		return 6
	}
	return level
}

// normalize 压缩多余空白：行内折叠空格，丢弃空行。
func normalize(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// collectText 递归收集节点下的纯文本。
func collectText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, sb)
	}
}
