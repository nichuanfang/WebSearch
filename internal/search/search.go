// Package search 承载 web_search 工具的业务逻辑。
package search

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/router"
)

// Service 是搜索服务。
type Service struct {
	router *router.Router
	log    *slog.Logger
	max    int
}

// New 构造 Service。
func New(r *router.Router, log *slog.Logger, maxAttempts int) *Service {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return &Service{router: r, log: log, max: maxAttempts}
}

// Request 是一次搜索请求。
type Request struct {
	Query      string
	NumResults int
}

// Response 是归一化后的搜索响应。
type Response struct {
	Count   int
	Results []provider.Result
}

// Execute 执行一次搜索。
func (s *Service) Execute(ctx context.Context, req Request) (*Response, error) {
	text := strings.TrimSpace(req.Query)
	if text == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	num := req.NumResults
	if num <= 0 {
		num = 5
	}
	if num > 20 {
		num = 20
	}

	q := provider.Query{
		Text:       text,
		NumResults: num,
	}
	res, err := s.router.Search(ctx, q, s.max)
	if err != nil {
		return nil, err
	}
	if len(res.Skipped) > 0 {
		s.log.Debug("部分平台被跳过", "skipped", strings.Join(res.Skipped, "; "))
	}
	return &Response{
		Count:   len(res.Result.Response.Results),
		Results: res.Result.Response.Results,
	}, nil
}
