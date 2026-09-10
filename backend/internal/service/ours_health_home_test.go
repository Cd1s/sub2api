package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------- 测试工具

type oursLogRecord struct {
	msg   string
	attrs map[string]any
}

type oursLogCapture struct {
	mu      sync.Mutex
	records []oursLogRecord
}

func (c *oursLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *oursLogCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *oursLogCapture) WithGroup(string) slog.Handler            { return c }
func (c *oursLogCapture) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Any()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, oursLogRecord{msg: record.Message, attrs: attrs})
	return nil
}

func (c *oursLogCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, record := range c.records {
		if record.msg == msg {
			n++
		}
	}
	return n
}

func (c *oursLogCapture) last(msg string) (oursLogRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.records) - 1; i >= 0; i-- {
		if c.records[i].msg == msg {
			return c.records[i], true
		}
	}
	return oursLogRecord{}, false
}

func captureOursLogs(t *testing.T) *oursLogCapture {
	t.Helper()
	previous := slog.Default()
	capture := &oursLogCapture{}
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

type oursTestClock struct{ now time.Time }

func (c *oursTestClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func withOursClock(t *testing.T) *oursTestClock {
	t.Helper()
	clock := &oursTestClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	previous := oursNow
	oursNow = func() time.Time { return clock.now }
	t.Cleanup(func() { oursNow = previous })
	return clock
}

func withProbeEvery(t *testing.T, n int) {
	t.Helper()
	previous := oursPrimaryProbeEvery
	oursPrimaryProbeEvery = n
	t.Cleanup(func() { oursPrimaryProbeEvery = previous })
}

func withReturnAfter(t *testing.T, d time.Duration) {
	t.Helper()
	previous := oursStickyReturnAfter
	oursStickyReturnAfter = d
	t.Cleanup(func() { oursStickyReturnAfter = previous })
}

func oursEscapeConfig(cfg *config.Config, enabled bool, errorRate float64) {
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = enabled
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = errorRate
}

// oursHealthTestScheduler：线上权重（W_priority=10000、W_error_rate=500），sticky_escape 阈值可调。
func oursHealthTestScheduler(escapeEnabled bool, errorRateThreshold float64) *defaultOpenAIAccountScheduler {
	cfg := &config.Config{}
	oursEscapeConfig(cfg, escapeEnabled, errorRateThreshold)
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{Priority: 10000, ErrorRate: 500}
	return &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{cfg: cfg}, stats: newOpenAIAccountRuntimeStats()}
}

func reportFailures(stats *openAIAccountRuntimeStats, accountID int64, n int) {
	for i := 0; i < n; i++ {
		stats.report(accountID, false, nil)
	}
}

// ---------------------------------------------------------------- 路由档位：健康降级 + 会话固定备用 + 滞回

// oursRouteRoster：组 7，本组 1，备用 2..8（7 个），兜底 9。
func oursRouteRoster() []Account {
	roster := []Account{*enrolledAccount(1, 50, 7)}
	for id := int64(2); id <= 8; id++ {
		roster = append(roster, *enrolledAccount(id, 50))
	}
	return append(roster, *enrolledAccount(9, 998))
}

func oursRouteReq(session string) OpenAIAccountScheduleRequest {
	return OpenAIAccountScheduleRequest{GroupID: int64Ptr(7), SessionHash: session}
}

func designatedOf(tiers map[int64]int) (int64, int) {
	var id int64
	n := 0
	for accountID, tier := range tiers {
		if tier == oursRouteDesignated {
			id, n = accountID, n+1
		}
	}
	return id, n
}

func TestOursRouteTiers_HealthyHomeOnTopAndNoSessionKeepsBackupsEqual(t *testing.T) {
	withGroupTiering(t, true)
	scheduler := oursHealthTestScheduler(true, 0.5)
	tiers := scheduler.oursRosterTiers(oursRouteReq(""), oursRouteRoster())
	require.Equal(t, oursRouteHome, tiers[1])
	for id := int64(2); id <= 8; id++ {
		require.Equalf(t, oursRouteHealthy, tiers[id], "无会话时健康备用同档：%d", id)
	}
	require.Equal(t, oursRouteFallback, tiers[9])
}

func TestOursRouteTiers_EachSessionGetsOneStableDesignatedBackup(t *testing.T) {
	withGroupTiering(t, true)
	scheduler := oursHealthTestScheduler(true, 0.5)
	first := scheduler.oursRosterTiers(oursRouteReq("sess-a"), oursRouteRoster())
	designated, n := designatedOf(first)
	require.Equal(t, 1, n, "每个会话恰好一个固定备用")
	require.NotEqual(t, int64(1), designated, "本组不是固定备用")
	require.NotEqual(t, int64(9), designated, "兜底不是固定备用")
	require.Equal(t, oursRouteHome, first[1], "本组健康时仍排第一")
	for i := 0; i < 20; i++ {
		again, _ := designatedOf(scheduler.oursRosterTiers(oursRouteReq("sess-a"), oursRouteRoster()))
		require.Equal(t, designated, again, "同一会话每次都是同一个固定备用")
	}
}

func TestOursRouteTiers_DesignationSpreadsSessionsEvenly(t *testing.T) {
	withGroupTiering(t, true)
	scheduler := oursHealthTestScheduler(true, 0.5)
	counts := map[int64]int{}
	const sessions = 7000
	for i := 0; i < sessions; i++ {
		id, _ := designatedOf(scheduler.oursRosterTiers(oursRouteReq(fmt.Sprintf("%032x", i*7919+13)), oursRouteRoster()))
		counts[id]++
	}
	require.Len(t, counts, 7)
	for id, n := range counts {
		require.InDeltaf(t, sessions/7, n, sessions/7*0.15, "备用 %d 分到 %d 个会话，偏离均值超过 15%%", id, n)
	}
}

func TestOursRouteTiers_UnhealthyDesignatedMovesOnlyItsSessions(t *testing.T) {
	withGroupTiering(t, true)
	scheduler := oursHealthTestScheduler(true, 0.5)
	before := map[string]int64{}
	for i := 0; i < 2000; i++ {
		session := fmt.Sprintf("s-%d", i)
		before[session], _ = designatedOf(scheduler.oursRosterTiers(oursRouteReq(session), oursRouteRoster()))
	}
	reportFailures(scheduler.stats, 4, 4) // 备用 4 变得不健康（0.5904）

	moved, kept := 0, 0
	for session, old := range before {
		now, _ := designatedOf(scheduler.oursRosterTiers(oursRouteReq(session), oursRouteRoster()))
		require.NotEqual(t, int64(4), now, "不健康的备用不再被选为固定备用")
		if old == 4 {
			moved++
		} else {
			require.Equalf(t, old, now, "会话 %s 原本不在 4 上，不得换号", session)
			kept++
		}
	}
	require.Greater(t, moved, 0)
	require.Greater(t, kept, 0)
	tiers := scheduler.oursRosterTiers(oursRouteReq(""), oursRouteRoster())
	require.Equal(t, oursRouteUnhealthy, tiers[4], "不健康备用排在健康备用之后")
}

func TestOursRouteTiers_ExcludedDesignatedFallsToStableNextChoice(t *testing.T) {
	withGroupTiering(t, true)
	scheduler := oursHealthTestScheduler(true, 0.5)
	first, _ := designatedOf(scheduler.oursRosterTiers(oursRouteReq("sess-x"), oursRouteRoster()))
	req := oursRouteReq("sess-x")
	req.ExcludedIDs = map[int64]struct{}{first: {}}
	second, n := designatedOf(scheduler.oursRosterTiers(req, oursRouteRoster()))
	require.Equal(t, 1, n)
	require.NotEqual(t, first, second)
	for i := 0; i < 10; i++ {
		again, _ := designatedOf(scheduler.oursRosterTiers(req, oursRouteRoster()))
		require.Equal(t, second, again, "failover 时下一个选择也是确定的")
	}
}

func TestOursRouteTiers_AllBackupsUnhealthyStillPinsSessionAndKeepsHome(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	scheduler := oursHealthTestScheduler(true, 0.5)
	for id := int64(1); id <= 8; id++ {
		reportFailures(scheduler.stats, id, 4)
	}
	tiers := scheduler.oursRosterTiers(oursRouteReq("sess-storm"), oursRouteRoster())
	require.Equal(t, oursRouteHome, tiers[1], "没有健康备用可让时本组不让位（保缓存）")
	designated, n := designatedOf(tiers)
	require.Equal(t, 1, n, "全局风暴时仍给会话固定一个备用，不追着最不差的号跑")
	again, _ := designatedOf(scheduler.oursRosterTiers(oursRouteReq("sess-storm"), oursRouteRoster()))
	require.Equal(t, designated, again)
}

// EWMA(alpha=0.2) 从 0 起连续失败：0.2、0.36、0.488、0.5904。
func TestOursPrimaryHealth_BelowThresholdKeepsHome(t *testing.T) {
	withGroupTiering(t, true)
	scheduler := oursHealthTestScheduler(true, 0.5)
	reportFailures(scheduler.stats, 1, 3) // 0.488 < 0.5
	require.Equal(t, oursRouteHome, scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1])
}

