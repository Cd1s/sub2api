# PATCH-NOTES —— fork 私有调度补丁

> 本文件只存在于本 fork，**不会也不应该出现在上游 `Wei-Shaw/sub2api`**。
>
> **推送规则（硬性）**：只允许 `git push origin ...`。
> **禁止** `git push upstream`、`gh pr create`、`gh issue create`。

| 版本 | 基线 | 内容 |
|---|---|---|
| `v0.2.4-ours` | 上游 `v0.2.4`（`5de5e2be`） | 登记制组内分档（本组按 priority 推断） |
| `v0.2.4-ours.2` | 同上 | 本组改为显式标记；本组健康门控降级 + 防饿死探测；会话自动回家（取代外部定时器）；管理端快照按组；后台「加入号池动态调度」开关 |
| `v0.2.4-ours.3` | 同上 | 修复：调度快照缓存白名单漏了 `ours_*` 两个键，路由看不到登记（见挂钩点 G） |
| `v0.2.4-ours.4` | 同上 | 会话固定备用（rendezvous 哈希）+ 健康分档；健康判定加滞回；没有健康备用时本组不让位 |

分支：`ours/scheduler-patch`（**fork 的默认分支**）。`main` 只镜像上游，永远不放补丁。
不要点 GitHub 上的「Sync fork」按钮——它会把上游 merge 进补丁分支；同步上游一律用 `ours/rebase.sh`。

---

## 一、要解决的问题

一批专属账号各自服务固定的几个客户分组，另有一个公用兜底账号。目标：

1. **本组优先**：客户请求先用本组账号。
2. **溢出均衡**：本组不行时，溢出请求在所有其它备用账号之间按当前负载/健康分散，而不是「打满 backup1 再去 backup2」。
3. **显式登记才参与**：只作用于明确登记过的账号；未登记账号不抢位置、不改变别人排序；没有登记账号的分组行为与上游完全一致。
4. **所有专属账号给所有分组当备用**：每个分组的候选 = 全部专属账号 + 兜底，靠「本组标记」区分谁在这个组排第一。

上游只按全局 `accounts.priority` 在候选池内做 min-max 归一化，单一梯子无法同时表达「本组第一」和「备用之间平等」，更无法让同一个账号在自己的组排第一、在别人的组当平等备用。

---

## 二、档位规则

不新增数据库字段、不加迁移、不改写入 API。只在高级调度器内，把候选的「调度用 priority」换成**组内档位**，且只对登记账号生效。

两个账号级 credentials 键：

| 键 | 含义 | 写法 |
|---|---|---|
| `ours_tiering` | 是否登记参与动态调度 | 布尔 `true`，或字符串 `"true"` / `"1"` |
| `ours_home_groups` | 这个账号担任本组的分组 ID | 逗号分隔字符串 `"11,12,13"`；也接受 JSON 数组（元素为数字或数字字符串）。空串、空数组、任一元素非法 → 整条作废，等于没有标记 |

档位**按当前分组**计算（选号请求带着分组）：

| 档位 | 定义 |
|---|---|
| 1 | 登记账号中，`ours_home_groups` 包含当前分组的 |
| 3 | 除档 1 外的登记账号中，priority **严格最大且唯一**的（兜底）；最大值有并列时不设档 3 |
| 2 | 其余登记账号（同档备用，彼此平等） |
| 4 | 未登记账号（仅当池内有登记账号时） |

- **完全不用 priority 推断本组。** priority 只用来选兜底。
- 名单里没有账号声明当前分组（包括本组暂时掉出名单：429、过载、临时不可调度）时，该组本次**不设档 1**，整组流量在备用之间平摊，**不会有别的号顶替成本组**。
- 两个及以上登记账号声明同一组：都是档 1，该组不做会话回家。
- 没有分组上下文时按「无本组」算。
- 池内没有任何登记账号 → 档位表为 nil，全部回落原 `accounts.priority`，与上游逐字节一致。

同档账号打分相同，上游现成的 tie-break 落到 LoadRate 低者优先——这就是「按当前负载分散」。

**把所有专属账号的 priority 统一成同一个值**（兜底保持最大）不影响分档结果，但能让「关掉分档」时整组退化为「所有专属账号平摊、兜底最后」，而不是全压到 priority 最小的那个账号上。

