package service

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// fork 私有补丁（v0.2.4-ours.2）：本组健康门控降级 + 防饿死探测 + 会话自动回家。
//
//   - 健康降级：本组（档 1）账号的运行时错误率超过 sticky_escape 的错误率阈值时，本次选号
//     把它降为档 2，由现有的 W_error_rate 在它和备用之间挑更健康的。只看错误率，不看首字延迟。
//   - 防饿死探测：被降级的本组每第 N 次选号仍按档 1 参与一次，让错误率 EWMA 有机会回落。
//   - 会话回家：会话绑在非本组账号满 RETURN_AFTER 秒、且本组健康有余量时，下一次请求把它
//     改绑回本组。取代外部的 sub2api-cascade-sticky-expire 定时器。
//
// 所有状态都挂在调度器实例上（进程内、重启清空），不改 openAIAccountRuntimeStat。

const (
	oursPrimaryProbeEveryEnvKey  = "SUB2API_OURS_PRIMARY_PROBE_EVERY"
	oursStickyReturnAfterEnvKey  = "SUB2API_OURS_STICKY_RETURN_AFTER_SECONDS"
	oursDefaultPrimaryProbeEvery = 20
	oursDefaultStickyReturnAfter = 600

	// oursOffHomeMaxEntries 是回家计时表的条目上限；满了就不再给新会话计时（这些会话本次不回家）。
	oursOffHomeMaxEntries = 20000
	// oursOffHomeSweepInterval 是计时表过期清理的最短间隔。
	oursOffHomeSweepInterval = time.Minute
)

var (
	// oursPrimaryProbeEvery：被降级的本组每第 N 次选号保持档 1；0 表示关闭探测。
	oursPrimaryProbeEvery = oursParseNonNegativeIntEnv(os.Getenv(oursPrimaryProbeEveryEnvKey), oursDefaultPrimaryProbeEvery)
	// oursStickyReturnAfter：会话离开本组多久后尝试回家；0 表示关闭回家。
	oursStickyReturnAfter = time.Duration(oursParseNonNegativeIntEnv(os.Getenv(oursStickyReturnAfterEnvKey), oursDefaultStickyReturnAfter)) * time.Second
	// oursNow 可在测试中替换。
	oursNow = time.Now
)