func TestOursPrimaryHealth_AboveThresholdYieldsToDesignatedBackup(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	scheduler := oursHealthTestScheduler(true, 0.5)
	reportFailures(scheduler.stats, 1, 4) // 0.5904 > 0.5

	tiers := scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())
	require.Equal(t, oursRouteUnhealthy, tiers[1])
	_, n := designatedOf(tiers)
	require.Equal(t, 1, n)
	require.Equal(t, oursRouteFallback, tiers[9], "兜底仍在备用之后")
}

func TestOursPrimaryHealth_EqualToThresholdDoesNotDemote(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	scheduler := oursHealthTestScheduler(true, 0.2)
	reportFailures(scheduler.stats, 1, 1) // 恰好 0.2
	errorRate, _, _ := scheduler.stats.snapshot(1)
	require.InDelta(t, 0.2, errorRate, 1e-12)
	require.Equal(t, oursRouteHome, scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1])
}

func TestOursPrimaryHealth_OnlyErrorRateCountsNotTTFT(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	scheduler := oursHealthTestScheduler(true, 0.5)
	slow := 60000
	for i := 0; i < 5; i++ {
		scheduler.stats.report(1, true, &slow)
	}
	require.Equal(t, oursRouteHome, scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1])
}

func TestOursPrimaryHealth_EscapeDisabledNeverDemotes(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	scheduler := oursHealthTestScheduler(false, 0.5)
	reportFailures(scheduler.stats, 1, 10)
	require.Equal(t, oursRouteHome, scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1])
}