---

## 三、路由档位（v0.2.4-ours.4 起按会话细分）

第二节是「按组」的基础档位；真实路由在它之上再按健康状态和会话细分：

| 路由档位 | 含义 |
|---|---|
| 1 | 本组（健康；或不健康但没有健康备用可让；或降级期间的探测轮次） |
| 2 | **该会话的固定备用**：健康备用里按「会话 × 账号」哈希选出，同一会话始终是同一个 |
| 3 | 其它健康备用 |
| 4 | 不健康的备用、让位中的本组 |
| 5 | 兜底 |
| 6 | 未登记 |

在 `W_priority=10000` 下各档之间至少差约 2000 分，远大于错误率项（最多 500 分），错误率只在同档内起作用。

**为什么要「会话固定备用」**（v0.2.4-ours.3 上线后的实测问题）：备用们同档、按错误率连续打分时，

- 24 个组的溢出全部涌向当时最健康的一两个号，把它压垮后再集体转移（每小时负载冠军都在换，上一小时扛最多的号下一小时常常失败率 90%）；
- 同一会话的相邻请求落到不同备用上：「备用 → 另一个备用」的换号占到后续请求的 15%（旧梯子为 0），上游缓存命中率从 89–94% 掉到 73–85%。

改成按会话哈希固定备用（rendezvous / 最高随机权重哈希）后：不同会话均匀分散到各备用（不再扎堆），同一会话一直落在同一个备用上（缓存保得住）；某个备用变得不健康时，只有原本选中它的会话会换到各自的下一个选择。没有会话标识的请求，健康备用同档。全部备用都不健康时（全局风暴），仍按哈希固定，不追着「最不差」的号跑。

**健康判定带滞回**：错误率超过 sticky_escape 的错误率阈值（默认 0.5）判为不健康，回落到「阈值 × 0.6」（0.3）以下才恢复，中间保持原状态。实测没有滞回时一小时降级 31 次、恢复 29 次。本组和备用用同一套规则（本组的状态切换打日志，备用不打）。

**没有健康备用时本组不让位**：本组不健康、但所有备用也不健康时，让位只会把会话推给同样不健康的号、白白丢缓存，所以本组保持第一。

## 三点五、本组健康门控降级 + 防饿死探测（v0.2.4-ours.2 起）

**问题**：本组领先备用 5000 分（`W_priority=10000` × pf 差 0.5），而错误率项最多只有 `W_error_rate=500` 分。本组 100% 失败时仍然约 99.98% 先打它，每个请求都要先失败一次才换号；绑在本组的会话因错误率逃逸后，负载均衡又会选回本组。

**做法**：本组账号的运行时错误率 **大于** sticky_escape 的错误率阈值（默认 0.5）、且存在健康备用时，本次选号让位（路由档位 4），由该会话的固定备用接手。

- 阈值复用 sticky_escape 的错误率阈值，不新增配置；sticky_escape 整体关闭时不降级。
- **只看错误率，不看首字延迟**：首字慢主要由请求本身决定（推理强度、上下文长度），换号不会变快，还会丢上游缓存。
- （v0.2.4-ours.2 时降级是与备用同档、由错误率挑；v0.2.4-ours.4 起改为让位给会话固定备用，见第三节。）
- 返回新 map，不改原始档位表。

**防饿死探测**：错误率 EWMA 只在有请求结果时更新，降级后没有流量，错误率会冻结在高位。所以被降级的本组**每第 N 次选号仍按档 1 参与一次**（默认 N=20，约 5%）。

- 按账号 ID 的原子计数器，确定、可测试，不用随机数。
- 本次请求已经排除了本组（failover 重试）时不计数，避免重试吃掉探测名额。
- 探测请求失败会走现有 failover——这是「坏账号修好后能被发现」的必要代价。

---

## 四、会话自动回家（取代外部粘性过期定时器）

**问题**：溢出或 failover 后绑到备用的会话，粘性层每次命中都会把 TTL 刷新成 1 小时，活跃会话会一直钉在备用上。以前靠一个外部定时器扫 Redis、删掉指向非本组账号超过 600 秒的粘性键；它直接删 Redis、自己维护状态文件、用 SQL 猜本组。现在写进源码，定时器退役。

