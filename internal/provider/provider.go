// Package provider 定义搜索平台的抽象接口与归一化结果模型。
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrorKind 描述一次失败的可恢复性，决定路由层是否退还预扣额度。
type ErrorKind string

const (
	// KindQuotaExhausted 额度耗尽：Exa/Brave 402，Tavily 432/433。
	KindQuotaExhausted ErrorKind = "quota_exhausted"
	// KindRateLimited 触发速率限制：429。进入冷却。
	KindRateLimited ErrorKind = "rate_limited"
	// KindAuth 鉴权或权限错误：401/403。需人工干预。
	KindAuth ErrorKind = "auth_error"
	// KindInvalidRequest 请求参数非法：400/422。
	KindInvalidRequest ErrorKind = "invalid_request"
	// KindTransient 瞬时故障：5xx、网络错误、超时。可安全重试并退还额度。
	KindTransient ErrorKind = "transient"
)

// Error 是带分类的平台错误。
type Error struct {
	Provider   string
	Kind       ErrorKind
	StatusCode int
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s 请求失败 (HTTP %d, %s): %s", e.Provider, e.StatusCode, e.Kind, e.Message)
	}
	return fmt.Sprintf("%s 请求失败 (%s): %s", e.Provider, e.Kind, e.Message)
}

// NewError 构造一个平台错误。
func NewError(provider string, kind ErrorKind, status int, msg string) *Error {
	return &Error{Provider: provider, Kind: kind, StatusCode: status, Message: msg}
}

// AsError 从 error 中提取平台错误，非平台错误返回 nil。
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// Result 是归一化后的单条搜索结果。
type Result struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score,omitempty"`
	Date    string  `json:"date,omitempty"`
}

// Query 是一次搜索请求的归一化参数。
type Query struct {
	Text       string
	NumResults int
}

// Response 是一次搜索的完整结果。
type Response struct {
	Provider string
	Results  []Result
	// CostUnits 本次消耗的 credit 数（Tavily advanced 为 2，其余为 1）。
	CostUnits float64
	// CostUSD 本次实际花费（美元），仅 Exa 能精确上报。
	CostUSD float64
	// ServerRemaining 服务端上报的剩余额度，仅 Brave 有意义（-1 表示未知）。
	ServerRemaining float64
	// ServerLimit 服务端上报的额度上限，0 表示不限量，仅 Brave 有意义。
	ServerLimit float64
	// ServerResetAt 服务端上报的下次重置时刻，零值表示未知。
	ServerResetAt time.Time
}

// Provider 是一个搜索平台的适配器。
type Provider interface {
	// Name 返回平台标识。
	Name() string
	// Search 执行一次搜索。
	Search(ctx context.Context, q Query) (*Response, error)
	// CostOf 返回某组参数下的预估消耗（credit）。
	CostOf(q Query) float64
}

// ErrorKindForStatus 把 HTTP 状态码映射到错误分类。
func ErrorKindForStatus(status int) ErrorKind {
	switch status {
	case 402:
		return KindQuotaExhausted
	case 432, 433:
		// Tavily 专属：432 计划额度超限，433 PayGo 额度超限。
		return KindQuotaExhausted
	case 429:
		return KindRateLimited
	case 401, 403:
		return KindAuth
	case 400, 422:
		return KindInvalidRequest
	default:
		return KindTransient
	}
}