// 滞回：超过 0.5 才降级，回落到 0.3（0.5 × 0.6）以下才恢复；切换日志各打一次。
func TestOursPrimaryHealth_HysteresisAndTransitionLogs(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	logs := captureOursLogs(t)
	scheduler := oursHealthTestScheduler(true, 0.5)
	homeTier := func() int { return scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1] }

	require.Equal(t, oursRouteHome, homeTier())
	reportFailures(scheduler.stats, 1, 4) // 0.5904
	require.Equal(t, oursRouteUnhealthy, homeTier())
	for _, want := range []float64{0.47232, 0.377856, 0.3022848} { // 仍 >= 0.3：保持降级
		scheduler.stats.report(1, true, nil)
		errorRate, _, _ := scheduler.stats.snapshot(1)
		require.InDelta(t, want, errorRate, 1e-9)
		require.Equalf(t, oursRouteUnhealthy, homeTier(), "错误率 %.4f 仍在滞回区间，不恢复", errorRate)
	}
	scheduler.stats.report(1, true, nil) // 0.2418 < 0.3
	require.Equal(t, oursRouteHome, homeTier())
	require.Equal(t, oursRouteHome, homeTier())

	require.Equal(t, 1, logs.count("ours_primary_demoted"))
	require.Equal(t, 1, logs.count("ours_primary_restored"))
	record, _ := logs.last("ours_primary_demoted")
	require.EqualValues(t, 1, record.attrs["account_id"])
	require.InDelta(t, 0.5, record.attrs["threshold"], 1e-12)
}

func TestOursNextUnhealthy(t *testing.T) {
	require.False(t, oursNextUnhealthy(false, 0.5, 0.5))
	require.True(t, oursNextUnhealthy(false, 0.51, 0.5))
	require.True(t, oursNextUnhealthy(true, 0.3, 0.5), "恰好 0.3 仍不恢复")
	require.False(t, oursNextUnhealthy(true, 0.29, 0.5))
	require.True(t, oursNextUnhealthy(true, 0.45, 0.5))
}