**挂钩**：`Select()` 的会话粘性层之前。

**本组表（黏住版）**：每次负载均衡选号时，从分组完整名单里记录「谁声明了自己是本组」：

- 名单里看到它声明 → 记录或刷新；
- 名单里看到它但它不再声明（或退出登记）→ 立即删除；
- 它暂时掉出名单 → 记录保留；超过粘性 TTL（1 小时）没见到 → 删除；
- 恰好一个声明者时它就是本组；两个及以上视为冲突，不做回家；表里没有该组（例如刚重启还没有新会话）也不做。

**计时表**：`(分组, 会话)` → 首次发现它绑在非本组账号的时间，在回家检查时才记录。会话绑回本组时删除；只在备用之间换号不重置；超过粘性 TTL 没再见到的条目清掉；硬上限 20000 条；重启后重新计时。

**回家条件（全部满足）**：

1. 会话非空；当前绑定是非本组账号；不是 guardian 父子会话；不是「保留绑定」请求。
2. 离开本组的时长 ≥ `SUB2API_OURS_STICKY_RETURN_AFTER_SECONDS`（默认 600）。
3. 本组不在本次请求的排除名单里，且 `shouldEscapeStickyAccount(本组)` 为假（错误率和首字都不超阈值）。
4. 借用粘性层 `selectBySessionHash` 选本组（模型映射、传输、分组、隐私、配额、利润门等兼容性检查全部复用），且**真的拿到并发槽**；需要排队就放弃。
5. 本组此刻仍登记且仍声明该组（`ours_tiering` 写回 false 时回家立刻停）。

**成功**：把会话绑定直接改成本组（不走 profit gate 的「准入后绑定」，否则有门时会出现「请求落在本组、绑定仍指向备用」），删除计时条目，打 `ours_sticky_returned_home`。

**失败**（本组满、不兼容、不健康）：会话原样留在当前账号，**不许借机换到别的备用**（换号会丢上游缓存），下次请求再试。回家尝试**不会**产生 `sticky_escape_triggered` 日志（运营者靠它统计真实逃逸）。

**有意的取舍：不动 `previous_response_id` 链。** 这条链的上游状态在产生它的账号上，强行换号有接续失败的风险，链会随对话结束自然过期。这点与旧定时器不同（旧定时器也会删 10 分钟没动的 response 键）。上线前核实过：HTTP 路径上几乎没有请求带 `previous_response_id`，回家机制不受影响。

---

## 五、挂钩点清单

新增文件（全部新增逻辑都在这里）：

- `backend/internal/service/openai_group_tiering.go` —— 档位规则、本组标记解析、tie-break 取值
- `backend/internal/service/ours_health_home.go` —— 健康降级、探测、本组表、计时表、会话回家、快照按组
- 对应测试：`openai_group_tiering_test.go`、`ours_health_home_test.go`
- 前端：`frontend/src/components/account/__tests__/EditAccountModal.oursTiering.spec.ts`

改动的上游代码（行号为当前值，rebase 后以函数名为准）：

| 挂钩 | 位置 | 改动 |
|---|---|---|
| 请求结构体 | `openai_account_scheduler.go:93-94` `OpenAIAccountScheduleRequest` | 不导出字段 `oursTiers`、`oursQuietEscape` |
| **A 算档位** | `selectByLoadBalance()` `:1438` | `req.oursTiers = s.oursRosterTiers(req, accounts)`（在分组完整名单上、failover 排除之前） |
| **B 真实路由** | `buildOpenAIAccountLoadPlan()` `:906`，及其后两处 | `tiers := openAIPlanTiers(req, candidates)`；两处 `openAISchedulingPriorityFor(x, tiers)` |
| **C 诊断快照** | 权重视图结构体 `:2623` 字段 `oursGroupID`；`RateLimitService.BuildOpenAIAccountSchedulerScoreSnapshot` `:2660`；`buildOpenAIAccountSchedulerScoreSnapshot()` `:2705` | 管理端按组打分时把分组经 ctx → 权重视图带进快照；`tiers := oursSnapshotTiers(accounts, weights)` |
| **C′ 管理端** | `handler/admin/account_handler.go:563` `scoreGroupPool` | `service.OursWithSchedulerScoreGroup(ctx, groupID)` |
| **G 快照缓存白名单** | `repository/scheduler_cache.go:956` `filterSchedulerCredentials` | 白名单加 `ours_tiering`、`ours_home_groups`（分组名单从 `sched:meta:<id>` 读，漏了这两个键路由就看不到登记） |
| **D tie-break** | `isOpenAIAccountCandidateBetter()` `:698` | `openAICandidateOrderingPriority(left/right)` |
| **E 会话回家** | `Select()` `:452-454` | `if selection, ok := s.oursTryReturnHome(ctx, req, &decision); ok { return ... }` |
| **F 静默逃逸日志** | `selectBySessionHash()` `:565`、`:590` | `slog.Info("sticky_escape_triggered", …)` → `oursStickyEscapeInfo(req)("sticky_escape_triggered", …)` |
| 前端开关 | `EditAccountModal.vue` | 「加入号池动态调度」开关读写 `credentials.ours_tiering`；中英文案在 `i18n/locales/*/admin/accounts.ts` |

