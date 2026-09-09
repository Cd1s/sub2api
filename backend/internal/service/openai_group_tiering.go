package service

import (
	"os"
	"strings"
)

// 「登记制组内分档」补丁（fork 私有，不来自上游）。
//
// 目的：让 OpenAI 高级调度器在同一个分组内做到「本组优先 + 溢出均衡」：
//   - 本组专属账号永远排第一顺位；
//   - 本组不可用时，溢出请求在其余备用账号之间按当前负载/健康分散，而不是打满一个再换下一个；
//   - 只有显式登记过的账号才参与这套排法，未登记账号既不抢位置也不影响登记账号之间的相对顺序。
//
// 实现方式：不新增数据库字段，只在高级调度器内把候选的「调度用 priority」从全局
// accounts.priority 换成组内档位（见 openAITieredPriorities）。档位相同的账号打分相同，
// 上游现成的 tie-break 会落到 LoadRate 低者优先——这就是「按当前负载分散」。
//
// 池内没有任何登记账号时返回 nil，调用方回落上游原逻辑，行为与上游完全一致。

// 档位定义。数值越小越优先，且必须全部 > 0
// （openAICandidateOrderingPriority 用 0 判断「候选未参与打分循环」）。
const (
	oursTierPrimary    = 1 // 本组专属：登记账号中全局 priority 最小者（可并列）
	oursTierBackup     = 2 // 同档备用：其余登记账号，彼此平等
	oursTierFallback   = 3 // 兜底：登记账号中全局 priority 最大者
	oursTierUnenrolled = 4 // 未登记账号：排在所有登记账号之后
)

// oursGroupTieringCredentialKey 是账号级登记开关在 credentials 中的键名。
const oursGroupTieringCredentialKey = "ours_tiering"

// oursGroupTieringEnabled 是整体开关：环境变量 SUB2API_OURS_GROUP_TIERING 为
// "0"/"false"/"off" 时关闭分档，默认开启。进程启动时读一次；关闭后调度行为与上游
// 完全一致，用于紧急回滚（改 systemd unit 重启即可，不需要动数据）。
var oursGroupTieringEnabled = oursGroupTieringEnabledFromEnv(os.Getenv(oursGroupTieringEnvKey))

// oursGroupTieringEnvKey 是整体开关的环境变量名。
const oursGroupTieringEnvKey = "SUB2API_OURS_GROUP_TIERING"

func oursGroupTieringEnabledFromEnv(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	return v != "0" && v != "false" && v != "off"
}

// oursTieringEnrolled 判断账号是否登记参与分档。
// 登记标记为 credentials.ours_tiering，接受布尔 true 或字符串 "true"/"1"。
// 其它取值（含缺键、nil、数字、"yes"）一律视为未登记。
func oursTieringEnrolled(a *Account) bool {
	if a == nil || a.Credentials == nil {
		return false
	}
	switch v := a.Credentials[oursGroupTieringCredentialKey].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1"
	}
	return false
}

// openAITieredPriorities 只在池内存在登记账号时返回档位表：
// 登记账号中全局 priority 最小者 = 1（可并列）、最大者 = 3、其余 = 2；未登记账号 = 4。
// 登记账号 priority 全相等（min == max）时全部为 1；只有两个不同值时只会出现 1 和 3。
// 池内无登记账号、或整体开关关闭时返回 nil，调用方回落上游原逻辑。
//
// 注意：min/max 只在登记账号之间统计，未登记账号的 priority 不参与，
// 因此新加进分组但未登记的账号不会改变登记账号之间的档位。
func openAITieredPriorities(accounts []*Account) map[int64]int {
	if !oursGroupTieringEnabled {
		return nil
	}
	minPriority, maxPriority := 0, 0
	enrolled := 0
	for _, account := range accounts {
		if !oursTieringEnrolled(account) {
			continue
		}
		if enrolled == 0 {
			minPriority, maxPriority = account.Priority, account.Priority
		} else {
			if account.Priority < minPriority {
				minPriority = account.Priority
			}
			if account.Priority > maxPriority {
				maxPriority = account.Priority
			}
		}
		enrolled++
	}
	if enrolled == 0 {
		return nil
	}

	tiers := make(map[int64]int, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		switch {
		case !oursTieringEnrolled(account):
			tiers[account.ID] = oursTierUnenrolled
		case minPriority == maxPriority, account.Priority == minPriority:
			tiers[account.ID] = oursTierPrimary
		case account.Priority == maxPriority:
			tiers[account.ID] = oursTierFallback
		default:
			tiers[account.ID] = oursTierBackup
		}
	}
	return tiers
}

