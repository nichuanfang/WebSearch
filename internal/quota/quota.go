// Package quota 实现各平台的额度周期模型与服务端真值采集。
//
// 三家平台的周期实测结论：
//   - Exa    自然月，每月 1 号归零，余额不累积（官方 pricing 文档明确）
//   - Brave  滚动 31 天（响应头 x-ratelimit-policy 实测 w=2678400），
//     且月度上限实测为 0 表示不限量，仅受 50 QPS 突发限制
//   - Tavily 滚动 1 个月（账单周期近似），/usage 接口实测不实时上报
package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/store"
)

// Period 描述一个额度周期。
type Period struct {
	Key   string
	Start time.Time
	End   time.Time
}

// Manager 负责周期推算与额度闸门。
type Manager struct {
	store *store.Store
	now   func() time.Time
}

// NewManager 构造 Manager。now 可注入以便测试。
func NewManager(s *store.Store, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{store: s, now: now}
}

// PeriodFor 按平台的周期模型推算当前所处的周期。
// anchor 为滚动周期的锚点日，零值时以当前时间作为锚点。
func (m *Manager) PeriodFor(model config.PeriodModel, anchor time.Time, now time.Time) Period {
	switch model {
	case config.PeriodCalendarMonth:
		return calendarMonthPeriod(now)
	case config.PeriodRolling31D:
		return rollingDaysPeriod(anchor, now, 31)
	case config.PeriodRolling1M:
		return rollingMonthPeriod(anchor, now)
	default:
		return calendarMonthPeriod(now)
	}
}

// calendarMonthPeriod 返回自然月周期：1 号 00:00 到次月 1 号 00:00。
func calendarMonthPeriod(now time.Time) Period {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 1, 0)
	return Period{
		Key:   start.Format("2006-01"),
		Start: start,
		End:   end,
	}
}

// rollingDaysPeriod 返回以 anchor 为起点、每 span 天滚动一次的周期。
func rollingDaysPeriod(anchor, now time.Time, span int) Period {
	if anchor.IsZero() {
		anchor = now
	}
	anchor = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, now.Location())
	elapsed := int(now.Sub(anchor).Hours() / 24)
	if elapsed < 0 {
		elapsed = 0
	}
	idx := elapsed / span
	start := anchor.AddDate(0, 0, idx*span)
	end := start.AddDate(0, 0, span)
	return Period{
		Key:   fmt.Sprintf("%s#%d", anchor.Format("2006-01-02"), idx),
		Start: start,
		End:   end,
	}
}

// rollingMonthPeriod 返回以 anchor 为起点、每 1 个月滚动一次的周期。
func rollingMonthPeriod(anchor, now time.Time) Period {
	if anchor.IsZero() {
		anchor = now
	}
	anchor = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, now.Location())
	// 不能只按月差推算：AddDate 会归一化月末日期（1/31 + 1 月 = 3/3），
	// 导致周期跨度漂移、now 落在周期之外。这里按实际起点逐月推进，
	// 每个周期从上一个周期的起点加一个月，保证 [start, end) 一定包含 now。
	if !now.After(anchor) {
		return Period{
			Key:   anchor.Format("2006-01-02") + "#0",
			Start: anchor,
			End:   nextMonth(anchor, 1),
		}
	}
	start := anchor
	idx := 0
	for {
		end := nextMonth(start, 1)
		if now.Before(end) {
			return Period{
				Key:   fmt.Sprintf("%s#%d", anchor.Format("2006-01-02"), idx),
				Start: start,
				End:   end,
			}
		}
		start = end
		idx++
	}
}

// nextMonth 返回 from 之后 n 个月的起点。
// 若目标月没有对应日期（如 1/31 → 2/31），收敛到该月最后一天，
// 避免 AddDate 归一化到下一月。
func nextMonth(from time.Time, n int) time.Time {
	y, m, d := from.Date()
	target := time.Date(y, m+time.Month(n), 1, 0, 0, 0, 0, from.Location())
	last := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, from.Location()).Day()
	if d > last {
		d = last
	}
	return time.Date(target.Year(), target.Month(), d, 0, 0, 0, 0, from.Location())
}

// Status 是一个平台的额度状态快照。
type Status struct {
	Provider     string
	PeriodKey    string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	LocalUsed    int
	Limit        int
	Remaining    int
	State        store.State
	ServerUsed   float64
	ServerLimit  float64
	HasServer    bool
	ServerReset  time.Time
	CooldownTill time.Time
	LastError    string
	// Truth 标注本地剩余量的推导依据：local / server / mixed。
	Truth string
}

