package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// enrolledAccount 构造一个已登记分档的账号。
func enrolledAccount(id int64, priority int) *Account {
	return &Account{
		ID:          id,
		Priority:    priority,
		Credentials: map[string]any{oursGroupTieringCredentialKey: true},
	}
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

// openAITieringTestScheduler 复刻线上权重：只有 priority 参与打分，
// 这样 plan 里的 score 就等于 10000 × priorityFactor，便于直接断言 pf。
func openAITieringTestScheduler() *defaultOpenAIAccountScheduler {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority: 10000,
	}
	return &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{cfg: cfg}}
}

func TestOursTieringEnrolled(t *testing.T) {
	withGroupTiering(t, true)

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
		"":      true,
		"1":     true,
		"true":  true,
		"on":    true,
		"0":     false,
		"false": false,
		"FALSE": false,
		" off ": false,
	}
	for raw, want := range cases {
		require.Equalf(t, want, oursGroupTieringEnabledFromEnv(raw), "SUB2API_OURS_GROUP_TIERING=%q", raw)
	}
}

func TestOpenAITieredPriorities(t *testing.T) {
	withGroupTiering(t, true)

	cases := []struct {
		name     string
		accounts []*Account
		want     map[int64]int
	}{
		{
			name:     "空切片",
			accounts: nil,
			want:     nil,
		},
		{
			name:     "全部未登记时不分档",
			accounts: []*Account{plainAccount(1, 10), plainAccount(2, 20), plainAccount(3, 998)},
			want:     nil,
		},
		{
			name:     "只有一个登记账号",
			accounts: []*Account{enrolledAccount(1, 10)},
			want:     map[int64]int{1: oursTierPrimary},
		},
		{
			name:     "两个登记账号不同 priority 只有档 1 和档 3",
			accounts: []*Account{enrolledAccount(1, 10), enrolledAccount(2, 998)},
			want:     map[int64]int{1: oursTierPrimary, 2: oursTierFallback},
		},
		{
			name: "多个登记账号：最小=1 最大=3 其余=2",
			accounts: []*Account{
				enrolledAccount(1, 10), enrolledAccount(2, 20),
				enrolledAccount(3, 30), enrolledAccount(4, 998),
			},
			want: map[int64]int{1: oursTierPrimary, 2: oursTierBackup, 3: oursTierBackup, 4: oursTierFallback},
		},
		{
			name:     "登记账号 priority 全相等则全部档 1",
			accounts: []*Account{enrolledAccount(1, 10), enrolledAccount(2, 10), enrolledAccount(3, 10)},
			want:     map[int64]int{1: oursTierPrimary, 2: oursTierPrimary, 3: oursTierPrimary},
		},
		{
			name: "两个并列最小同为档 1",
			accounts: []*Account{
				enrolledAccount(1, 10), enrolledAccount(2, 10),
				enrolledAccount(3, 30), enrolledAccount(4, 998),
			},
			want: map[int64]int{1: oursTierPrimary, 2: oursTierPrimary, 3: oursTierBackup, 4: oursTierFallback},
		},
		{
			name: "混合：未登记账号排档 4，且不影响登记账号的档位",
			accounts: []*Account{
				plainAccount(9, 1), // priority 比所有登记账号都小，仍不得成为档 1
				enrolledAccount(1, 10), enrolledAccount(2, 20),
				enrolledAccount(3, 998),
				plainAccount(8, 9999), // priority 比所有登记账号都大，仍不得影响档 3
			},
			want: map[int64]int{
				9: oursTierUnenrolled,
				1: oursTierPrimary,
				2: oursTierBackup,
				3: oursTierFallback,
				8: oursTierUnenrolled,
			},
		},
		{
			name:     "切片里的 nil 账号被忽略",
			accounts: []*Account{nil, enrolledAccount(1, 10), nil, enrolledAccount(2, 998)},
			want:     map[int64]int{1: oursTierPrimary, 2: oursTierFallback},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAITieredPriorities(tc.accounts))
		})
	}
}

// 整体开关关闭时永远不分档，行为回落上游。
func TestOpenAITieredPriorities_DisabledByEnv(t *testing.T) {
	withGroupTiering(t, false)
	accounts := []*Account{enrolledAccount(1, 10), enrolledAccount(2, 20), enrolledAccount(3, 998)}
	require.Nil(t, openAITieredPriorities(accounts))
}