func oursParseNonNegativeIntEnv(raw string, fallback int) int {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// ---------------------------------------------------------------- 状态

type oursSchedulerState struct {
	demoted sync.Map // accountID int64 -> *atomic.Bool（本组是否处于降级状态，只用于切换日志）
	probes  sync.Map // accountID int64 -> *atomic.Uint64（降级期间的选号计数，决定探测轮次）
	homes   sync.Map // groupID int64 -> *oursGroupHome

	offHome      sync.Map // oursSessionKey -> *oursOffHomeEntry
	offHomeCount atomic.Int64
	lastSweep    atomic.Int64 // UnixNano
}

// oursGroupHome 记录「谁在名单里声明了自己是这个组的本组」，以及最近一次看到它声明的时间。
//
// 黏住语义：
//   - 名单里看到某账号声明本组 → 记录/刷新；
//   - 名单里看到它但它不再声明（或已退出登记）→ 立即删除；
//   - 它暂时掉出名单（429、过载、临时不可调度）→ 记录保留；
//   - 超过粘性 TTL 没再见到 → 删除。
//
// 恰好一个声明者时它就是本组；两个及以上视为冲突，该组不做回家。
type oursGroupHome struct {
	mu        sync.Mutex
	declarers map[int64]time.Time
}

type oursSessionKey struct {
	groupID     int64
	sessionHash string
}

type oursOffHomeEntry struct {
	since    time.Time    // 首次发现会话绑在非本组账号的时间；只在备用之间换号时不重置
	lastSeen atomic.Int64 // UnixNano，用于过期清理
}

var oursSchedulerStates sync.Map // *defaultOpenAIAccountScheduler -> *oursSchedulerState

// oursMapLoad / oursMapLoadOrStore 是 sync.Map 的类型安全封装：每张表只存一种值类型。
func oursMapLoad[T any](m *sync.Map, key any) (*T, bool) {
	value, ok := m.Load(key)
	if !ok {
		return nil, false
	}
	typed, ok := value.(*T)
	return typed, ok
}

func oursMapLoadOrStore[T any](m *sync.Map, key any, newValue func() *T) *T {
	if typed, ok := oursMapLoad[T](m, key); ok {
		return typed
	}
	value, _ := m.LoadOrStore(key, newValue())
	if typed, ok := value.(*T); ok {
		return typed
	}
	fresh := newValue()
	m.Store(key, fresh)
	return fresh
}

func (s *defaultOpenAIAccountScheduler) oursState() *oursSchedulerState {
	return oursMapLoadOrStore(&oursSchedulerStates, s, func() *oursSchedulerState { return &oursSchedulerState{} })
}

func (s *defaultOpenAIAccountScheduler) oursStickyTTL() time.Duration {
	if s == nil || s.service == nil {
		return openaiStickySessionTTL
	}
	return s.service.openAIWSSessionStickyTTL()
}

// ---------------------------------------------------------------- 档位 + 健康降级（挂钩 A）

// oursRosterTiers 在分组完整名单（failover 排除与运行时封禁之前）上计算本次选号用的档位表：
// 先按本组标记分档，顺手刷新本组表，再对档 1 做健康降级。返回新 map，不改原始档位表。
func (s *defaultOpenAIAccountScheduler) oursRosterTiers(req OpenAIAccountScheduleRequest, roster []Account) map[int64]int {
	if !oursGroupTieringEnabled || len(roster) == 0 {
		return nil
	}
	accounts := make([]*Account, 0, len(roster))
	for i := range roster {
		accounts = append(accounts, &roster[i])
	}
	if s == nil {
		return oursGroupTiers(accounts, req.GroupID)
	}
	state := s.oursState()
	if req.GroupID != nil {
		state.observeHomes(*req.GroupID, accounts, oursNow(), s.oursStickyTTL())
	}
	tiers := oursGroupTiers(accounts, req.GroupID)
	return s.oursApplyPrimaryHealth(state, req, tiers)
}

func (s *defaultOpenAIAccountScheduler) oursApplyPrimaryHealth(state *oursSchedulerState, req OpenAIAccountScheduleRequest, tiers map[int64]int) map[int64]int {
	if tiers == nil || s.stats == nil || s.service == nil {
		return tiers
	}
	cfg := s.service.openAIStickyEscapeConfig()
	var adjusted map[int64]int
	for accountID, tier := range tiers {
		if tier != oursTierPrimary {
			continue
		}
		errorRate, _, _ := s.stats.snapshot(accountID)
		demote := cfg.enabled && errorRate > cfg.errorRate
		state.recordPrimaryHealth(accountID, demote, errorRate, cfg.errorRate)
		if !demote {
			continue
		}
		if _, excluded := req.ExcludedIDs[accountID]; excluded {
			// 本次请求已经排除了它（failover 重试）：档位无关紧要，也不消耗探测名额。
			continue
		}
		if state.takeProbe(accountID) {
			continue
		}
		if adjusted == nil {
			adjusted = make(map[int64]int, len(tiers))
			for id, t := range tiers {
				adjusted[id] = t
			}
		}
		adjusted[accountID] = oursTierBackup
	}
	if adjusted == nil {
		return tiers
	}
	return adjusted
}

func (state *oursSchedulerState) recordPrimaryHealth(accountID int64, demoted bool, errorRate, threshold float64) {
	flag, ok := oursMapLoad[atomic.Bool](&state.demoted, accountID)
	if !ok {
		if !demoted {
			return
		}
		flag = oursMapLoadOrStore(&state.demoted, accountID, func() *atomic.Bool { return new(atomic.Bool) })
	}
	if demoted {
		if flag.CompareAndSwap(false, true) {
			slog.Info("ours_primary_demoted", "account_id", accountID, "error_rate", errorRate, "threshold", threshold)
		}
		return
	}
	if flag.CompareAndSwap(true, false) {
		slog.Info("ours_primary_restored", "account_id", accountID, "error_rate", errorRate, "threshold", threshold)
	}
}

// takeProbe 返回本次是否为探测轮次：降级期间每第 N 次选号返回 true。确定、无随机数。
func (state *oursSchedulerState) takeProbe(accountID int64) bool {
	every := oursPrimaryProbeEvery
	if every <= 0 {
		return false
	}
	counter := oursMapLoadOrStore(&state.probes, accountID, func() *atomic.Uint64 { return new(atomic.Uint64) })
	n := counter.Add(1)
	return n%uint64(every) == 0
}

// ---------------------------------------------------------------- 本组表

func (state *oursSchedulerState) observeHomes(groupID int64, roster []*Account, now time.Time, ttl time.Duration) {
	home, ok := oursMapLoad[oursGroupHome](&state.homes, groupID)
	if !ok {
		declared := false
		for _, account := range roster {
			if oursTieringEnrolled(account) && oursDeclaresHomeGroup(account, groupID) {
				declared = true
				break
			}
		}
		if !declared {
			return
		}
		home = oursMapLoadOrStore(&state.homes, groupID, func() *oursGroupHome {
			return &oursGroupHome{declarers: make(map[int64]time.Time)}
		})
	}
	home.mu.Lock()
	defer home.mu.Unlock()
	for _, account := range roster {
		if account == nil {
			continue
		}
		if oursTieringEnrolled(account) && oursDeclaresHomeGroup(account, groupID) {
			home.declarers[account.ID] = now
		} else {
			delete(home.declarers, account.ID)
		}
	}
	for accountID, seen := range home.declarers {
		if now.Sub(seen) > ttl {
			delete(home.declarers, accountID)
		}
	}
}

// soleHome 返回该组唯一的本组账号；表中没有该组、没有声明者或声明者冲突时返回 false。
func (state *oursSchedulerState) soleHome(groupID int64, now time.Time, ttl time.Duration) (int64, bool) {
	home, ok := oursMapLoad[oursGroupHome](&state.homes, groupID)
	if !ok {
		return 0, false
	}
	home.mu.Lock()
	defer home.mu.Unlock()
	var homeID int64
	count := 0
	for accountID, seen := range home.declarers {
		if now.Sub(seen) > ttl {
			continue
		}
		homeID = accountID
		count++
	}
	return homeID, count == 1
}

// ---------------------------------------------------------------- 回家计时表

func (state *oursSchedulerState) touchOffHome(key oursSessionKey, now time.Time) (time.Time, bool) {
	if entry, ok := oursMapLoad[oursOffHomeEntry](&state.offHome, key); ok {
		entry.lastSeen.Store(now.UnixNano())
		return entry.since, true
	}
	if state.offHomeCount.Load() >= oursOffHomeMaxEntries {
		return time.Time{}, false
	}
	entry := &oursOffHomeEntry{since: now}
	entry.lastSeen.Store(now.UnixNano())
	actual, loaded := state.offHome.LoadOrStore(key, entry)
	if !loaded {
		state.offHomeCount.Add(1)
		return now, true
	}
	existing, ok := actual.(*oursOffHomeEntry)
	if !ok {
		return time.Time{}, false
	}
	existing.lastSeen.Store(now.UnixNano())
	return existing.since, true
}

func (state *oursSchedulerState) forgetOffHome(key oursSessionKey) {
	if _, loaded := state.offHome.LoadAndDelete(key); loaded {
		state.offHomeCount.Add(-1)
	}
}

// sweepOffHome 清掉超过 ttl 没再见到的计时条目；最多每 oursOffHomeSweepInterval 执行一次。
func (state *oursSchedulerState) sweepOffHome(now time.Time, ttl time.Duration) {
	last := state.lastSweep.Load()
	if last != 0 && now.UnixNano()-last < int64(oursOffHomeSweepInterval) {
		return
	}
	if !state.lastSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	cutoff := now.Add(-ttl).UnixNano()
	state.offHome.Range(func(key, value any) bool {
		if entry, ok := value.(*oursOffHomeEntry); !ok || entry.lastSeen.Load() < cutoff {
			if _, loaded := state.offHome.LoadAndDelete(key); loaded {
				state.offHomeCount.Add(-1)
			}
		}
		return true
	})
}

// ---------------------------------------------------------------- 会话回家（挂钩 E）

// oursTryReturnHome 在会话粘性层之前尝试把「绑在非本组账号上太久」的会话改绑回本组。
//
// 返回 ok=false 时调用方继续走原来的流程，会话原样留在当前账号——
// 失败时不许借机把它换到别的备用上（换号会丢上游缓存），下次请求再试。
func (s *defaultOpenAIAccountScheduler) oursTryReturnHome(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	decision *OpenAIAccountScheduleDecision,
) (*AccountSelectionResult, bool) {
	if !oursGroupTieringEnabled || oursStickyReturnAfter <= 0 || s == nil || s.service == nil || req.GroupID == nil {
		return nil, false
	}
	sessionHash := strings.TrimSpace(req.SessionHash)
	if sessionHash == "" || req.StickyAccountID <= 0 || req.GuardianParentAccountID != 0 || req.PreserveStickyBinding {
		return nil, false
	}
	if preserveOpenAIGuardianParentBinding(ctx, sessionHash) {
		return nil, false
	}

	state := s.oursState()
	now := oursNow()
	ttl := s.oursStickyTTL()
	state.sweepOffHome(now, ttl)

	groupID := *req.GroupID
	homeID, ok := state.soleHome(groupID, now, ttl)
	if !ok {
		return nil, false
	}
	key := oursSessionKey{groupID: groupID, sessionHash: sessionHash}
	if req.StickyAccountID == homeID {
		state.forgetOffHome(key)
		return nil, false
	}
	since, tracked := state.touchOffHome(key, now)
	if !tracked || now.Sub(since) < oursStickyReturnAfter {
		return nil, false
	}
	if _, excluded := req.ExcludedIDs[homeID]; excluded {
		return nil, false
	}
	// 刚回去就会被逃逸规则踢走的（错误率或首字）不回。这里只读内存统计，不打日志。
	if _, _, _, escape := s.shouldEscapeStickyAccount(homeID, s.service.openAIStickyEscapeConfig()); escape {
		return nil, false
	}

	// 照 guardian-parent 层的写法借用 selectBySessionHash：模型映射、传输、分组、隐私、
	// 配额、利润门等兼容性检查全部复用。PreserveStickyBinding 保证失败时原绑定一个字节都不动；
	// oursQuietEscape 让它内部的 sticky_escape_triggered 日志静默（运营者靠它统计真实逃逸）。
	homeReq := req
	homeReq.StickyAccountID = homeID
	homeReq.PreserveStickyBinding = true
	homeReq.oursQuietEscape = true
	selection, _, err := s.selectBySessionHash(ctx, homeReq)
	if err != nil || selection == nil || selection.Account == nil {
		return nil, false
	}
	release := func() {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
	// 只接受真正拿到槽位的结果；WaitPlan（需要排队）一律放弃。
	// 登记或本组标记在这一刻被撤掉（例如 ours_tiering 写回 false 回滚）也放弃，保证秒级回滚。
	if !selection.Acquired || !oursTieringEnrolled(selection.Account) || !oursDeclaresHomeGroup(selection.Account, groupID) {
		release()
		return nil, false
	}
	// 直接写绑定：有利润门时 bindOpenAIStickySessionDuringSelection 不绑，而 handler 的
	// BindStickySessionAfterProfitAdmission 不会覆盖已有的不同绑定，会造成「请求落在本组、
	// 绑定仍指向备用」。本组已在 selectBySessionHash 里通过了利润否决检查。
	if err := s.service.BindStickySession(ctx, req.GroupID, sessionHash, homeID); err != nil {
		slog.Warn("ours_sticky_return_home_bind_failed", "group_id", groupID, "account_id", homeID, "error", err)
		release()
		return nil, false
	}
	state.forgetOffHome(key)
	slog.Info("ours_sticky_returned_home",
		"group_id", groupID,
		"from_account_id", req.StickyAccountID,
		"to_account_id", homeID,
		"off_own_seconds", int64(now.Sub(since)/time.Second),
	)
	if decision != nil {
		decision.Layer = openAIAccountScheduleLayerSessionSticky
		decision.StickySessionHit = true
		decision.SelectedAccountID = selection.Account.ID
		decision.SelectedAccountType = selection.Account.Type
	}
	return selection, true
}

// oursStickyEscapeInfo 返回 sticky_escape_triggered 的日志函数：回家尝试时静默，其它情况原样 slog.Info。
func oursStickyEscapeInfo(req OpenAIAccountScheduleRequest) func(string, ...any) {
	if req.oursQuietEscape {
		return func(string, ...any) {}
	}
	return slog.Info
}

// ---------------------------------------------------------------- 管理端快照按组（挂钩 C）

type oursSchedulerScoreGroupCtxKey struct{}

// OursWithSchedulerScoreGroup 在管理端按分组计算 scheduler_scores 时把分组 ID 放进 ctx，
// 让诊断快照与真实路由用同一套「本组标记」规则。没有分组上下文时按「无本组」算。
func OursWithSchedulerScoreGroup(ctx context.Context, groupID *int64) context.Context {
	if ctx == nil || groupID == nil || *groupID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, oursSchedulerScoreGroupCtxKey{}, *groupID)
}

// oursWeightsWithScoreGroup 把 ctx 里的分组 ID 带进按值传递的权重视图（快照内层函数没有 ctx）。
func oursWeightsWithScoreGroup(ctx context.Context, weights GatewayOpenAIWSSchedulerScoreWeightsView) GatewayOpenAIWSSchedulerScoreWeightsView {
	weights.oursGroupID = nil
	if ctx != nil {
		if groupID, ok := ctx.Value(oursSchedulerScoreGroupCtxKey{}).(int64); ok && groupID > 0 {
			weights.oursGroupID = &groupID
		}
	}
	return weights
}

// oursSnapshotTiers 是诊断快照用的档位表：与真实路由同一规则，但不做健康降级
// （快照路径把 errorRate 固定为 0，降级靠 ours_primary_demoted 日志观察）。
func oursSnapshotTiers(accounts []*Account, weights GatewayOpenAIWSSchedulerScoreWeightsView) map[int64]int {
	return oursGroupTiers(accounts, weights.oursGroupID)
}
