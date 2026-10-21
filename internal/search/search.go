// Package search 承载 web_search 工具的业务逻辑。
package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nichuanfang/WebSearch/internal/provider"
	"github.com/nichuanfang/WebSearch/internal/router"
)

// Service 是搜索服务。
type Service struct {
	router *router.Router
	log    *slog.Logger
	max    int
	// fallbacks 是免密钥通道，按优先级排列。
	fallbacks []fallbackTarget
	// fallbackCooldown 是兜底失败后的冷却时长。
	fallbackCooldown time.Duration
	now              func() time.Time

	mu sync.Mutex
	// cooldowns 记录各通道的冷却截止时间与最近错误。
	cooldowns map[string]fallbackState
}

// fallbackTarget 是一个免密钥通道。
type fallbackTarget struct {
	Provider provider.Provider
	// Terminal 表示该通道是最后手段，失败后不再继续。
	Terminal bool
}

// fallbackState 记录通道的冷却状态。
type fallbackState struct {
	Until time.Time
	Err   error
}

// New 构造 Service。
func New(r *router.Router, log *slog.Logger, maxAttempts int) *Service {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return &Service{router: r, log: log, max: maxAttempts, now: time.Now}
}

// WithFallback 追加一个免密钥兜底通道。
// 按加入顺序依次尝试；仅当付费平台全部不可用时才会调用。
func (s *Service) WithFallback(p provider.Provider, cooldown time.Duration) *Service {
	return s.addFallback(p, cooldown, false)
}

// WithFallbackOptions 追加一个免密钥通道，可指定是否为最后手段。
func (s *Service) WithFallbackOptions(p provider.Provider, cooldown time.Duration, terminal bool) *Service {
	return s.addFallback(p, cooldown, terminal)
}

func (s *Service) addFallback(p provider.Provider, cooldown time.Duration, terminal bool) *Service {
	if cooldown <= 0 {
		cooldown = 10 * time.Minute
	}
	s.fallbacks = append(s.fallbacks, fallbackTarget{Provider: p, Terminal: terminal})
	s.fallbackCooldown = cooldown
	if s.now == nil {
		s.now = time.Now
	}
	if s.cooldowns == nil {
		s.cooldowns = map[string]fallbackState{}
	}
	return s
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
		var allFailed *router.AllFailedError
		if len(s.fallbacks) == 0 || !errors.As(err, &allFailed) {
			return nil, err
		}
		return s.executeFallback(ctx, q, allFailed)
	}
	if len(res.Skipped) > 0 {
		s.log.Debug("部分平台被跳过", "skipped", strings.Join(res.Skipped, "; "))
	}
	return &Response{
		Count:   len(res.Result.Response.Results),
		Results: res.Result.Response.Results,
	}, nil
}

// executeFallback 在所有付费平台不可用时，按优先级尝试免密钥通道。
func (s *Service) executeFallback(ctx context.Context, q provider.Query, allFailed *router.AllFailedError) (*Response, error) {
	now := s.now()
	reasons := make([]string, 0, len(s.fallbacks))
	var lastErr error

	for _, target := range s.fallbacks {
		name := target.Provider.Name()
		if reason, cooling := s.cooling(name, now); cooling {
			reasons = append(reasons, name+": "+reason)
			continue
		}

		start := time.Now()
		resp, err := target.Provider.Search(ctx, q)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			lastErr = err
			s.log.Warn("免密钥搜索失败",
				"provider", name,
				"terminal", target.Terminal,
				"error", err.Error(),
				"latency_ms", latency)
			// 限速与反爬继续请求只会更容易被封，进入冷却。
			if pe := provider.AsError(err); pe != nil && pe.Kind == provider.KindRateLimited {
				s.markCooldown(name, now.Add(s.fallbackCooldown), err)
			}
			reasons = append(reasons, name+": "+err.Error())
			if target.Terminal {
				break
			}
			continue
		}

		s.log.Info("免密钥搜索完成",
			"provider", name,
			"results", len(resp.Results),
			"latency_ms", latency)
		return &Response{Count: len(resp.Results), Results: resp.Results}, nil
	}

	return nil, fallbackFailed(allFailed, reasons, lastErr)
}

// cooling 返回通道是否仍在冷却期。
func (s *Service) cooling(name string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.cooldowns[name]
	if !ok || !state.Until.After(now) {
		return "", false
	}
	return fmt.Sprintf("冷却中，预计 %s 恢复", state.Until.Format("15:04:05")), true
}

func (s *Service) markCooldown(name string, until time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooldowns == nil {
		s.cooldowns = map[string]fallbackState{}
	}
	s.cooldowns[name] = fallbackState{Until: until, Err: err}
}

// fallbackFailed 合并付费平台与各免密钥通道的失败原因，返回统一的错误文本。
func fallbackFailed(allFailed *router.AllFailedError, reasons []string, cause error) error {
	// AllFailedError 自带「所有搜索平台都不可用」前缀，这里不要重复拼接。
	msg := allFailed.Error()
	if len(reasons) > 0 {
		msg += "; " + strings.Join(reasons, "; ")
	}
	return &FallbackError{Message: msg, Cause: cause}
}

// FallbackError 表示付费平台与免密钥兜底都不可用。
type FallbackError struct {
	Message string
	Cause   error
}

func (e *FallbackError) Error() string { return e.Message }

func (e *FallbackError) Unwrap() error { return e.Cause }