func TestOpenAISchedulingPriorityFor(t *testing.T) {
	account := enrolledAccount(7, 42)

	// 无档位表：回落 accounts.priority。
	require.Equal(t, 42, openAISchedulingPriorityFor(account, nil))
	// 有档位表但未命中：同样回落。
	require.Equal(t, 42, openAISchedulingPriorityFor(account, map[int64]int{99: oursTierPrimary}))
	// 命中：返回档位。
	require.Equal(t, oursTierBackup, openAISchedulingPriorityFor(account, map[int64]int{7: oursTierBackup}))
	// nil 账号沿用上游语义返回 0。
	require.Equal(t, 0, openAISchedulingPriorityFor(nil, map[int64]int{7: oursTierBackup}))
}

// 挂钩点 D 保护：同档（priority 相同）时必须按 LoadRate 打破平手，
// 而不是退回全局 accounts.priority——否则同档备用会变成固定瀑布，补丁静默失效。
func TestIsOpenAIAccountCandidateBetter_SameTierBreaksByLoadRate(t *testing.T) {
	heavy := openAIAccountCandidateScore{
		account:  &Account{ID: 11, Priority: 20},
		loadInfo: &AccountLoadInfo{LoadRate: 80, WaitingCount: 0},
		score:    10,
		priority: oursTierBackup,
	}
	light := openAIAccountCandidateScore{
		account:  &Account{ID: 12, Priority: 70},
		loadInfo: &AccountLoadInfo{LoadRate: 10, WaitingCount: 0},
		score:    10,
		priority: oursTierBackup,
	}

	require.True(t, isOpenAIAccountCandidateBetter(light, heavy), "同档时在飞更少的账号更优")
	require.False(t, isOpenAIAccountCandidateBetter(heavy, light))

	// 不同档时仍然按档位排序，档位小的优先。
	primary := heavy
	primary.priority = oursTierPrimary
	require.True(t, isOpenAIAccountCandidateBetter(primary, light), "档位更小者优先于负载更低者")
}

// 挂钩点 D 回归：候选未经过打分循环（priority 为零值）时回落 accounts.priority，
// 与上游语义一致。保护 TestSelectTopKOpenAICandidates 等上游测试。
func TestIsOpenAIAccountCandidateBetter_UnscoredCandidateFallsBackToAccountPriority(t *testing.T) {
	low := openAIAccountCandidateScore{
		account:  &Account{ID: 11, Priority: 1},
		loadInfo: &AccountLoadInfo{LoadRate: 80},
		score:    10,
	}
	high := openAIAccountCandidateScore{
		account:  &Account{ID: 12, Priority: 2},
		loadInfo: &AccountLoadInfo{LoadRate: 10},
		score:    10,
	}
	require.True(t, isOpenAIAccountCandidateBetter(low, high), "未分档候选按全局 priority 排序")
	require.False(t, isOpenAIAccountCandidateBetter(high, low))
}

func openAIPlanPriorities(plan openAIAccountLoadPlan) map[int64]int {
	priorities := make(map[int64]int, len(plan.candidates))
	for _, candidate := range plan.candidates {
		priorities[candidate.account.ID] = candidate.priority
	}
	return priorities
}

// 挂钩点 B 保护（真实路由）：登记账号在 plan 中按档位打分。
func TestBuildOpenAIAccountLoadPlan_EnrolledAccountsUseTiers(t *testing.T) {
	withGroupTiering(t, true)

	filtered := []*Account{
		enrolledAccount(1, 10),  // 本组专属
		enrolledAccount(2, 20),  // 备用
		enrolledAccount(3, 30),  // 备用
		enrolledAccount(4, 998), // 兜底
	}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{}, filtered, map[int64]*AccountLoadInfo{},
	)

	require.Equal(t, map[int64]int{
		1: oursTierPrimary,
		2: oursTierBackup,
		3: oursTierBackup,
		4: oursTierFallback,
	}, openAIPlanPriorities(plan))

	// weights.priority = 10000 且其余权重为 0 ⇒ score = 10000 × priorityFactor。
	scores := openAIPlanScores(plan)
	require.InDelta(t, 10000.0, scores[1], 1e-6, "本组 pf = 1.0")
	require.InDelta(t, 5000.0, scores[2], 1e-6, "备用 pf = 0.5")
	require.InDelta(t, 5000.0, scores[3], 1e-6, "两个备用分数必须完全相同")
	require.InDelta(t, 0.0, scores[4], 1e-6, "兜底 pf = 0")
}

