package mcp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nichuanfang/WebSearch/internal/fetch"
	"github.com/nichuanfang/WebSearch/internal/textcut"
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

// 抓取通道，便于测试替换。
type pageFetcher interface {
	Fetch(ctx context.Context, rawURL string) (*fetch.Result, error)
}

// registerWebFetch 注册 web_fetch 工具。
func registerWebFetch(srv *mcp.Server, fetcher pageFetcher, log *slog.Logger) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "web_fetch",
		Description: "抓取指定网页并返回正文内容。当需要阅读搜索结果的原文、查看具体文档或文章内容时使用。" +
			"返回标题、最终 URL 与 Markdown 风格的正文。优先使用远程渲染通道以支持 JavaScript 页面，" +
			"远程不可用时自动回落到本地抓取。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in WebFetchInput) (*mcp.CallToolResult, WebFetchOutput, error) {
		res, err := fetcher.Fetch(ctx, in.URL)
		if err != nil {
			// 工具级错误以 IsError 返回，模型可据此换用其他方式。
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "抓取失败：" + err.Error()}},
			}, WebFetchOutput{}, nil
		}
		content := res.Content
		truncated := res.Truncated
		limit := textcut.Clamp(in.MaxChars, fetch.DefaultMaxChars, fetch.MaxAllowedChars)
		if len(content) > limit {
			content = textcut.Truncate(content, limit)
			truncated = true
		}
		log.Info("抓取完成",
			"url", res.URL,
			"title", res.Title,
			"chars", len(content),
			"truncated", truncated)
		return nil, WebFetchOutput{
			Title:     res.Title,
			URL:       res.URL,
			Content:   content,
			Truncated: truncated,
		}, nil
	})
}

var _ = fmt.Sprintf
