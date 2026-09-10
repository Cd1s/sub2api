package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// enrolledAccount 构造一个已登记分档的账号；homes 非空时写入 ours_home_groups（逗号分隔字符串）。
func enrolledAccount(id int64, priority int, homes ...int64) *Account {
	credentials := map[string]any{oursGroupTieringCredentialKey: true}
	if len(homes) > 0 {
		parts := make([]string, 0, len(homes))
		for _, home := range homes {
			parts = append(parts, fmt.Sprintf("%d", home))
		}
		credentials[oursHomeGroupsCredentialKey] = strings.Join(parts, ",")
	}
	return &Account{ID: id, Priority: priority, Credentials: credentials}
}

// plainAccount 构造一个未登记的账号（credentials 里没有 ours_tiering）。
func plainAccount(id int64, priority int) *Account {
	return &Account{ID: id, Priority: priority}
}

// withGroupTiering 在单个测试内临时改写整体开关，结束后恢复。
func withGroupTiering(t *testing.T, enabled bool) {
	t.Helper()
	previous := oursGroupTieringEnabled
	oursGroupTieringEnabled = enabled
	t.Cleanup(func() { oursGroupTieringEnabled = previous })
}

// openAITieringTestScheduler 只让 priority 参与打分，plan 里的 score 就等于 10000 × priorityFactor。
func openAITieringTestScheduler() *defaultOpenAIAccountScheduler {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority: 10000,
	}
	return &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{cfg: cfg}}
}

// ---------------------------------------------------------------- 目标形态：8 个本组账号 + 1 个兜底，全部绑进全部 24 个组

type fleetAccount struct {
	id       int64
	priority int
	homes    []int64
}

// testFleet：8 个本组账号，各自声明 3 个本组分组，priority 为原梯子 10..80；兜底 998 不写标记。
var testFleet = []fleetAccount{
	{id: 101, priority: 10, homes: []int64{11, 12, 13}},
	{id: 102, priority: 20, homes: []int64{21, 22, 23}},
	{id: 103, priority: 30, homes: []int64{31, 32, 33}},
	{id: 104, priority: 40, homes: []int64{41, 42, 43}},
	{id: 105, priority: 50, homes: []int64{51, 52, 53}},
	{id: 106, priority: 60, homes: []int64{61, 62, 63}},
	{id: 107, priority: 70, homes: []int64{71, 72, 73}},
	{id: 108, priority: 80, homes: []int64{81, 82, 83}},
}

const fleetFallbackID = int64(109)