// StatusOf 读取某平台当前周期的额度状态。
func (m *Manager) StatusOf(ctx context.Context, name string, pc config.ProviderConfig) (*Status, error) {
	now := m.now()
	period := m.PeriodFor(pc.PeriodModel, m.anchorFor(ctx, name), now)
	q, err := m.store.GetQuota(ctx, name, period.Key)
	if err != nil {
		return nil, err
	}
	st := &Status{
		Provider:    name,
		PeriodKey:   period.Key,
		PeriodStart: period.Start,
		PeriodEnd:   period.End,
		Limit:       pc.MonthlyLimit,
		State:       store.StateOK,
		Truth:       "local",
	}
	if q != nil {
		st.LocalUsed = q.LocalUsed
		st.State = q.State
		st.CooldownTill = q.CooldownUntil
		st.LastError = q.LastError
		if q.HasServerUsed {
			st.ServerUsed = q.ServerUsed
			st.HasServer = true
		}
		if q.HasServerLimit {
			st.ServerLimit = q.ServerLimit
		}
		st.ServerReset = q.ServerResetAt
	}
	st.Remaining = st.Limit - st.LocalUsed
	if st.Remaining < 0 {
		st.Remaining = 0
	}
	// 服务端真值参与收敛：Brave 的月度上限实测为 0（不限量），不参与取最严。
	if st.HasServer && st.ServerLimit > 0 {
		serverRemaining := int(st.ServerLimit - st.ServerUsed)
		if serverRemaining < 0 {
			serverRemaining = 0
		}
		if serverRemaining < st.Remaining {
			st.Remaining = serverRemaining
			st.Truth = "server"
		} else {
			st.Truth = "mixed"
		}
	}
	return st, nil
}

// anchorFor 读取滚动周期的锚点日；未记录时返回零值。
func (m *Manager) anchorFor(ctx context.Context, name string) time.Time {
	// 锚点记录在滚动周期的首行。跨周期后需要在新周期重新定位，
	// 因此这里扫描该平台最近的锚点记录，取最早的一个作为锚点。
	anchors, err := m.store.ListAnchors(ctx, name)
	if err != nil || len(anchors) == 0 {
		return time.Time{}
	}
	return anchors[0]
}

// EnsureAnchor 为滚动周期记录锚点日（首次使用时确定）。
func (m *Manager) EnsureAnchor(ctx context.Context, name string, pc config.ProviderConfig, period Period) error {
	if pc.PeriodModel == config.PeriodCalendarMonth {
		return nil
	}
	existing, err := m.store.GetQuota(ctx, name, period.Key)
	if err != nil {
		return err
	}
	if existing != nil && !existing.AnchorDate.IsZero() {
		return nil
	}
	return m.store.SetAnchorDate(ctx, name, period.Key, period.Start)
}

// GateResult 是放行闸门的判定结果。
type GateResult struct {
	Allowed bool
	Reason  string
}

// CheckGate 判定某平台本次调用是否放行。
// units 为本次预估消耗的 credit 数。
func (m *Manager) CheckGate(ctx context.Context, name string, pc config.ProviderConfig, units float64) (GateResult, error) {
	now := m.now()
	st, err := m.StatusOf(ctx, name, pc)
	if err != nil {
		return GateResult{Reason: "读取额度失败: " + err.Error()}, err
	}
	switch st.State {
	case store.StateAuthError:
		return GateResult{Reason: "平台鉴权失败，需人工干预"}, nil
	case store.StateExhausted:
		return GateResult{Reason: fmt.Sprintf("额度已耗尽（周期 %s）", st.PeriodKey)}, nil
	case store.StateCooldown:
		if st.CooldownTill.After(now) {
			return GateResult{Reason: fmt.Sprintf("速率限制冷却中，预计 %s 恢复", st.CooldownTill.Format("15:04:05"))}, nil
		}
	}
	if float64(st.Remaining) < units {
		return GateResult{Reason: fmt.Sprintf("剩余额度 %d 不足以本次消耗 %.0f", st.Remaining, units)}, nil
	}
	return GateResult{Allowed: true}, nil
}

// ApplyServerTruth 记录一次服务端真值上报。
func (m *Manager) ApplyServerTruth(ctx context.Context, name string, pc config.ProviderConfig, serverUsed, serverLimit float64, resetAt time.Time) error {
	now := m.now()
	period := m.PeriodFor(pc.PeriodModel, m.anchorFor(ctx, name), now)
	return m.store.UpdateServerTruth(ctx, name, period.Key, serverUsed, serverLimit, resetAt, now)
}

// CurrentPeriod 返回某平台当前周期。
func (m *Manager) CurrentPeriod(ctx context.Context, name string, pc config.ProviderConfig) (Period, error) {
	now := m.now()
	period := m.PeriodFor(pc.PeriodModel, m.anchorFor(ctx, name), now)
	return period, nil
}