上游 Go 文件相对 v0.2.4-ours 净新增 5 行（`oursQuietEscape`、回家挂钩 3 行、`oursGroupID`），其余是单行替换。

### 挂钩点 G 的教训（v0.2.4-ours.3 修复）

调度快照缓存只保留一份白名单里的 credentials 键（`filterSchedulerCredentials`）。分组名单（`listSchedulableAccounts`）
从缓存的 `sched:meta:<id>` 读，所以 **v0.2.4-ours 与 v0.2.4-ours.2 在生产上，路由基本看不到 `ours_tiering` / `ours_home_groups`**：
分档、健康降级、本组表、会话回家只在缓存失效回源读库的少数请求上生效。管理端 `scheduler_scores` 走的是直接读库的路径，
照样显示正确，所以它**不能**用来证明路由已经生效；单测里的仓储也不经过这层缓存。

现在由仓储层单测 `TestOursSchedulerMetadata*` 守住白名单。上线后验证路由是否真的看到登记，看这两样：

- 缓存：`sched:meta:<id>` 里含 `ours_tiering` 与 `ours_home_groups` 两个键名（只数键名，不要打印值）；
- 行为：本组出错时出现 `ours_primary_demoted`，会话离开本组满 600 秒后出现 `ours_sticky_returned_home`。

白名单改动只在账号缓存被重写时生效（账号有写入、或快照重建）；部署后要让登记账号各被写一次，或等快照重建。

**没有碰的东西**：打分公式段、旧调度路径、`gateway_scheduling.go`、`openAIAccountRuntimeStat`、`account_groups.priority`、任何 settings 键、数据库迁移。

### 挂钩点 D 的历史说明

直接写 `left.priority < right.priority` 会打挂上游自带的 `TestSelectTopKOpenAICandidates`（它构造候选时只填 `account.Priority`）。所以经 `openAICandidateOrderingPriority()` 取值：候选 priority 为零值（未经打分循环）时回落 `account.Priority`。档位常量全部 > 0，分档路径永远走不到回落分支。

---

## 六、环境变量（进程启动时读一次）

| 变量 | 默认 | 含义 |
|---|---|---|
| `SUB2API_OURS_GROUP_TIERING` | 开 | `0`/`false`/`off` 时分档、降级、探测、回家**全部关闭**，行为回到上游原样 |
| `SUB2API_OURS_PRIMARY_PROBE_EVERY` | `20` | 被降级的本组每第 N 次选号保持档 1；`0` 关闭探测 |
| `SUB2API_OURS_STICKY_RETURN_AFTER_SECONDS` | `600` | 会话离开本组多久后尝试回家；`0` 关闭回家 |

非法值（非整数、负数）回落默认值。

## 七、日志事件（事件名固定，不要改）

| 事件 | 何时打 | 字段 |
|---|---|---|
| `ours_primary_demoted` | 本组进入降级（只在状态切换时打） | `account_id` `error_rate` `threshold` |
| `ours_primary_restored` | 本组退出降级（只在状态切换时打） | 同上 |
| `ours_sticky_returned_home` | 每次成功回家 | `group_id` `from_account_id` `to_account_id` `off_own_seconds` |

降级在管理端 `scheduler_scores` 里看不到（快照路径把错误率固定为 0），只能看日志。