func fleetGroups() []int64 {
	var groups []int64
	for _, account := range testFleet {
		groups = append(groups, account.homes...)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return groups
}

func fleetHomeOf(groupID int64) int64 {
	for _, account := range testFleet {
		for _, home := range account.homes {
			if home == groupID {
				return account.id
			}
		}
	}
	return 0
}

// fleetRoster 返回全量绑定后任一本组分组的名单；unifiedPriority>0 时 8 个本组账号统一成该值。
func fleetRoster(unifiedPriority int) []*Account {
	roster := make([]*Account, 0, len(testFleet)+1)
	for _, account := range testFleet {
		priority := account.priority
		if unifiedPriority > 0 {
			priority = unifiedPriority
		}
		roster = append(roster, enrolledAccount(account.id, priority, account.homes...))
	}
	return append(roster, enrolledAccount(fleetFallbackID, 998))
}

func TestOursGroupTiers_FullBindingEveryGroupHasOnlyItsHomeAsPrimary(t *testing.T) {
	withGroupTiering(t, true)

	groups := fleetGroups()
	require.Len(t, groups, 24)
	roster := fleetRoster(0)
	for _, groupID := range groups {
		tiers := oursGroupTiers(roster, int64Ptr(groupID))
		home := fleetHomeOf(groupID)
		for _, account := range roster {
			switch account.ID {
			case home:
				require.Equalf(t, oursTierPrimary, tiers[account.ID], "组 %d 的本组 %d 必须是档 1", groupID, account.ID)
			case fleetFallbackID:
				require.Equalf(t, oursTierFallback, tiers[account.ID], "组 %d 的兜底必须是档 3", groupID)
			default:
				require.Equalf(t, oursTierBackup, tiers[account.ID], "组 %d 里其它本组账号 %d 必须是档 2", groupID, account.ID)
			}
		}
	}
}

func TestOursGroupTiers_UnifiedPrioritiesKeepTiers(t *testing.T) {
	withGroupTiering(t, true)

	ladder := fleetRoster(0)
	unified := fleetRoster(50)
	for _, groupID := range fleetGroups() {
		require.Equalf(t, oursGroupTiers(ladder, int64Ptr(groupID)), oursGroupTiers(unified, int64Ptr(groupID)),
			"组 %d：8 个本组账号 priority 统一后档位不得变化", groupID)
	}
}

func TestOursGroupTiers_HomeAbsentFromRosterHasNoPrimary(t *testing.T) {
	withGroupTiering(t, true)

	// 组 31 的本组 103 暂时掉出名单（429、过载、临时不可调度）。
	var roster []*Account
	for _, account := range fleetRoster(0) {
		if account.ID != 103 {
			roster = append(roster, account)
		}
	}
	tiers := oursGroupTiers(roster, int64Ptr(31))
	for id, tier := range tiers {
		require.NotEqualf(t, oursTierPrimary, tier, "本组不在名单时账号 %d 不得顶替成档 1", id)
	}
	require.Equal(t, oursTierFallback, tiers[fleetFallbackID])
	require.Equal(t, oursTierBackup, tiers[101], "priority 最小的 101 也只能是备用")
}

func TestOursGroupTiers_UnmarkedGroupHasNoPrimary(t *testing.T) {
	withGroupTiering(t, true)

	// 没有本组标记的分组（例如 1）：登记账号同档，兜底最后，未登记的排在所有登记账号之后。
	roster := []*Account{
		plainAccount(110, 10),
		enrolledAccount(101, 10, 11, 12, 13),
		enrolledAccount(102, 20, 21, 22, 23),
		enrolledAccount(104, 40, 41, 42, 43),
		enrolledAccount(fleetFallbackID, 998),
	}
	require.Equal(t, map[int64]int{
		110:             oursTierUnenrolled,
		101:             oursTierBackup,
		102:             oursTierBackup,
		104:             oursTierBackup,
		fleetFallbackID: oursTierFallback,
	}, oursGroupTiers(roster, int64Ptr(1)))
}

func TestOursGroupTiers_NoGroupContextHasNoPrimary(t *testing.T) {
	withGroupTiering(t, true)

	tiers := oursGroupTiers(fleetRoster(0), nil)
	for id, tier := range tiers {
		require.NotEqualf(t, oursTierPrimary, tier, "没有分组上下文时账号 %d 不得是档 1", id)
	}
}

func TestOursGroupTiers_MaxPriorityTieHasNoFallback(t *testing.T) {
	withGroupTiering(t, true)

	roster := []*Account{
		enrolledAccount(1, 10, 7),
		enrolledAccount(2, 50),
		enrolledAccount(3, 998),
		enrolledAccount(4, 998),
	}
	require.Equal(t, map[int64]int{
		1: oursTierPrimary,
		2: oursTierBackup,
		3: oursTierBackup,
		4: oursTierBackup,
	}, oursGroupTiers(roster, int64Ptr(7)))
}

func TestOursGroupTiers_ConflictingHomesAreBothPrimary(t *testing.T) {
	withGroupTiering(t, true)

	roster := []*Account{
		enrolledAccount(1, 10, 7),
		enrolledAccount(2, 20, 7),
		enrolledAccount(3, 998),
	}
	require.Equal(t, map[int64]int{
		1: oursTierPrimary,
		2: oursTierPrimary,
		3: oursTierFallback,
	}, oursGroupTiers(roster, int64Ptr(7)))
}

func TestOursGroupTiers_UnenrolledAndDisabled(t *testing.T) {
	withGroupTiering(t, true)

	require.Nil(t, oursGroupTiers(nil, int64Ptr(7)))
	require.Nil(t, oursGroupTiers([]*Account{plainAccount(1, 10), plainAccount(2, 20)}, int64Ptr(7)),
		"池内没有登记账号时不分档")

	// 声明了本组但没登记：不算本组。
	marked := &Account{ID: 5, Priority: 1, Credentials: map[string]any{oursHomeGroupsCredentialKey: "7"}}
	tiers := oursGroupTiers([]*Account{marked, enrolledAccount(6, 50), enrolledAccount(7, 998)}, int64Ptr(7))
	require.Equal(t, oursTierUnenrolled, tiers[5])

	withGroupTiering(t, false)
	require.Nil(t, oursGroupTiers(fleetRoster(0), int64Ptr(31)), "整体开关关闭时永远不分档")
}

func TestOursParseHomeGroups(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []int64
	}{
		{name: "逗号分隔字符串", value: "11,12,13", want: []int64{11, 12, 13}},
		{name: "带空白", value: " 8 , 9 ,17 ", want: []int64{8, 9, 17}},
		{name: "单个", value: "12", want: []int64{12}},
		{name: "重复去重", value: "8,8,9", want: []int64{8, 9}},
		{name: "JSON 数组字符串（数字）", value: "[8, 9, 17]", want: []int64{8, 9, 17}},
		{name: "JSON 数组字符串（数字字符串）", value: `["8","9"]`, want: []int64{8, 9}},
		{name: "已解码数组（json.Number / 字符串 / float64）", value: []any{json.Number("8"), "9", float64(17)}, want: []int64{8, 9, 17}},
		{name: "空串", value: "", want: nil},
		{name: "空白串", value: "   ", want: nil},
		{name: "空数组", value: "[]", want: nil},
		{name: "非法元素整条作废", value: "8,x,9", want: nil},
		{name: "空元素整条作废", value: "8,,9", want: nil},
		{name: "零或负数作废", value: "0,-1", want: nil},
		{name: "小数作废", value: []any{float64(8.5)}, want: nil},
		{name: "JSON 解析失败", value: "[8,", want: nil},
		{name: "单个数字（不是列表）", value: 8, want: nil},
		{name: "nil", value: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, oursParseHomeGroups(tc.value))
		})
	}
}