// 挂钩点 B 回归：池内没有登记账号时按原 accounts.priority 打分，与上游一致。
func TestBuildOpenAIAccountLoadPlan_UnenrolledPoolMatchesUpstream(t *testing.T) {
	withGroupTiering(t, true)

	filtered := []*Account{
		plainAccount(1, 10),
		plainAccount(2, 20),
		plainAccount(3, 30),
		plainAccount(4, 998),
	}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{}, filtered, map[int64]*AccountLoadInfo{},
	)

	require.Equal(t, map[int64]int{1: 10, 2: 20, 3: 30, 4: 998}, openAIPlanPriorities(plan))

	scores := openAIPlanScores(plan)
	span := float64(998 - 10)
	for _, account := range filtered {
		want := 10000 * (1 - float64(account.Priority-10)/span)
		require.InDeltaf(t, want, scores[account.ID], 1e-6, "账号 %d 的分数应与上游归一化结果一致", account.ID)
	}
}

// 混合池：未登记账号排档 4，登记账号之间的相对顺序不受影响。
func TestBuildOpenAIAccountLoadPlan_MixedPoolPutsUnenrolledLast(t *testing.T) {
	withGroupTiering(t, true)

	filtered := []*Account{
		enrolledAccount(1, 10),
		enrolledAccount(2, 20),
		enrolledAccount(3, 998),
		plainAccount(9, 5), // 全局 priority 最小，但未登记
	}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{}, filtered, map[int64]*AccountLoadInfo{},
	)

	require.Equal(t, map[int64]int{
		1: oursTierPrimary,
		2: oursTierBackup,
		3: oursTierFallback,
		9: oursTierUnenrolled,
	}, openAIPlanPriorities(plan))

	scores := openAIPlanScores(plan)
	require.Greater(t, scores[1], scores[2])
	require.Greater(t, scores[2], scores[3])
	require.Greater(t, scores[3], scores[9], "未登记账号必须排在所有登记账号之后")
}

// 整体开关关闭时，真实路由回落上游行为。
func TestBuildOpenAIAccountLoadPlan_DisabledFallsBackToUpstream(t *testing.T) {
	withGroupTiering(t, false)

	filtered := []*Account{
		enrolledAccount(1, 10),
		enrolledAccount(2, 20),
		enrolledAccount(3, 30),
		enrolledAccount(4, 998),
	}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{}, filtered, map[int64]*AccountLoadInfo{},
	)

	require.Equal(t, map[int64]int{1: 10, 2: 20, 3: 30, 4: 998}, openAIPlanPriorities(plan))
}

// 挂钩点 C 保护（管理端诊断快照）：scheduler_scores 与真实路由用同一套档位。
func TestBuildOpenAIAccountSchedulerScoreSnapshot_UsesTiers(t *testing.T) {
	withGroupTiering(t, true)

	accounts := []*Account{
		enrolledAccount(1, 10),
		enrolledAccount(2, 20),
		enrolledAccount(3, 30),
		enrolledAccount(4, 998),
		plainAccount(9, 5),
	}
	weights := GatewayOpenAIWSSchedulerScoreWeightsView{Priority: 10000}

	snapshot := buildOpenAIAccountSchedulerScoreSnapshot(accounts, nil, weights, false, 0)
	require.Len(t, snapshot, len(accounts))

	// 档位 1/2/2/3/4 在 [1,4] 区间归一化：1.0 / 2/3 / 2/3 / 1/3 / 0。
	require.InDelta(t, 10000.0, snapshot[1].BaseScore, 1e-6)
	require.InDelta(t, 10000.0*2/3, snapshot[2].BaseScore, 1e-6)
	require.InDelta(t, 10000.0*2/3, snapshot[3].BaseScore, 1e-6)
	require.InDelta(t, 10000.0/3, snapshot[4].BaseScore, 1e-6)
	require.InDelta(t, 0.0, snapshot[9].BaseScore, 1e-6)
}

// 挂钩点 C 回归：无登记账号时快照与上游一致。
func TestBuildOpenAIAccountSchedulerScoreSnapshot_UnenrolledMatchesUpstream(t *testing.T) {
	withGroupTiering(t, true)

	accounts := []*Account{plainAccount(1, 10), plainAccount(2, 20), plainAccount(3, 998)}
	weights := GatewayOpenAIWSSchedulerScoreWeightsView{Priority: 10000}

	snapshot := buildOpenAIAccountSchedulerScoreSnapshot(accounts, nil, weights, false, 0)
	span := float64(998 - 10)
	for _, account := range accounts {
		want := 10000 * (1 - float64(account.Priority-10)/span)
		require.InDeltaf(t, want, snapshot[account.ID].BaseScore, 1e-6, "账号 %d", account.ID)
	}
}