---

## 八、怎么配置

| 我想做的事 | 用什么 |
|---|---|
| 让某账号参与动态调度 | 后台编辑账号，打开「加入号池动态调度」；或 `bulk-update` 写 `credentials.ours_tiering: true` |
| 让某账号退出 | 关掉开关；或写 `credentials.ours_tiering: false` |
| 指定某账号是哪些组的本组 | `bulk-update` 写 `credentials.ours_home_groups: "11,12,13"`（后台没有这个输入框） |
| 指定兜底 | 让它是登记账号里 priority 严格最大且唯一的 |
| 让某账号给某组当备用 / 不再当备用 | 把它绑进 / 移出该分组（`group_ids`） |
| 新加一个账号进某组、先观察不参与 | 什么都不做——不登记就排在最后 |
| 一键整体关回上游行为 | 环境变量 `SUB2API_OURS_GROUP_TIERING=0` 重启 |

「加入号池动态调度」和上游的「号池模式」（`pool_mode`）是两个独立的设置，互不影响。

写入命令模板（管理 API 认证头是 `x-api-key`，不是 Bearer；密钥只在服务器上从受限文件读取，不要打印）：

```bash
curl -sS -X POST -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  -d '{"account_ids":[<ID>],"credentials":{"ours_home_groups":"11,12,13"}}' \
  "$BASE/api/v1/admin/accounts/bulk-update"
```

`bulk-update` 对 `credentials` 做 **JSONB 按键合并**，不会清掉其它键；对 `group_ids` 是**全量替换**（先 GET 现值再合并）。每次写完逐个 GET 回读。

### 成功率怎么算（重要）

**不要用 access 日志里的 HTTP 状态码算成功率。** 流式请求在 failover 期间已经先回了 200，全部失败后才在流里报错，
access 日志会把这些失败记成 200（实测失败请求的 access 状态全是 200、耗时 52–124 秒）。正确口径：

```
成功率 = usage_logs 条数 /（usage_logs 条数 + ops_error_logs 中 status_code >= 400 且 error_type <> 'model_not_found' 的条数）
```

两张表按同一时间窗、同一组分组过滤；`ops_error_logs` 每条对应一个最终失败的请求（request_id 互不相同，且不会出现在 usage_logs 里）。

衡量缓存与会话稳定性：按 `usage_logs.session_id` 排序，统计相邻两次请求 `account_id` 不同的比例，以及其中「非本组 → 另一个非本组」的次数；
缓存命中率 = `sum(cache_read_tokens) / sum(input_tokens + cache_read_tokens)`。

### 验证分档是否生效

管理端账号列表默认**不返回**调度打分，需要带 `include_scheduler_score=true`。`scheduler_scores[]` 按分组给出 `base_score`，**没有 priority 字段**，档位只体现在分数上。线上权重（`W_priority=10000`、`W_error_rate=500`）下，一个分组内：

| 状态 | base_score 形态 |
|---|---|
| 未分档 | 连续梯子，备用们分数各不相同 |
| 已分档（本组 / 备用 / 兜底） | 本组 `10500`，其它登记账号**全部同一个值** `5500`，兜底 `500` |
| 没有本组标记的组（备用 / 兜底 / 未登记） | 备用 `10500`，兜底 `3833.33`，未登记 `500` |

管理端快照没有会话，看不到「会话固定备用」和健康降级（快照把错误率固定为 0）；这两者看日志与会话换号比例。

### credentials 写回路径（核实过，都保留 `ours_*` 键）

| 路径 | 结论 |
|---|---|
| OAuth token 刷新 | 新 token 与旧 credentials 合并，未涉及的键原样保留（`token_refresher.go` → `MergeCredentials`） |
| 后台编辑 | 前端整份展开回传（任何键、任何类型），后端只补回 token 类敏感键 |
| 账号测试 | 只读 token，写库只写 `extra` 列和状态列 |

**仍然要注意**：后台「重新授权」会整体替换 `credentials`；直接调 `PUT /accounts/{id}` 且带 `credentials` 时，未提交的非敏感键会被清空。动过账号后复查 `ours_tiering`、`ours_home_groups`、`model_mapping`。

---

## 九、运行约束