// openAICandidateTieredPriorities 从候选切片构造档位表，供真实路由挂钩点使用。
// 档位在「实际参与打分的候选池」内计算，与其后的 min-max 归一化保持同一集合。
func openAICandidateTieredPriorities(candidates []openAIAccountCandidateScore) map[int64]int {
	if len(candidates) == 0 {
		return nil
	}
	accounts := make([]*Account, 0, len(candidates))
	for i := range candidates {
		accounts = append(accounts, candidates[i].account)
	}
	return openAITieredPriorities(accounts)
}

// openAIAccountRosterTiers 在**分组完整名单**上计算档位表。
//
// 这一步必须发生在 failover 排除（req.ExcludedIDs）和运行时封禁
// （isOpenAIAccountRequestRuntimeBlocked：429 冷却、临时不可调度等）之前，
// 否则本组账号一旦出池，剩下的备用里 priority 最小的那个就会顶替成新的档 1，
// 溢出又会全压在它一个身上——正是本补丁要消灭的固定瀑布。
//
// 名单来自 listSchedulableAccounts()，它不随单次请求的失败重试变化。
func openAIAccountRosterTiers(roster []Account) map[int64]int {
	if !oursGroupTieringEnabled || len(roster) == 0 {
		return nil
	}
	// 先无分配地扫一遍：这个分组一个登记账号都没有就直接回落上游，
	// 不为它在每次请求上分配指针切片。
	enrolled := false
	for i := range roster {
		if oursTieringEnrolled(&roster[i]) {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return nil
	}
	accounts := make([]*Account, 0, len(roster))
	for i := range roster {
		accounts = append(accounts, &roster[i])
	}
	return openAITieredPriorities(accounts)
}

// openAIPlanTiers 优先用请求里预先算好的名单档位表；没有时（其它调用路径、
// 单测直接构造 plan）回落到候选池自算，行为仍然正确，只是本组出池后档位会重排。
func openAIPlanTiers(req OpenAIAccountScheduleRequest, candidates []openAIAccountCandidateScore) map[int64]int {
	if req.oursTiers != nil {
		return req.oursTiers
	}
	return openAICandidateTieredPriorities(candidates)
}

// openAISchedulingPriorityFor 是 openAIAccountSchedulingPriority 的分档版本：
// 有档位表且命中时返回档位，否则回落上游原函数。
func openAISchedulingPriorityFor(account *Account, tiers map[int64]int) int {
	if tiers != nil && account != nil {
		if tier, ok := tiers[account.ID]; ok {
			return tier
		}
	}
	return openAIAccountSchedulingPriority(account)
}

// openAICandidateOrderingPriority 返回候选在 tie-break 中应使用的 priority。
//
// 打分循环会把 candidate.priority 填成档位（或未分档时的 accounts.priority），
// tie-break 必须用它，否则同档备用会被全局 priority 打破平手、退回固定瀑布，
// 整个补丁静默失效。
//
// candidate.priority 为 0 表示该候选没有经过打分循环（例如测试里直接构造的候选，
// 或未来上游新增的旁路），此时回落 accounts.priority，与上游语义一致。
// 档位常量全部 > 0，因此分档路径不会走到这个回落分支。
func openAICandidateOrderingPriority(candidate openAIAccountCandidateScore) int {
	if candidate.priority != 0 {
		return candidate.priority
	}
	return openAIAccountSchedulingPriority(candidate.account)
}