// 降级后固定备用的分数领先其它健康备用超过一个错误率满分（500），错误率不会把顺序打乱。
func TestOursRouteTiers_PlanScoresKeepTierOrderOverErrorTerm(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 0)
	scheduler := oursHealthTestScheduler(true, 0.5)
	reportFailures(scheduler.stats, 1, 4)
	roster := oursRouteRoster()
	req := oursRouteReq("sess-plan")
	req.oursTiers = scheduler.oursRosterTiers(req, roster)
	designated, _ := designatedOf(req.oursTiers)
	pool := make([]*Account, 0, len(roster))
	for i := range roster {
		pool = append(pool, &roster[i])
	}
	scores := openAIPlanScores(scheduler.buildOpenAIAccountLoadPlan(context.Background(), req, pool, map[int64]*AccountLoadInfo{}))
	for id, score := range scores {
		if id == designated {
			continue
		}
		require.Greaterf(t, scores[designated]-score, 500.0, "固定备用必须领先账号 %d 超过 500 分", id)
	}
}

func TestOursPrimaryHealth_ProbeKeepsHomeExactlyOncePerN(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 5)
	scheduler := oursHealthTestScheduler(true, 0.5)
	reportFailures(scheduler.stats, 1, 4)
	var homeRounds []int
	for i := 1; i <= 20; i++ {
		if scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1] == oursRouteHome {
			homeRounds = append(homeRounds, i)
		}
	}
	require.Equal(t, []int{5, 10, 15, 20}, homeRounds)
}

func TestOursPrimaryHealth_ExcludedPrimaryDoesNotConsumeProbe(t *testing.T) {
	withGroupTiering(t, true)
	withProbeEvery(t, 2)
	scheduler := oursHealthTestScheduler(true, 0.5)
	reportFailures(scheduler.stats, 1, 4)
	excluded := oursRouteReq("s")
	excluded.ExcludedIDs = map[int64]struct{}{1: {}}
	for i := 0; i < 5; i++ {
		scheduler.oursRosterTiers(excluded, oursRouteRoster())
	}
	require.Equal(t, oursRouteUnhealthy, scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1], "第 1 次")
	require.Equal(t, oursRouteHome, scheduler.oursRosterTiers(oursRouteReq("s"), oursRouteRoster())[1], "第 2 次是探测")
}

// ---------------------------------------------------------------- 本组表（黏住版）

func TestOursGroupHomeTable_StickyAcrossRosterDropoutAndConflict(t *testing.T) {
	withGroupTiering(t, true)
	state := &oursSchedulerState{}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour
	home := enrolledAccount(1, 50, 7)
	backup := enrolledAccount(2, 50)

	_, ok := state.soleHome(7, now, ttl)
	require.False(t, ok, "表里没有该组")

	state.observeHomes(7, []*Account{home, backup}, now, ttl)
	id, ok := state.soleHome(7, now, ttl)
	require.True(t, ok)
	require.EqualValues(t, 1, id)

	// 本组暂时掉出名单：记录保留。
	state.observeHomes(7, []*Account{backup}, now.Add(10*time.Minute), ttl)
	id, ok = state.soleHome(7, now.Add(10*time.Minute), ttl)
	require.True(t, ok)
	require.EqualValues(t, 1, id)

	// 它掉出名单期间另一个号也声明了本组：冲突，不做回家。
	other := enrolledAccount(3, 50, 7)
	state.observeHomes(7, []*Account{backup, other}, now.Add(20*time.Minute), ttl)
	_, ok = state.soleHome(7, now.Add(20*time.Minute), ttl)
	require.False(t, ok)

	// 名单里看到本组但它不再声明（撤了标记）：立即删除，只剩 3。
	unmarked := enrolledAccount(1, 50)
	state.observeHomes(7, []*Account{unmarked, backup, other}, now.Add(21*time.Minute), ttl)
	id, ok = state.soleHome(7, now.Add(21*time.Minute), ttl)
	require.True(t, ok)
	require.EqualValues(t, 3, id)

	// 超过 TTL 没见到：删除。
	_, ok = state.soleHome(7, now.Add(21*time.Minute+ttl+time.Second), ttl)
	require.False(t, ok)
}