func TestOursTieringEnrolled(t *testing.T) {
	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil 账号", account: nil, want: false},
		{name: "无 Credentials", account: &Account{ID: 1}, want: false},
		{name: "无 ours_tiering 键", account: &Account{ID: 1, Credentials: map[string]any{"pool_mode": true}}, want: false},
		{name: "布尔 true", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": true}}, want: true},
		{name: "布尔 false", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": false}}, want: false},
		{name: "字符串 true", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": "true"}}, want: true},
		{name: "字符串 TRUE 带空格", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": " TRUE "}}, want: true},
		{name: "字符串 1", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": "1"}}, want: true},
		{name: "字符串 yes 不算", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": "yes"}}, want: false},
		{name: "数字 1 不算", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": 1}}, want: false},
		{name: "nil 值不算", account: &Account{ID: 1, Credentials: map[string]any{"ours_tiering": nil}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, oursTieringEnrolled(tc.account))
		})
	}
}

func TestOursGroupTieringEnabledFromEnv(t *testing.T) {
	cases := map[string]bool{
		"": true, "1": true, "true": true, "on": true,
		"0": false, "false": false, "FALSE": false, " off ": false,
	}
	for raw, want := range cases {
		require.Equalf(t, want, oursGroupTieringEnabledFromEnv(raw), "SUB2API_OURS_GROUP_TIERING=%q", raw)
	}
}

func TestOpenAISchedulingPriorityFor(t *testing.T) {
	account := enrolledAccount(7, 42)
	require.Equal(t, 42, openAISchedulingPriorityFor(account, nil))
	require.Equal(t, 42, openAISchedulingPriorityFor(account, map[int64]int{99: oursTierPrimary}))
	require.Equal(t, oursTierBackup, openAISchedulingPriorityFor(account, map[int64]int{7: oursTierBackup}))
	require.Equal(t, 0, openAISchedulingPriorityFor(nil, map[int64]int{7: oursTierBackup}))
}

// ---------------------------------------------------------------- 挂钩 D：tie-break

// 同档（priority 相同）时必须按 LoadRate 打破平手，而不是退回全局 accounts.priority。
func TestIsOpenAIAccountCandidateBetter_SameTierBreaksByLoadRate(t *testing.T) {
	heavy := openAIAccountCandidateScore{
		account: &Account{ID: 11, Priority: 20}, loadInfo: &AccountLoadInfo{LoadRate: 80},
		score: 10, priority: oursTierBackup,
	}
	light := openAIAccountCandidateScore{
		account: &Account{ID: 12, Priority: 70}, loadInfo: &AccountLoadInfo{LoadRate: 10},
		score: 10, priority: oursTierBackup,
	}
	require.True(t, isOpenAIAccountCandidateBetter(light, heavy), "同档时在飞更少的账号更优")
	require.False(t, isOpenAIAccountCandidateBetter(heavy, light))

	primary := heavy
	primary.priority = oursTierPrimary
	require.True(t, isOpenAIAccountCandidateBetter(primary, light), "档位更小者优先于负载更低者")
}

// 候选未经过打分循环（priority 为零值）时回落 accounts.priority，保护上游 TestSelectTopKOpenAICandidates。
func TestIsOpenAIAccountCandidateBetter_UnscoredCandidateFallsBackToAccountPriority(t *testing.T) {
	low := openAIAccountCandidateScore{account: &Account{ID: 11, Priority: 1}, loadInfo: &AccountLoadInfo{LoadRate: 80}, score: 10}
	high := openAIAccountCandidateScore{account: &Account{ID: 12, Priority: 2}, loadInfo: &AccountLoadInfo{LoadRate: 10}, score: 10}
	require.True(t, isOpenAIAccountCandidateBetter(low, high))
	require.False(t, isOpenAIAccountCandidateBetter(high, low))
}

// ---------------------------------------------------------------- 挂钩 B：真实路由

func openAIPlanPriorities(plan openAIAccountLoadPlan) map[int64]int {
	priorities := make(map[int64]int, len(plan.candidates))
	for _, candidate := range plan.candidates {
		priorities[candidate.account.ID] = candidate.priority
	}
	return priorities
}

func TestBuildOpenAIAccountLoadPlan_EnrolledAccountsUseTiers(t *testing.T) {
	withGroupTiering(t, true)

	filtered := []*Account{
		enrolledAccount(1, 10, 7), // 本组
		enrolledAccount(2, 20),    // 备用
		enrolledAccount(3, 30),    // 备用
		enrolledAccount(4, 998),   // 兜底
	}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{GroupID: int64Ptr(7)}, filtered, map[int64]*AccountLoadInfo{},
	)
	require.Equal(t, map[int64]int{1: oursTierPrimary, 2: oursTierBackup, 3: oursTierBackup, 4: oursTierFallback}, openAIPlanPriorities(plan))

	scores := openAIPlanScores(plan)
	require.InDelta(t, 10000.0, scores[1], 1e-6, "本组 pf = 1.0")
	require.InDelta(t, 5000.0, scores[2], 1e-6, "备用 pf = 0.5")
	require.InDelta(t, 5000.0, scores[3], 1e-6, "两个备用分数必须完全相同")
	require.InDelta(t, 0.0, scores[4], 1e-6, "兜底 pf = 0")
}

func TestBuildOpenAIAccountLoadPlan_UnenrolledPoolMatchesUpstream(t *testing.T) {
	withGroupTiering(t, true)

	filtered := []*Account{plainAccount(1, 10), plainAccount(2, 20), plainAccount(3, 30), plainAccount(4, 998)}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{GroupID: int64Ptr(7)}, filtered, map[int64]*AccountLoadInfo{},
	)
	require.Equal(t, map[int64]int{1: 10, 2: 20, 3: 30, 4: 998}, openAIPlanPriorities(plan))

	scores := openAIPlanScores(plan)
	span := float64(998 - 10)
	for _, account := range filtered {
		want := 10000 * (1 - float64(account.Priority-10)/span)
		require.InDeltaf(t, want, scores[account.ID], 1e-6, "账号 %d 的分数应与上游归一化结果一致", account.ID)
	}
}

func TestBuildOpenAIAccountLoadPlan_DisabledFallsBackToUpstream(t *testing.T) {
	withGroupTiering(t, false)

	filtered := []*Account{enrolledAccount(1, 10, 7), enrolledAccount(2, 20), enrolledAccount(3, 998)}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{GroupID: int64Ptr(7)}, filtered, map[int64]*AccountLoadInfo{},
	)
	require.Equal(t, map[int64]int{1: 10, 2: 20, 3: 998}, openAIPlanPriorities(plan))
}

// 本组账号被 failover 排除出池后，剩下的备用必须保持同档、分数相同，由 LoadRate 决定谁先上。
func TestBuildOpenAIAccountLoadPlan_RosterTiersSurviveExclusion(t *testing.T) {
	withGroupTiering(t, true)

	roster := []Account{
		*enrolledAccount(1, 10, 7),
		*enrolledAccount(2, 20),
		*enrolledAccount(3, 30),
		*enrolledAccount(4, 40),
		*enrolledAccount(5, 998),
	}
	scheduler := openAITieringTestScheduler()
	req := OpenAIAccountScheduleRequest{GroupID: int64Ptr(7)}
	req.oursTiers = scheduler.oursRosterTiers(req, roster)

	pool := []*Account{&roster[1], &roster[2], &roster[3], &roster[4]}
	loadMap := map[int64]*AccountLoadInfo{
		2: {AccountID: 2, LoadRate: 90},
		3: {AccountID: 3, LoadRate: 5},
		4: {AccountID: 4, LoadRate: 80},
		5: {AccountID: 5},
	}
	plan := scheduler.buildOpenAIAccountLoadPlan(context.Background(), req, pool, loadMap)
	require.Equal(t, map[int64]int{2: oursTierBackup, 3: oursTierBackup, 4: oursTierBackup, 5: oursTierFallback},
		openAIPlanPriorities(plan), "本组出池不得让备用顶替成档 1")

	scores := openAIPlanScores(plan)
	require.InDelta(t, scores[2], scores[3], 1e-9)
	require.InDelta(t, scores[2], scores[4], 1e-9)
	require.Greater(t, scores[2], scores[5])

	top2 := selectTopKOpenAICandidates(plan.candidates, 2)
	require.Equal(t, []int64{3, 4}, []int64{top2[0].account.ID, top2[1].account.ID}, "同档备用按 LoadRate 排序")
}

// ---------------------------------------------------------------- 挂钩 C：管理端诊断快照

func snapshotBaseScores(t *testing.T, accounts []*Account, groupID *int64) map[int64]float64 {
	t.Helper()
	ctx := OursWithSchedulerScoreGroup(context.Background(), groupID)
	weights := oursWeightsWithScoreGroup(ctx, GatewayOpenAIWSSchedulerScoreWeightsView{Priority: 10000, ErrorRate: 500})
	snapshot := buildOpenAIAccountSchedulerScoreSnapshot(accounts, nil, weights, false, 0)
	out := make(map[int64]float64, len(snapshot))
	for id, score := range snapshot {
		out[id] = score.BaseScore
	}
	return out
}

// 线上权重（W_priority=10000、W_error_rate=500）下管理端 scheduler_scores 的形态：本组 10500、其它本组账号 5500、兜底 500。
func TestBuildOpenAIAccountSchedulerScoreSnapshot_GroupContextMatchesFleetExpectation(t *testing.T) {
	withGroupTiering(t, true)

	roster := fleetRoster(0)
	for _, groupID := range fleetGroups() {
		scores := snapshotBaseScores(t, roster, int64Ptr(groupID))
		home := fleetHomeOf(groupID)
		for _, account := range roster {
			switch account.ID {
			case home:
				require.InDeltaf(t, 10500.0, scores[account.ID], 1e-6, "组 %d 本组", groupID)
			case fleetFallbackID:
				require.InDeltaf(t, 500.0, scores[account.ID], 1e-6, "组 %d 兜底", groupID)
			default:
				require.InDeltaf(t, 5500.0, scores[account.ID], 1e-6, "组 %d 备用 %d", groupID, account.ID)
			}
		}
	}
}

func TestBuildOpenAIAccountSchedulerScoreSnapshot_NoGroupContextHasNoPrimary(t *testing.T) {
	withGroupTiering(t, true)

	scores := snapshotBaseScores(t, fleetRoster(0), nil)
	for id, score := range scores {
		if id == fleetFallbackID {
			require.InDelta(t, 500.0, score, 1e-6)
			continue
		}
		require.InDeltaf(t, 10500.0, score, 1e-6, "无分组上下文时 8 个本组账号同档（无本组）：账号 %d", id)
	}
}

func TestBuildOpenAIAccountSchedulerScoreSnapshot_UnenrolledMatchesUpstream(t *testing.T) {
	withGroupTiering(t, true)

	accounts := []*Account{plainAccount(1, 10), plainAccount(2, 20), plainAccount(3, 998)}
	scores := snapshotBaseScores(t, accounts, int64Ptr(7))
	span := float64(998 - 10)
	for _, account := range accounts {
		want := 10000*(1-float64(account.Priority-10)/span) + 500
		require.InDeltaf(t, want, scores[account.ID], 1e-6, "账号 %d", account.ID)
	}
}

// 管理端按组算出的档位必须与真实路由一致（真实路由在本组健康时不降级）。
func TestOursSnapshotTiersMatchRoutingTiers(t *testing.T) {
	withGroupTiering(t, true)

	scheduler := openAITieringTestScheduler()
	rosterPointers := fleetRoster(0)
	roster := make([]Account, 0, len(rosterPointers))
	for _, account := range rosterPointers {
		roster = append(roster, *account)
	}
	for _, groupID := range append(fleetGroups(), 1, 2, 3, 4, 5) {
		routing := scheduler.oursRosterTiers(OpenAIAccountScheduleRequest{GroupID: int64Ptr(groupID)}, roster)
		ctx := OursWithSchedulerScoreGroup(context.Background(), int64Ptr(groupID))
		snapshot := oursSnapshotTiers(rosterPointers, oursWeightsWithScoreGroup(ctx, GatewayOpenAIWSSchedulerScoreWeightsView{}))
		require.Equalf(t, routing, snapshot, "组 %d：管理端档位与真实路由不一致", groupID)
	}
}

// 管理端 RateLimitService 入口：ctx 带组时按该组算本组。
func TestRateLimitServiceSchedulerScoreSnapshot_UsesGroupFromContext(t *testing.T) {
	withGroupTiering(t, true)

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{Priority: 10000, ErrorRate: 500}
	rateLimitService := &RateLimitService{cfg: cfg}
	roster := fleetRoster(0)

	scores := rateLimitService.BuildOpenAIAccountSchedulerScoreSnapshot(OursWithSchedulerScoreGroup(context.Background(), int64Ptr(62)), roster, nil)
	require.InDelta(t, 10500.0, scores[106].BaseScore, 1e-6, "组 62 的本组是 106")
	require.InDelta(t, 5500.0, scores[101].BaseScore, 1e-6)

	noGroup := rateLimitService.BuildOpenAIAccountSchedulerScoreSnapshot(context.Background(), roster, nil)
	require.InDelta(t, noGroup[106].BaseScore, noGroup[101].BaseScore, 1e-6, "无分组上下文时不设本组")
}
