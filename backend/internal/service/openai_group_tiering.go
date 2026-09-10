package service

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
)

// 「登记制组内分档」补丁（fork 私有，不来自上游）。
//
// 目的：让 OpenAI 高级调度器在同一个分组内做到「本组优先 + 溢出均衡」：
//   - 本组账号永远排第一顺位；
//   - 本组不可用时，溢出请求在其余备用账号之间按当前负载/健康分散，而不是打满一个再换下一个；
//   - 只有显式登记过的账号才参与这套排法，未登记账号既不抢位置也不影响登记账号之间的相对顺序。
//
// 本组由账号级标记 credentials.ours_home_groups 显式声明，**完全不再用 priority 推断**。
// 这样同一个账号可以同时是自己几个分组的本组、其它所有分组的备用，
// 所有 Pro 账号可以绑进所有分组而不会让 priority 最小的那个吃掉全站流量。
//
// 实现方式：不新增数据库字段，只在高级调度器内把候选的「调度用 priority」从全局
// accounts.priority 换成组内档位。oursGroupTiers 是按组的基础档位（管理端快照也用它）；
// 真实路由在其上按健康状态和会话再细分（见 ours_health_home.go 的 oursRouteTiers）：
// 本组不可用时，每个会话固定落到一个健康备用上，不同会话均匀分散。
//
// 池内没有任何登记账号时返回 nil，调用方回落上游原逻辑，行为与上游完全一致。

// 档位定义。数值越小越优先，且必须全部 > 0
// （openAICandidateOrderingPriority 用 0 判断「候选未参与打分循环」）。
const (
	oursTierPrimary    = 1 // 本组：登记账号中声明了当前分组的
	oursTierBackup     = 2 // 同档备用：其余登记账号，彼此平等
	oursTierFallback   = 3 // 兜底：非本组登记账号中 priority 严格最大且唯一者
	oursTierUnenrolled = 4 // 未登记账号：排在所有登记账号之后
)

const (
	// oursGroupTieringCredentialKey 是账号级登记开关在 credentials 中的键名。
	oursGroupTieringCredentialKey = "ours_tiering"
	// oursHomeGroupsCredentialKey 声明该账号担任本组的分组 ID 列表。
	oursHomeGroupsCredentialKey = "ours_home_groups"
)

// oursGroupTieringEnvKey 是整体开关的环境变量名。
const oursGroupTieringEnvKey = "SUB2API_OURS_GROUP_TIERING"

// oursGroupTieringEnabled 是整体开关：环境变量为 "0"/"false"/"off" 时关闭分档、
// 健康降级、探测和会话回家，默认开启。进程启动时读一次；关闭后调度行为与上游
// 完全一致，用于紧急回滚（改 systemd unit 重启即可，不需要动数据）。
var oursGroupTieringEnabled = oursGroupTieringEnabledFromEnv(os.Getenv(oursGroupTieringEnvKey))

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

// oursParseHomeGroups 解析 credentials.ours_home_groups。
//
// 接受三种写法：
//   - 逗号分隔的字符串 "8,9,17"（元素两侧空白忽略）；
//   - JSON 数组字符串 "[8,9,17]" 或 "[\"8\",\"9\"]"；
//   - 已解码的数组（元素为数字或数字字符串，含 json.Number）。
//
// 空串、空数组、任一元素非法（非整数、≤0、无法解析）时返回 nil，
// 即「整条标记作废」，等同于没有标记——宁可不当本组，也不按半条错误配置去调度。
func oursParseHomeGroups(value any) []int64 {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil
		}
		if strings.HasPrefix(s, "[") {
			var items []any
			decoder := json.NewDecoder(strings.NewReader(s))
			decoder.UseNumber()
			if err := decoder.Decode(&items); err != nil {
				return nil
			}
			return oursParseHomeGroupItems(items)
		}
		parts := strings.Split(s, ",")
		items := make([]any, 0, len(parts))
		for _, part := range parts {
			items = append(items, part)
		}
		return oursParseHomeGroupItems(items)
	case []any:
		return oursParseHomeGroupItems(v)
	case []string:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return oursParseHomeGroupItems(items)
	case []int64:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return oursParseHomeGroupItems(items)
	case []int:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return oursParseHomeGroupItems(items)
	}
	return nil
}