func TestOursGroupHomeTable_UnenrolledGroupNeverRecorded(t *testing.T) {
	state := &oursSchedulerState{}
	now := time.Now()
	marked := &Account{ID: 1, Credentials: map[string]any{oursHomeGroupsCredentialKey: "7"}} // 有标记但没登记
	state.observeHomes(7, []*Account{marked, plainAccount(2, 50)}, now, time.Hour)
	_, ok := state.homes.Load(int64(7))
	require.False(t, ok)
}

// ---------------------------------------------------------------- 回家计时表

func TestOursOffHomeTable_ExpiryAndCap(t *testing.T) {
	state := &oursSchedulerState{}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	since, ok := state.touchOffHome(oursSessionKey{groupID: 7, sessionHash: "a"}, now)
	require.True(t, ok)
	require.Equal(t, now, since)
	since, ok = state.touchOffHome(oursSessionKey{groupID: 7, sessionHash: "a"}, now.Add(5*time.Minute))
	require.True(t, ok)
	require.Equal(t, now, since, "再次见到不重置起点")
	state.touchOffHome(oursSessionKey{groupID: 7, sessionHash: "b"}, now.Add(50*time.Minute))
	require.EqualValues(t, 2, state.offHomeCount.Load())

	// a 最后一次见到是 12:05，b 是 12:50；到 13:30 清理时只清掉 a。
	state.sweepOffHome(now.Add(90*time.Minute), ttl)
	require.EqualValues(t, 1, state.offHomeCount.Load())
	_, stillA := state.offHome.Load(oursSessionKey{groupID: 7, sessionHash: "a"})
	require.False(t, stillA)

	// 清理限频：1 分钟内不重复扫。
	state.sweepOffHome(now.Add(90*time.Minute+30*time.Second), time.Nanosecond)
	require.EqualValues(t, 1, state.offHomeCount.Load())

	// 上限：满了不再插入新条目。
	state.offHomeCount.Store(oursOffHomeMaxEntries)
	_, ok = state.touchOffHome(oursSessionKey{groupID: 7, sessionHash: "c"}, now)
	require.False(t, ok)
	_, present := state.offHome.Load(oursSessionKey{groupID: 7, sessionHash: "c"})
	require.False(t, present)

	state.forgetOffHome(oursSessionKey{groupID: 7, sessionHash: "b"})
	require.EqualValues(t, oursOffHomeMaxEntries-1, state.offHomeCount.Load())
}

// ---------------------------------------------------------------- 会话回家（端到端）

const (
	oursHomeGroup   = int64(30070)
	oursHomeID      = int64(31001)
	oursBackupAID   = int64(31002)
	oursBackupBID   = int64(31003)
	oursMonocleID   = int64(31009)
	oursSessionHash = "ours_session_1"
	oursSessionKeyR = "openai:" + oursSessionHash
)

type oursHomeFixture struct {
	svc     *OpenAIGatewayService
	cache   *schedulerTestGatewayCache
	acquire map[int64]bool
	clock   *oursTestClock
	logs    *oursLogCapture
}

func oursHomeAccount(id int64, priority int, homes string, enrolled bool) Account {
	credentials := map[string]any{}
	if enrolled {
		credentials[oursGroupTieringCredentialKey] = true
	}
	if homes != "" {
		credentials[oursHomeGroupsCredentialKey] = homes
	}
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Concurrency: 5, Priority: priority, GroupIDs: []int64{oursHomeGroup}, Credentials: credentials,
	}
}

func oursDefaultHomeAccounts() []Account {
	return []Account{
		oursHomeAccount(oursHomeID, 50, "30070", true),
		oursHomeAccount(oursBackupAID, 50, "", true),
		oursHomeAccount(oursBackupBID, 50, "", true),
		oursHomeAccount(oursMonocleID, 998, "", true),
	}
}