func TestOpenAIAccountRosterTiers(t *testing.T) {
	withGroupTiering(t, true)

	require.Nil(t, openAIAccountRosterTiers(nil))
	require.Nil(t, openAIAccountRosterTiers([]Account{*plainAccount(1, 10), *plainAccount(2, 20)}))

	roster := []Account{
		*enrolledAccount(1, 10),
		*enrolledAccount(2, 20),
		*enrolledAccount(3, 30),
		*enrolledAccount(4, 998),
		*plainAccount(9, 5),
	}
	require.Equal(t, map[int64]int{
		1: oursTierPrimary,
		2: oursTierBackup,
		3: oursTierBackup,
		4: oursTierFallback,
		9: oursTierUnenrolled,
	}, openAIAccountRosterTiers(roster))
}

func TestOpenAIPlanTiers_PrefersRequestRoster(t *testing.T) {
	withGroupTiering(t, true)

	rosterTiers := map[int64]int{1: oursTierPrimary, 2: oursTierBackup}
	req := OpenAIAccountScheduleRequest{oursTiers: rosterTiers}
	require.Equal(t, rosterTiers, openAIPlanTiers(req, nil))

	// 请求里没有名单档位表时回落候选池自算。
	candidates := []openAIAccountCandidateScore{
		{account: enrolledAccount(1, 10)},
		{account: enrolledAccount(2, 998)},
	}
	require.Equal(t, map[int64]int{1: oursTierPrimary, 2: oursTierFallback},
		openAIPlanTiers(OpenAIAccountScheduleRequest{}, candidates))
}

// 补丁的核心性质：本组账号被 failover 排除（或运行时封禁）出池后，
// 剩下的备用必须**保持同档、分数相同**，由 LoadRate 决定谁先上——
// 而不是让 priority 次小的备用顶替成新的档 1、把溢出全接走。
func TestBuildOpenAIAccountLoadPlan_RosterTiersSurviveExclusion(t *testing.T) {
	withGroupTiering(t, true)

	roster := []Account{
		*enrolledAccount(1, 10),  // 本组专属
		*enrolledAccount(2, 20),  // 备用
		*enrolledAccount(3, 30),  // 备用
		*enrolledAccount(4, 40),  // 备用
		*enrolledAccount(5, 998), // 兜底
	}
	req := OpenAIAccountScheduleRequest{oursTiers: openAIAccountRosterTiers(roster)}

	// 本组账号（ID=1）已被排除，池里只剩备用与兜底。
	pool := []*Account{&roster[1], &roster[2], &roster[3], &roster[4]}
	loadMap := map[int64]*AccountLoadInfo{
		2: {AccountID: 2, LoadRate: 90},
		3: {AccountID: 3, LoadRate: 5},
		4: {AccountID: 4, LoadRate: 80},
		5: {AccountID: 5},
	}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), req, pool, loadMap,
	)

	require.Equal(t, map[int64]int{
		2: oursTierBackup,
		3: oursTierBackup,
		4: oursTierBackup,
		5: oursTierFallback,
	}, openAIPlanPriorities(plan), "本组出池不得让备用顶替成档 1")

	scores := openAIPlanScores(plan)
	require.InDelta(t, scores[2], scores[3], 1e-9, "同档备用分数必须相同")
	require.InDelta(t, scores[2], scores[4], 1e-9, "同档备用分数必须相同")
	require.Greater(t, scores[2], scores[5], "兜底仍排在备用之后")

	// 分数相同 ⇒ tie-break 落到 LoadRate：线上 lb_top_k=2 时进 TopK 的是最空的两个备用
	// （ID=3 负载 5、ID=4 负载 80），负载最高的 ID=2（90）与兜底都进不去。
	top2 := selectTopKOpenAICandidates(plan.candidates, 2)
	require.Equal(t,
		[]int64{3, 4},
		[]int64{top2[0].account.ID, top2[1].account.ID},
		"同档备用按 LoadRate 排序，最空的先上",
	)
}

// 对照：如果档位表退回「候选池自算」，本组出池后 priority 次小的备用会变成档 1。
// 这条测试记录的是被我们避免掉的行为，一旦 req.oursTiers 传递链断了就会红。
func TestBuildOpenAIAccountLoadPlan_CandidatePoolTiersWouldPromoteBackup(t *testing.T) {
	withGroupTiering(t, true)

	pool := []*Account{enrolledAccount(2, 20), enrolledAccount(3, 30), enrolledAccount(4, 998)}
	plan := openAITieringTestScheduler().buildOpenAIAccountLoadPlan(
		context.Background(), OpenAIAccountScheduleRequest{}, pool, map[int64]*AccountLoadInfo{},
	)
	require.Equal(t, map[int64]int{
		2: oursTierPrimary,
		3: oursTierBackup,
		4: oursTierFallback,
	}, openAIPlanPriorities(plan))
}
