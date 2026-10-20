// Package mcp 封装 MCP 服务器与工具注册。
package mcp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/search"
)

// WebSearchInput 是 web_search 工具的入参。
type WebSearchInput struct {
	Query      string `json:"query" jsonschema:"搜索关键词，必填"`
	NumResults int    `json:"num_results,omitempty" jsonschema:"返回结果条数，1-20，默认 5"`
}

// WebSearchOutput 是 web_search 工具的出参。
type WebSearchOutput struct {
	Count   int               `json:"count" jsonschema:"返回条数"`
	Results []provider.Result `json:"results" jsonschema:"搜索结果列表"`
}

// NewServer 构造注册好 web_search 工具的 MCP 服务器。
func NewServer(svc *search.Service, log *slog.Logger) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "websearch",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "web_search",
		Description: "搜索互联网以获取最新信息。当需要回答涉及事实、文档、新闻或任何可能超出你知识范围的问题时使用。返回结果列表，包含标题、链接、摘要、评分与日期，便于引用来源。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in WebSearchInput) (*mcp.CallToolResult, WebSearchOutput, error) {
		out, err := svc.Execute(ctx, search.Request{
			Query:      in.Query,
			NumResults: in.NumResults,
		})
		if err != nil {
			// 工具级错误必须以 IsError 返回，让模型能读懂并自我纠正。
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "搜索失败：" + err.Error()}},
			}, WebSearchOutput{}, nil
		}
		results := out.Results
		if results == nil {
			results = []provider.Result{}
		}
		return nil, WebSearchOutput{
			Count:   out.Count,
			Results: results,
		}, nil
	})
	return srv
}

// Serve 在 stdio 上运行 MCP 服务器。
func Serve(ctx context.Context, srv *mcp.Server, log *slog.Logger) error {
	log.Info("MCP 服务启动", "transport", "stdio")
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("MCP 服务异常退出: %w", err)
	}
	return nil
}