func oursParseHomeGroupItems(items []any) []int64 {
	if len(items) == 0 {
		return nil
	}
	out := make([]int64, 0, len(items))
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		id, ok := oursParseHomeGroupID(item)
		if !ok {
			return nil
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func oursParseHomeGroupID(item any) (int64, bool) {
	var id int64
	switch v := item.(type) {
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		id = parsed
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		id = parsed
	case float64:
		if v != math.Trunc(v) || v > math.MaxInt64 || v < math.MinInt64 {
			return 0, false
		}
		id = int64(v)
	case int:
		id = int64(v)
	case int64:
		id = v
	default:
		return 0, false
	}
	if id <= 0 {
		return 0, false
	}
	return id, true
}

// oursDeclaresHomeGroup 判断账号是否声明自己是 groupID 的本组。
func oursDeclaresHomeGroup(a *Account, groupID int64) bool {
	if a == nil || a.Credentials == nil || groupID <= 0 {
		return false
	}
	raw, ok := a.Credentials[oursHomeGroupsCredentialKey]
	if !ok {
		return false
	}
	for _, id := range oursParseHomeGroups(raw) {
		if id == groupID {
			return true
		}
	}
	return false
}

// oursGroupTiers 计算某个分组内的档位表：
//
//	档 1：登记账号中，ours_home_groups 包含当前分组的；
//	档 3：除档 1 外的登记账号中，priority 严格最大且唯一的（兜底）；最大值有并列时不设档 3；
//	档 2：其余登记账号；
//	档 4：未登记账号。
//
// groupID 为 nil（没有分组上下文）时按「无本组」处理，不设档 1。
// 名单里没有账号声明当前分组（本组暂时掉出名单：429、过载、临时不可调度）时，
// 该组本次同样不设档 1——整组流量在备用之间平摊，**不会有别的号顶替成本组**。
// 池内没有任何登记账号、或整体开关关闭时返回 nil，调用方回落上游原逻辑。
func oursGroupTiers(accounts []*Account, groupID *int64) map[int64]int {
	if !oursGroupTieringEnabled {
		return nil
	}
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}

	type accountTierInput struct {
		enrolled bool
		home     bool
	}
	inputs := make([]accountTierInput, len(accounts))
	anyEnrolled := false
	fallbackPriority, fallbackCount, hasFallback := 0, 0, false
	for i, account := range accounts {
		if !oursTieringEnrolled(account) {
			continue
		}
		anyEnrolled = true
		home := gid > 0 && oursDeclaresHomeGroup(account, gid)
		inputs[i] = accountTierInput{enrolled: true, home: home}
		if home {
			continue
		}
		switch {
		case !hasFallback || account.Priority > fallbackPriority:
			fallbackPriority, fallbackCount, hasFallback = account.Priority, 1, true
		case account.Priority == fallbackPriority:
			fallbackCount++
		}
	}
	if !anyEnrolled {
		return nil
	}

	tiers := make(map[int64]int, len(accounts))
	for i, account := range accounts {
		if account == nil {
			continue
		}
		input := inputs[i]
		switch {
		case !input.enrolled:
			tiers[account.ID] = oursTierUnenrolled
		case input.home:
			tiers[account.ID] = oursTierPrimary
		case hasFallback && fallbackCount == 1 && account.Priority == fallbackPriority:
			tiers[account.ID] = oursTierFallback
		default:
			tiers[account.ID] = oursTierBackup
		}
	}
	return tiers
}

// openAIPlanTiers 优先用请求里预先算好的名单档位表（含健康降级）；没有时
// （其它调用路径、单测直接构造 plan）回落到候选池按同一规则自算。
func openAIPlanTiers(req OpenAIAccountScheduleRequest, candidates []openAIAccountCandidateScore) map[int64]int {
	if req.oursTiers != nil {
		return req.oursTiers
	}
	if len(candidates) == 0 {
		return nil
	}
	accounts := make([]*Account, 0, len(candidates))
	for i := range candidates {
		accounts = append(accounts, candidates[i].account)
	}
	return oursRouteTiersFromBase(oursGroupTiers(accounts, req.GroupID))
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
