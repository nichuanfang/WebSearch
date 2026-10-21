package fetch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nichuanfang/WebSearch/internal/provider"
)

// RemoteFetcher 是一个远程抓取通道。
type RemoteFetcher interface {
	// Name 返回通道标识。
	Name() string
	// Fetch 抓取指定 URL 的正文。
	Fetch(ctx context.Context, rawURL string) (*Result, error)
}

// Manager 按优先级组合远程抓取与本地兜底。
//
// 默认顺序：Jina Reader → 本地抓取。远程通道能处理 JS 渲染页，
// 本地通道在远程不可用或限流时保证基础页面仍可读取。
type Manager struct {
	remote   []RemoteFetcher
	local    *Fetcher
	log      *slog.Logger
	now      func() time.Time
	cooldown time.Duration
	mu       sync.Mutex
	states   map[string]cooldownState
}

type cooldownState struct {
	until time.Time
	err   error
}

// NewManager 构造抓取管理器。remote 按优先级排列，local 可为 nil。
func NewManager(remote []RemoteFetcher, local *Fetcher, cooldown time.Duration, log *slog.Logger) *Manager {
	if cooldown <= 0 {
		cooldown = DefaultFetchCooldown
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		remote:   remote,
		local:    local,
		log:      log,
		now:      time.Now,
		cooldown: cooldown,
		states:   map[string]cooldownState{},
	}
}

// DefaultFetchCooldown 是远程抓取限流后的默认冷却时长。
const DefaultFetchCooldown = 5 * time.Minute

// RemoteNames 返回已装配的远程通道名称，按优先级排列。
// 供诊断与测试确认实际生效的链路。
func (m *Manager) RemoteNames() []string {
	out := make([]string, 0, len(m.remote))
	for _, r := range m.remote {
		out = append(out, r.Name())
	}
	return out
}

// Fetch 按顺序尝试远程通道，最后回落到本地抓取。
//
// 远程通道返回反爬/限流错误时进入冷却；其他错误直接尝试下一个通道。
func (m *Manager) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	now := m.now()
	var causes []string

	for _, r := range m.remote {
		name := r.Name()
		if reason, cooling := m.cooling(name, now); cooling {
			causes = append(causes, name+": "+reason)
			continue
		}
		start := time.Now()
		res, err := r.Fetch(ctx, rawURL)
		latency := time.Since(start).Milliseconds()
		if err == nil {
			if reason := remoteQualityIssue(res); reason != "" {
				m.log.Warn("远程抓取质量不足，改用本地提取",
					"provider", name,
					"url", rawURL,
					"reason", reason,
					"latency_ms", latency)
				causes = append(causes, name+": "+reason)
				continue
			}
			if strings.TrimSpace(res.Title) == "" {
				// 纯 Markdown / 纯文本没有 <title>，用正文首个标题行补位。
				res.Title = titleFromMarkdown(res.Content)
			}
			m.log.Info("抓取完成",
				"provider", name,
				"url", res.URL,
				"title", res.Title,
				"chars", len(res.Content),
				"latency_ms", latency)
			return res, nil
		}
		m.log.Warn("远程抓取失败",
			"provider", name,
			"error", err.Error(),
			"latency_ms", latency)
		if pe := provider.AsError(err); pe != nil && pe.Kind == provider.KindRateLimited {
			m.markCooldown(name, now.Add(m.cooldown), err)
		}
		causes = append(causes, name+": "+err.Error())
	}

	if m.local == nil {
		return nil, fmt.Errorf("抓取失败: %s", strings.Join(causes, "; "))
	}
	res, err := m.local.Fetch(ctx, rawURL)
	if err == nil {
		m.log.Info("抓取完成",
			"provider", "local",
			"url", res.URL,
			"title", res.Title,
			"chars", len(res.Content))
		return res, nil
	}
	causes = append(causes, "local: "+err.Error())
	return nil, &FetchError{Causes: causes}
}

// titleFromMarkdown 从 Markdown 正文的首个标题行推断标题。
// 找不到时返回空字符串，不影响正文返回。
func titleFromMarkdown(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		// 首行就是标题的情况：取前 100 字符作为标题。
		if len([]rune(line)) > 100 {
			return string([]rune(line)[:100])
		}
		return line
	}
	return ""
}

// remoteQualityIssue 判断远程抓取结果是否明显不可用。
// Jina 对部分站点会直接透传原始 HTML 或拿不到标题，这类结果交给本地提取更可靠。
func remoteQualityIssue(res *Result) string {
	if res == nil || strings.TrimSpace(res.Content) == "" {
		return "远程返回空正文"
	}
	head := res.Content
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(head)
	if strings.Contains(lower, "<!doctype html") || strings.Contains(lower, "<html") {
		return "远程返回原始 HTML"
	}
	return ""
}

func (m *Manager) cooling(name string, now time.Time) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[name]
	if !ok || !state.until.After(now) {
		return "", false
	}
	return fmt.Sprintf("冷却中，预计 %s 恢复", state.until.Format("15:04:05")), true
}

func (m *Manager) markCooldown(name string, until time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[name] = cooldownState{until: until, err: err}
}

// FetchError 汇总所有抓取通道的失败原因。
type FetchError struct {
	Causes []string
}

func (e *FetchError) Error() string {
	if len(e.Causes) == 0 {
		return "抓取失败"
	}
	return "抓取失败 -> " + strings.Join(e.Causes, "; ")
}

// ErrRateLimited 用于判断错误是否属于限流。
func ErrRateLimited(err error) bool {
	var pe *provider.Error
	return errors.As(err, &pe) && pe.Kind == provider.KindRateLimited
}