- **`lb_top_k` 保持 2、`weight_priority` 保持 10000。** 路由档位靠 `W_priority` 拉开差距；它若接近其它权重（上游默认约 1），档位差距会被负载、错误率等项淹没，TopK 大时加权随机近乎平均抽签。
- `weight_error_rate` 保持现值，降级靠它在同档之间挑健康的；**不要靠调大它来补救**，行为已经写在源码里。
- `sticky_weighted` 模式下（线上关闭）不做会话回家。
- WebSocket 只在建立连接走 `Select()` 时可能回家，连接内的轮次不重新选号。

---

## 十、部署与回滚

部署：跑前置守卫 → 上传二进制到临时名并核对 sha256 → 备份旧二进制 → 替换并恢复属主 → 重启 → 用管理 API `GET /api/v1/admin/system/version` 回读版本。

- 不要直接执行二进制加 `--version`：它不是标准 flag，会真的启动进程、抢端口和数据库连接。
- `GET /version` 会被前端 SPA 接管，拿不到后端版本。
- 本补丁零迁移，不需要备份数据库。

**外部粘性过期定时器已退役**：它的功能由本版源码接管，部署后必须 disable，否则两套机制同时挪会话、也分不清效果是谁的。只 disable，不删除脚本和状态目录，回滚要用。

### 回滚（顺序不能颠倒）

- **快速止血**：把登记账号写回 `ours_tiering=false`，秒级生效，不重启。若 priority 已统一，整组退化为「专属账号平摊、兜底最后」，不会压到单个账号。
- **完整回滚到旧梯子**：按快照逐个恢复每个账号的 `group_ids` 和 `priority` → 写 `ours_tiering=false` → 需要的话回滚二进制 → **最后**才能重开外部粘性过期定时器。
- 回滚到 `v0.2.4-ours` 或上游二进制、或设置了 `SUB2API_OURS_GROUP_TIERING=0` / `SUB2API_OURS_STICKY_RETURN_AFTER_SECONDS=0` 时，**必须同时恢复外部粘性过期定时器**，否则会话又会钉在备用上。
- **在「全量绑定 + priority 统一」状态下绝对不能重开旧定时器**：它按「组内 priority 最小、再取 id 最小」认本组，会把所有组的本组都认成同一个账号。

---

## 十一、跟随上游的 SOP

每次上游发新 tag，跑 `ours/rebase.sh vX.Y.Z`：

1. 同步 upstream、把 `main` 硬同步到 `upstream/main`；
2. **先看 `backend/migrations/` 的 diff**：上游迁移内嵌、启动时自动执行、没有 down 迁移。非空就停，人工拍板后加 `--migrations-reviewed` 重跑。这是唯一不能自动化的门；
3. `git rebase vX.Y.Z`；
4. **挂钩锚点检查**：逐个 grep 第五节的挂钩行（含次数），缺失或重复就停；
5. `go build` + `go test ./...` + 补丁单测 + 前端开关单测 + `golangci-lint`；
6. `ours/build-release.sh` 出 `sub2api_<版本>_linux_{arm64,amd64}.tar.gz` + `checksums.txt`（与上游同形态；二进制本体约 107MB，和上游一样，GitHub 上看到的 30 多 MB 是压缩后大小）；
7. 加 `--push` 才会推 fork 并打 tag。

### 冲突处理原则

**可以自行解决**：纯位置漂移、空白、import 顺序、上下文行变化；挂钩函数体内其它行被改但挂钩行形状不变。

**必须停下来人工确认**：

- 挂钩函数被删除、改签名或改语义（`Select` / `selectBySessionHash` / `selectByLoadBalance` / `buildOpenAIAccountLoadPlan` / `buildOpenAIAccountSchedulerScoreSnapshot` / `isOpenAIAccountCandidateBetter`）；
- `selectByLoadBalance` 的过滤循环被挪走，或 `listSchedulableAccounts` 返回语义变化；
- `OpenAIAccountScheduleRequest` 不再按值传递，或权重视图结构体被重写；
- `sticky_escape_triggered` 日志被改名或挪位（静默标志依赖它）；
- 管理端 `scoreGroupPool` 的打分调用方式变化；
- `openAIAccountCandidateScore` 字段改名（尤其 `priority`）；`Account.Credentials` 类型或访问方式变化；
- 打分公式段或 TopK / 加权随机逻辑变化；settings 键改名；
- `backend/migrations/` diff 非空。

