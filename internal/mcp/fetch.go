package mcp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nichuanfang/WebSearch/internal/fetch"
)

// WebFetchInput 是 web_fetch 工具的入参。
type WebFetchInput struct {
	URL string `json:"url" jsonschema:"要抓取的网页 URL，必须是 http 或 https 开头"`
	// MaxChars 限制返回正文的字符数，0 表示使用默认值。
	MaxChars int `json:"max_chars,omitempty" jsonschema:"返回正文的字符上限，1-200000，默认 20000"`
}

// WebFetchOutput 是 web_fetch 工具的出参。
type WebFetchOutput struct {
	Title   string `json:"title" jsonschema:"页面标题"`
	URL     string `json:"url" jsonschema:"最终 URL（跟随重定向后）"`
	Content string `json:"content" jsonschema:"页面正文，Markdown 风格"`
	// Truncated 表示正文是否因长度限制被截断。
	Truncated bool `json:"truncated" jsonschema:"正文是否被截断"`
}

// registerWebFetch 注册 web_fetch 工具。
func registerWebFetch(srv *mcp.Server, fetcher *fetch.Fetcher, log *slog.Logger) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "web_fetch",
		Description: "抓取指定网页并返回正文内容。当需要阅读搜索结果的原文、查看具体文档或文章内容时使用。" +
			"返回标题、最终 URL 与 Markdown 风格的正文。仅支持服务端渲染的页面，依赖 JavaScript 渲染或有强反爬的站点会失败。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in WebFetchInput) (*mcp.CallToolResult, WebFetchOutput, error) {
		f := fetcher
		if in.MaxChars > 0 {
			// 复制一份以免影响其他调用的上限设置。
			cp := *fetcher
			cp.MaxChars = in.MaxChars
			f = &cp
		}
		res, err := f.Fetch(ctx, in.URL)
		if err != nil {
			// 工具级错误以 IsError 返回，模型可据此换用其他方式。
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "抓取失败：" + err.Error()}},
			}, WebFetchOutput{}, nil
		}
		log.Info("抓取完成",
			"url", res.URL,
			"title", res.Title,
			"chars", len(res.Content),
			"truncated", res.Truncated)
		return nil, WebFetchOutput{
			Title:     res.Title,
			URL:       res.URL,
			Content:   res.Content,
			Truncated: res.Truncated,
		}, nil
	})
}

var _ = fmt.Sprintf
