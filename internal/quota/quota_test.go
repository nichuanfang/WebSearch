package quota

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nichuanfang/WebSearch/internal/config"
	"github.com/nichuanfang/WebSearch/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewManager(s, nil), s
}

func TestCalendarMonthPeriod(t *testing.T) {
	m := &Manager{}
	// 3 月 15 日应落在 3 月周期。
	now := time.Date(2026, 3, 15, 13, 30, 0, 0, time.UTC)
	p := m.PeriodFor(config.PeriodCalendarMonth, time.Time{}, now)
	if p.Key != "2026-03" {
		t.Errorf("period_key = %q, 期望 2026-03", p.Key)
	}
	if p.Start.Day() != 1 || p.Start.Hour() != 0 {
		t.Errorf("起始应为当月 1 日 00:00, 实际 %v", p.Start)
	}
	if p.End.Month() != 4 || p.End.Day() != 1 {
		t.Errorf("结束应为次月 1 日, 实际 %v", p.End)
	}
}

func TestCalendarMonthBoundary(t *testing.T) {
	m := &Manager{}
	// 4 月 1 日 00:00 应切换到 4 月周期。
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	p := m.PeriodFor(config.PeriodCalendarMonth, time.Time{}, now)
	if p.Key != "2026-04" {
		t.Errorf("跨月后 period_key = %q, 期望 2026-04", p.Key)
	}
}

func TestRolling31dPeriod(t *testing.T) {
	m := &Manager{}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 锚点后 30 天仍在第 0 个周期。
	now := anchor.AddDate(0, 0, 30)
	p := m.PeriodFor(config.PeriodRolling31D, anchor, now)
	if p.Key != "2026-01-01#0" {
		t.Errorf("第 30 天 period_key = %q, 期望 2026-01-01#0", p.Key)
	}
	// 锚点后 31 天进入第 1 个周期。
	now = anchor.AddDate(0, 0, 31)
	p = m.PeriodFor(config.PeriodRolling31D, anchor, now)
	if p.Key != "2026-01-01#1" {
		t.Errorf("第 31 天 period_key = %q, 期望 2026-01-01#1", p.Key)
	}
}

func TestRolling1mPeriod(t *testing.T) {
	m := &Manager{}
	anchor := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	// 同月内。
	now := time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC)
	p := m.PeriodFor(config.PeriodRolling1M, anchor, now)
	if p.Key != "2026-01-15#0" {
		t.Errorf("同月 period_key = %q, 期望 2026-01-15#0", p.Key)
	}
	// 跨月。
	now = time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)
	p = m.PeriodFor(config.PeriodRolling1M, anchor, now)
	if p.Key != "2026-01-15#1" {
		t.Errorf("跨月 period_key = %q, 期望 2026-01-15#1", p.Key)
	}
}

func TestServerLimitZeroSkipsConvergence(t *testing.T) {
	ctx := context.Background()
	m, s := newTestManager(t)
	now := time.Now()
	pc := config.ProviderConfig{MonthlyLimit: 1000, PeriodModel: config.PeriodCalendarMonth}

	period := m.PeriodFor(pc.PeriodModel, time.Time{}, now)
	// Brave 实测：server_limit = 0 表示不限量。
	if err := s.UpdateServerTruth(ctx, "brave", period.Key, 0, 0, time.Time{}, now); err != nil {
		t.Fatalf("写入服务端额度失败: %v", err)
	}
	st, err := m.StatusOf(ctx, "brave", pc)
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if st.Remaining != 1000 {
		t.Errorf("不限量平台的剩余额度 = %d, 期望等于本地上限 1000", st.Remaining)
	}
	if st.Truth != "local" {
		t.Errorf("真值来源 = %q, 期望 local", st.Truth)
	}
}

func TestServerLimitConvergesRemaining(t *testing.T) {
	ctx := context.Background()
	m, s := newTestManager(t)
	now := time.Now()
	pc := config.ProviderConfig{MonthlyLimit: 1000, PeriodModel: config.PeriodCalendarMonth}
	period := m.PeriodFor(pc.PeriodModel, time.Time{}, now)

	// 服务端说只剩 10，本地闸门应取更严的 10。
	if err := s.UpdateServerTruth(ctx, "exa", period.Key, 90, 100, time.Time{}, now); err != nil {
		t.Fatalf("写入服务端额度失败: %v", err)
	}
	st, err := m.StatusOf(ctx, "exa", pc)
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if st.Remaining != 10 {
		t.Errorf("剩余额度 = %d, 期望取服务端真值 10", st.Remaining)
	}
	if st.Truth != "server" {
		t.Errorf("真值来源 = %q, 期望 server", st.Truth)
	}
}

func TestCheckGateBlocksWhenExhausted(t *testing.T) {
	ctx := context.Background()
	m, s := newTestManager(t)
	now := time.Now()
	pc := config.ProviderConfig{MonthlyLimit: 10, PeriodModel: config.PeriodCalendarMonth}
	period := m.PeriodFor(pc.PeriodModel, time.Time{}, now)

	if err := s.SetState(ctx, "exa", period.Key, store.StateExhausted, time.Time{}, "额度耗尽"); err != nil {
		t.Fatalf("设置状态失败: %v", err)
	}
	gate, err := m.CheckGate(ctx, "exa", pc, 1)
	if err != nil {
		t.Fatalf("闸门检查失败: %v", err)
	}
	if gate.Allowed {
		t.Error("额度耗尽的平台不应放行")
	}
}

func TestCheckGateBlocksWhenCooldownActive(t *testing.T) {
	ctx := context.Background()
	m, s := newTestManager(t)
	now := time.Now()
	pc := config.ProviderConfig{MonthlyLimit: 100, PeriodModel: config.PeriodCalendarMonth}
	period := m.PeriodFor(pc.PeriodModel, time.Time{}, now)

	if err := s.SetState(ctx, "tavily", period.Key, store.StateCooldown, now.Add(time.Minute), "限速"); err != nil {
		t.Fatalf("设置状态失败: %v", err)
	}
	gate, err := m.CheckGate(ctx, "tavily", pc, 1)
	if err != nil {
		t.Fatalf("闸门检查失败: %v", err)
	}
	if gate.Allowed {
		t.Error("冷却中的平台不应放行")
	}
}

func TestCheckGateAllowsWithinLimit(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t)
	pc := config.ProviderConfig{MonthlyLimit: 100, PeriodModel: config.PeriodCalendarMonth}
	gate, err := m.CheckGate(ctx, "exa", pc, 1)
	if err != nil {
		t.Fatalf("闸门检查失败: %v", err)
	}
	if !gate.Allowed {
		t.Errorf("额度充足时应放行, 实际拒绝原因: %s", gate.Reason)
	}
}