func newOursHomeFixture(t *testing.T, accounts []Account) *oursHomeFixture {
	t.Helper()
	withGroupTiering(t, true)
	withReturnAfter(t, 600*time.Second)
	withProbeEvery(t, 0)
	cfg := &config.Config{}
	oursEscapeConfig(cfg, true, 0.5)
	// 线上调度设置：W_priority=10000、W_error_rate=500、其余 0，lb_top_k=2。档位设计依赖这组权重。
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{Priority: 10000, ErrorRate: 500}
	cfg.Gateway.OpenAIWS.LBTopK = 2
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{}}
	acquire := map[int64]bool{}
	fixture := &oursHomeFixture{
		svc: &OpenAIGatewayService{
			accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
			cache:              cache,
			cfg:                cfg,
			rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: acquire}),
			openaiAccountStats: newOpenAIAccountRuntimeStats(),
		},
		cache:   cache,
		acquire: acquire,
		clock:   withOursClock(t),
		logs:    captureOursLogs(t),
	}
	return fixture
}

func (f *oursHomeFixture) selectFor(t *testing.T, ctx context.Context, previousResponseID, sessionHash, model string) (*AccountSelectionResult, OpenAIAccountScheduleDecision) {
	t.Helper()
	groupID := oursHomeGroup
	selection, decision, err := f.svc.SelectAccountWithScheduler(ctx, &groupID, previousResponseID, sessionHash, model, nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	return selection, decision
}

// seedHomeTable 用一个未绑定的会话走一次负载均衡，让本组表记下本组（线上每个新会话都会这样）。
func (f *oursHomeFixture) seedHomeTable(t *testing.T) {
	t.Helper()
	f.selectFor(t, context.Background(), "", "ours_seed_session", "gpt-5.1")
	delete(f.cache.sessionBindings, "openai:ours_seed_session")
}

func (f *oursHomeFixture) bindSessionTo(accountID int64) {
	f.cache.sessionBindings[oursSessionKeyR] = accountID
}

func TestOursReturnHome_StaysOnBackupBeforeThreshold(t *testing.T) {
	f := newOursHomeFixture(t, oursDefaultHomeAccounts())
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)

	selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1") // 开始计时
	require.Equal(t, oursBackupAID, selection.Account.ID)
	f.clock.advance(599 * time.Second)
	selection, _ = f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursBackupAID, selection.Account.ID)
	require.Equal(t, oursBackupAID, f.cache.sessionBindings[oursSessionKeyR])
	require.Zero(t, f.logs.count("ours_sticky_returned_home"))
}

func TestOursReturnHome_ReturnsWhenHealthyAndHasCapacity(t *testing.T) {
	f := newOursHomeFixture(t, oursDefaultHomeAccounts())
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)

	f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	f.clock.advance(600 * time.Second)
	selection, decision := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursHomeID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.Equal(t, oursHomeID, f.cache.sessionBindings[oursSessionKeyR], "Redis 绑定改成本组")
	require.Equal(t, 1, f.logs.count("ours_sticky_returned_home"))
	record, _ := f.logs.last("ours_sticky_returned_home")
	require.EqualValues(t, oursHomeGroup, record.attrs["group_id"])
	require.EqualValues(t, oursBackupAID, record.attrs["from_account_id"])
	require.EqualValues(t, oursHomeID, record.attrs["to_account_id"])
	require.EqualValues(t, 600, record.attrs["off_own_seconds"])

	// 回家后继续粘在本组，不再重复打日志。
	selection, _ = f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursHomeID, selection.Account.ID)
	require.Equal(t, 1, f.logs.count("ours_sticky_returned_home"))
}

func TestOursReturnHome_SwitchingBetweenBackupsKeepsTimer(t *testing.T) {
	f := newOursHomeFixture(t, oursDefaultHomeAccounts())
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)

	f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	f.clock.advance(300 * time.Second)
	f.bindSessionTo(oursBackupBID) // 只在备用之间换号
	f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	f.clock.advance(300 * time.Second)
	selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursHomeID, selection.Account.ID, "计时从第一次离开本组算起")
}