**如果上游自己实现了同类功能**（读了 `account_groups.priority`、加了分组内优先级或本组概念）→ 放弃本补丁、采用上游实现，并在本文件记录。

---

## 十二、fork 上的 GitHub Actions（默认整体关闭）

上游 `.github/workflows/release.yml` 的触发条件是 `push: tags: 'v*'`，会匹配 `-ours` tag，在 fork 上跑完整发布（建 Release、推镜像到 fork 所有者名下）；上游 `backend-ci.yml` 是 `on: push`。因此 fork 的 Actions **整体关闭**，所有构建在本机做。

`.github/workflows/ours-release.yml` 是 fork 私有的最小工作流（`v*-ours*` tag 上跑测试并出两个架构的 tar.gz），带 `if: github.repository == 'Cd1s/sub2api'` 双保险。要启用，顺序不能反：

```bash
gh api -X PUT repos/Cd1s/sub2api/actions/permissions -F enabled=true
for w in release.yml backend-ci.yml security-scan.yml cla.yml; do
  gh api -X PUT "repos/Cd1s/sub2api/actions/workflows/$w/disable"
done
gh api repos/Cd1s/sub2api/actions/workflows --jq '.workflows[] | "\(.path)\t\(.state)"'
```

---

## 十三、单测护栏

| 测试 | 保护什么 |
|---|---|
| `TestOursGroupTiers_FullBindingEveryGroupHasOnlyItsHomeAsPrimary` | 全量绑定时 24 个组每组档 1 恰好是本组，其余备用档 2，兜底档 3 |
| `TestOursGroupTiers_HomeAbsentFromRosterHasNoPrimary` | 本组掉出名单时没有别的号顶替成档 1 |
| `TestOursGroupTiers_UnmarkedGroupHasNoPrimary` / `NoGroupContextHasNoPrimary` | 没有标记的组、没有分组上下文时不设档 1 |
| `TestOursGroupTiers_MaxPriorityTieHasNoFallback` / `UnifiedPrioritiesKeepTiers` | priority 并列无档 3；priority 统一后档位不变 |
| `TestOursParseHomeGroups` | 字符串 / 数组 / 空串 / 非法值解析 |
| `TestOursSnapshotTiersMatchRoutingTiers` / `TestRateLimitServiceSchedulerScoreSnapshot_UsesGroupFromContext` | 管理端按组的档位与真实路由一致（挂钩 C、C′） |
| `TestOursPrimaryHealth_*` | 降级阈值（小于 / 大于 / 等于）、只看错误率、滞回（0.5 进 / 0.3 出）、探测每 N 次 1 次、切换日志各一次、排除时不耗探测 |
| `TestOursRouteTiers_*` | 每个会话恰好一个稳定的固定备用、7000 个会话均匀分到 7 个备用（±15%）、备用变坏只挪它名下的会话、failover 排除后下一个选择确定、全部不健康时本组不让位且会话仍固定、固定备用领先其它账号超过错误率满分 |
| `TestOursRoute_UnhealthyHomeSessionPinsToOneBackupEndToEnd` | 端到端：本组不健康时同一会话连续请求落在同一个备用上 |
| `TestOursReturnHome_*` | 未满 600 秒不回、满足条件回家并改绑定、不健康 / 满 / 不支持模型时留在原备用且无逃逸日志、无唯一本组不做、previous_response 不变、开关关闭、撤销登记立刻停、profit gate 下绑定真的改成本组 |
| `TestOursOffHomeTable_ExpiryAndCap` / `TestOursGroupHomeTable_*` | 计时表过期与上限、本组表黏住语义 |
| `TestBuildOpenAIAccountLoadPlan_*` | 挂钩 A、B：真实路由按档打分，本组出池后备用仍同档 |
| `TestIsOpenAIAccountCandidateBetter_*` | 挂钩 D：同档按 LoadRate、零值回落 |
| `TestOursSchedulerMetadata*`（repository 包） | 挂钩 G：调度快照缓存保留 `ours_*`，且不放进 token |
| `EditAccountModal.oursTiering.spec.ts` | 前端开关读写 `ours_tiering`，不碰其它 credentials 键 |