func TestOursReturnHome_UnhealthyHomeStaysWithoutEscapeLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		poison func(stats *openAIAccountRuntimeStats)
	}{
		{name: "错误率", poison: func(stats *openAIAccountRuntimeStats) { reportFailures(stats, oursHomeID, 4) }},
		{name: "首字", poison: func(stats *openAIAccountRuntimeStats) {
			slow := 60000
			stats.report(oursHomeID, true, &slow)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOursHomeFixture(t, oursDefaultHomeAccounts())
			f.seedHomeTable(t)
			f.bindSessionTo(oursBackupAID)
			tc.poison(f.svc.openaiAccountStats)

			f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
			f.clock.advance(600 * time.Second)
			selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
			require.Equal(t, oursBackupAID, selection.Account.ID)
			require.Equal(t, oursBackupAID, f.cache.sessionBindings[oursSessionKeyR])
			require.Zero(t, f.logs.count("sticky_escape_triggered"))
			require.Zero(t, f.logs.count("ours_sticky_returned_home"))
		})
	}
}

func TestOursReturnHome_FullHomeStaysOnSameBackupWithoutEscapeLog(t *testing.T) {
	f := newOursHomeFixture(t, oursDefaultHomeAccounts())
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)
	f.acquire[oursHomeID] = false // 本组并发满

	f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	f.clock.advance(600 * time.Second)
	selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursBackupAID, selection.Account.ID, "留在同一个备用，没有被换到别的备用")
	require.Equal(t, oursBackupAID, f.cache.sessionBindings[oursSessionKeyR])
	require.Zero(t, f.logs.count("sticky_escape_triggered"))
	require.Zero(t, f.logs.count("ours_sticky_returned_home"))
}

func TestOursReturnHome_HomeWithoutModelStays(t *testing.T) {
	accounts := oursDefaultHomeAccounts()
	accounts[0].Credentials["model_mapping"] = map[string]any{"gpt-other": "gpt-other"}
	f := newOursHomeFixture(t, accounts)
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)

	f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	f.clock.advance(600 * time.Second)
	selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursBackupAID, selection.Account.ID)
	require.Equal(t, oursBackupAID, f.cache.sessionBindings[oursSessionKeyR])
}

func TestOursReturnHome_DoesNothingWithoutUniqueRecordedHome(t *testing.T) {
	cases := []struct {
		name     string
		accounts []Account
		seed     bool
	}{
		{name: "表里没有该组", accounts: oursDefaultHomeAccounts(), seed: false},
		{name: "分组无登记账号", accounts: []Account{
			oursHomeAccount(oursHomeID, 50, "30070", false),
			oursHomeAccount(oursBackupAID, 50, "", false),
		}, seed: true},
		{name: "档 1 不唯一", accounts: []Account{
			oursHomeAccount(oursHomeID, 50, "30070", true),
			oursHomeAccount(oursBackupAID, 50, "", true),
			oursHomeAccount(oursBackupBID, 50, "30070", true),
		}, seed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOursHomeFixture(t, tc.accounts)
			if tc.seed {
				f.seedHomeTable(t)
			}
			f.bindSessionTo(oursBackupAID)
			f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
			f.clock.advance(time.Hour - time.Second)
			selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
			require.Equal(t, oursBackupAID, selection.Account.ID)
			require.Zero(t, f.logs.count("ours_sticky_returned_home"))
		})
	}
}

func TestOursReturnHome_PreviousResponseLayerUnchanged(t *testing.T) {
	accounts := oursDefaultHomeAccounts()
	for i := range accounts {
		accounts[i].Extra = map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}
	}
	f := newOursHomeFixture(t, accounts)
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)
	ctx := context.Background()
	require.NoError(t, f.svc.getOpenAIWSStateStore().BindResponseAccount(ctx, oursHomeGroup, "resp_ours_prev", oursBackupAID, time.Hour))

	f.selectFor(t, ctx, "", oursSessionHash, "gpt-5.1") // 开始计时
	f.clock.advance(2 * time.Hour)
	selection, decision := f.selectFor(t, ctx, "resp_ours_prev", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursBackupAID, selection.Account.ID, "previous_response_id 链留在产生它的账号上")
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	require.Zero(t, f.logs.count("ours_sticky_returned_home"))
}

func TestOursReturnHome_DisabledByReturnAfterZeroOrMasterSwitch(t *testing.T) {
	t.Run("RETURN_AFTER=0", func(t *testing.T) {
		f := newOursHomeFixture(t, oursDefaultHomeAccounts())
		withReturnAfter(t, 0)
		f.seedHomeTable(t)
		f.bindSessionTo(oursBackupAID)
		f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
		f.clock.advance(time.Hour - time.Second)
		selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
		require.Equal(t, oursBackupAID, selection.Account.ID)
	})
	t.Run("整体开关关闭", func(t *testing.T) {
		f := newOursHomeFixture(t, oursDefaultHomeAccounts())
		f.seedHomeTable(t)
		withGroupTiering(t, false)
		f.bindSessionTo(oursBackupAID)
		f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
		f.clock.advance(time.Hour - time.Second)
		selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
		require.Equal(t, oursBackupAID, selection.Account.ID)
		require.Zero(t, f.logs.count("ours_sticky_returned_home"))
	})
}

// 回滚时把 ours_tiering 写回 false：回家必须立刻停，不等本组表过期。
func TestOursReturnHome_StopsImmediatelyWhenHomeUnenrolled(t *testing.T) {
	accounts := oursDefaultHomeAccounts()
	f := newOursHomeFixture(t, accounts)
	f.seedHomeTable(t)
	f.bindSessionTo(oursBackupAID)
	f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	f.clock.advance(600 * time.Second)

	accounts[0].Credentials[oursGroupTieringCredentialKey] = false
	f.svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
	selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
	require.Equal(t, oursBackupAID, selection.Account.ID)
	require.Equal(t, oursBackupAID, f.cache.sessionBindings[oursSessionKeyR])
}

// 利润门生效时绑定也必须真的改成本组；本组被门否决时留在原备用。
func TestOursReturnHome_ProfitGateStillRebindsToHome(t *testing.T) {
	cheap := 0.1
	for _, tc := range []struct {
		name      string
		threshold float64
		wantID    int64
	}{
		{name: "门放行本组", threshold: 10, wantID: oursHomeID},
		{name: "门否决本组", threshold: 0.05, wantID: oursBackupAID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := oursDefaultHomeAccounts()
			accounts[0].RateMultiplier = &cheap
			backupRate := 0.01
			for i := 1; i < len(accounts); i++ {
				accounts[i].RateMultiplier = &backupRate
			}
			f := newOursHomeFixture(t, accounts)
			f.seedHomeTable(t)
			f.bindSessionTo(oursBackupAID)
			gate := &openAIProfitControlGate{groupID: oursHomeGroup, platform: PlatformOpenAI, threshold: tc.threshold}
			ctx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, gate)
			require.True(t, gatewayProfitControlGateActive(ctx))

			f.selectFor(t, ctx, "", oursSessionHash, "gpt-5.1")
			f.clock.advance(600 * time.Second)
			selection, _ := f.selectFor(t, ctx, "", oursSessionHash, "gpt-5.1")
			require.Equal(t, tc.wantID, selection.Account.ID)
			require.Equal(t, tc.wantID, f.cache.sessionBindings[oursSessionKeyR])
		})
	}
}

// 端到端：会话绑在不健康的本组上，连续请求都落到同一个固定备用，不在备用之间来回跳。
func TestOursRoute_UnhealthyHomeSessionPinsToOneBackupEndToEnd(t *testing.T) {
	f := newOursHomeFixture(t, oursDefaultHomeAccounts())
	f.seedHomeTable(t)
	f.bindSessionTo(oursHomeID)
	reportFailures(f.svc.openaiAccountStats, oursHomeID, 4)

	var picked []int64
	for i := 0; i < 8; i++ {
		selection, _ := f.selectFor(t, context.Background(), "", oursSessionHash, "gpt-5.1")
		picked = append(picked, selection.Account.ID)
	}
	require.NotEqual(t, oursHomeID, picked[0], "本组不健康时让位")
	require.NotEqual(t, oursMonocleID, picked[0], "有健康备用时不落兜底")
	for _, id := range picked {
		require.Equal(t, picked[0], id, "同一会话每次都落在同一个固定备用上")
	}
}

func TestOursParseNonNegativeIntEnv(t *testing.T) {
	require.Equal(t, 20, oursParseNonNegativeIntEnv("", 20))
	require.Equal(t, 0, oursParseNonNegativeIntEnv("0", 20))
	require.Equal(t, 7, oursParseNonNegativeIntEnv(" 7 ", 20))
	require.Equal(t, 20, oursParseNonNegativeIntEnv("-1", 20))
	require.Equal(t, 20, oursParseNonNegativeIntEnv("abc", 20))
}
