# PATCH-NOTES —— fork 私有调度补丁「登记制组内分档」

> 本文件只存在于 fork `Cd1s/sub2api`，**不会也不应该出现在上游 `Wei-Shaw/sub2api`**。
>
> **推送规则（硬性）**：只允许 `git push origin ...`。
> **禁止** `git push upstream`、`gh pr create`、`gh issue create`。

基线：上游 `v0.2.4`（commit `5de5e2be`）。分支：`ours/scheduler-patch`。

---

## 一、这个补丁解决什么

8 个专属 ChatGPT 账号各服务固定的几个客户分组（24 组），外加 1 个公用兜底账号 monocle。需要：

1. **本组优先**：客户请求永远先用本组专属账号。
2. **溢出均衡**：专属账号不行时，溢出请求在其余备用账号之间按当前负载/健康分散，而不是「把 backup1 打满再去 backup2」。
3. **显式登记才参与**：只作用于明确登记过的账号；未登记账号不抢位置、不改变别人排序；一个组里没有任何登记账号时行为与上游完全一致。

上游只按全局 `accounts.priority`（小者优先）在候选池内做 min-max 归一化，单一梯子无法同时表达「本组第一」和「备用之间平等」：`lb_top_k=2` 时溢出 ~99% 落在 pf 最高的那个备用上；放大 K 又会把专属账号的第一顺位概率打到 ≤66%。

**做法**：不新增数据库字段、不加迁移、不改写入 API、不改前端。只在高级调度器内，把候选的「调度用 priority」从全局数值换成**组内档位**，且只对登记账号生效。

| 档位 | 定义 | 含义 |
|---|---|---|
| 1 | 登记账号中池内全局 `priority` **最小**者（可并列） | 本组专属 |
| 2 | 其余登记账号 | 同档备用（彼此平等） |
| 3 | 登记账号中池内全局 `priority` **最大**者（当 max ≠ min） | 兜底 monocle |
| 4 | **未登记**账号（仅当池内有登记账号时） | 排最后 |

登记标记：`account.credentials.ours_tiering == true`（也接受字符串 `"true"` / `"1"`）。
池内没有任何登记账号 → 不分档，全部用原 `accounts.priority`，与上游逐字节一致。

同档备用打分打平后，上游现成的 tie-break 落到 **LoadRate 低者优先**——这就是「按当前负载分散」。

---

## 二、补丁范围（挂钩点清单）

新增 1 个文件 + 在上游文件里动 10 行（其中 4 行是纯新增），无签名变更、无重命名、无重排。

### 新增文件

- `backend/internal/service/openai_group_tiering.go` —— 全部新增逻辑都在这里：
  - `oursGroupTieringEnabled` / `oursGroupTieringEnabledFromEnv()` —— 整体环境变量开关
  - `oursTieringEnrolled(*Account) bool` —— 账号级登记判定
  - `openAITieredPriorities([]*Account) map[int64]int` —— 档位表（无登记账号返回 nil）
  - `openAIAccountRosterTiers([]Account)` —— 在**分组完整名单**上算档位表（见挂钩点 A）
  - `openAICandidateTieredPriorities([]openAIAccountCandidateScore)` —— 从候选切片构造档位表（回落用）
  - `openAIPlanTiers(req, candidates)` —— 优先用名单档位表，没有才自算
  - `openAISchedulingPriorityFor(*Account, tiers)` —— 有档位用档位，否则回落 `openAIAccountSchedulingPriority`
  - `openAICandidateOrderingPriority(candidate)` —— tie-break 用的 priority（见挂钩点 D）
- `backend/internal/service/openai_group_tiering_test.go` —— 保护三个挂钩点的单测

### 改动的上游代码（行号为打完补丁后的当前值，rebase 后以**函数名**为准）

全部在 `backend/internal/service/openai_account_scheduler.go`：

| 挂钩 | 位置 | 改动 |
|---|---|---|
| **A（算档位表）** | `OpenAIAccountScheduleRequest` 加不导出字段 `oursTiers map[int64]int`（`:93`）；`selectByLoadBalance()`（`:1393`）在过滤循环之前 `:1434` 加一行 `req.oursTiers = openAIAccountRosterTiers(accounts)` | 让档位表建在**分组完整名单**上 |
| **B（真实路由）** | `buildOpenAIAccountLoadPlan()`（`:851`），改动在 `:902`、`:903`、`:911` | 新增 `tiers := openAIPlanTiers(req, candidates)`；两处 `openAIAccountSchedulingPriority(x)` → `openAISchedulingPriorityFor(x, tiers)` |
| **C（诊断快照）** | `buildOpenAIAccountSchedulerScoreSnapshot()`（`:2669`），改动在 `:2700`、`:2701`、`:2705` | 新增 `tiers := openAITieredPriorities(accounts)`；同样两处替换 |
| **D（tie-break）** | `isOpenAIAccountCandidateBetter()`（`:690`），改动在 `:694-695` | `left.account.Priority` → `openAICandidateOrderingPriority(left)`（右侧同理） |

**挂钩点 D 是最容易被忽略的一处**：不改它，同档备用会按全局 `priority` 打破平手、退回固定瀑布，整个补丁**静默失效且不报错**。

### 挂钩点 A 为什么必须存在（设计稿漏了这一条）

设计稿只说在 `buildOpenAIAccountLoadPlan` 里从候选池算档位。**照那样做，需求 2 不成立**，实测（9 账号、`lb_top_k=2`、20000 次抽样）：

| 场景 | 只按候选池算档位 | 按完整名单算档位（当前实现） |
|---|---|---|
| 正常 | 本组 99.97% ✅ | 本组 99.97% ✅ |
| 本组被 failover 排除 | **acct2 独吞 99.97%** ❌ | 负载最低的两个备用各 ~50% ✅ |

原因：`buildOpenAIAccountLoadPlan` 拿到的候选池**已经过滤过** —— `selectByLoadBalance()` 的过滤循环
（`openai_account_scheduler.go:1436` 起）先剔掉 `req.ExcludedIDs`、不可调度、运行时封禁（429 冷却、
临时不可调度）的账号。本组账号一出池，剩下备用里 `priority` 最小的那个立刻变成新的「池内最小值」＝档 1，
溢出又全压在它一个身上，正是这个补丁要消灭的固定瀑布。

所以档位表必须在过滤**之前**算好，并随 `req` 带下去。`listSchedulableAccounts()` 返回的名单不随单次
请求的失败重试变化，是正确的锚点。`openAIPlanTiers()` 在 `req.oursTiers` 为空时回落候选池自算，
所以其它调用路径和直接构造 plan 的单测都不受影响。

护栏：`TestBuildOpenAIAccountLoadPlan_RosterTiersSurviveExclusion`（本组出池后备用必须同档同分）
与 `TestBuildOpenAIAccountLoadPlan_CandidatePoolTiersWouldPromoteBackup`（记录被避免掉的那个行为）。
rebase 时如果 `req.oursTiers` 的传递链断了，前者会红。

**没有碰的东西**：打分公式段（`openai_account_scheduler.go:980-1060` 附近，7 个月被独立改过 7 次）、`openai_gateway_scheduling.go` 旧路径、`gateway_scheduling.go`、`account_groups.priority` 列、`BindGroups`、任何 DTO/handler/前端、任何 settings 键、数据库迁移。

### 挂钩点 D 的实现与原始设计稿的一处偏差（重要）

设计稿写的是直接改成 `left.priority < right.priority`。**照字面写会打挂上游自带的测试**
`backend/internal/service/openai_account_scheduler_test.go:3337 TestSelectTopKOpenAICandidates`
（该测试构造候选时只填了 `account.Priority`，没填候选结构体的 `priority` 字段；全为零值后 tie-break 会掉到 LoadRate，断言 `expected 13, actual 11` 失败）。

因此实际实现改为经 `openAICandidateOrderingPriority()` 取值：

```go
func openAICandidateOrderingPriority(candidate openAIAccountCandidateScore) int {
    if candidate.priority != 0 {
        return candidate.priority
    }
    return openAIAccountSchedulingPriority(candidate.account)
}
```

- 档位常量全部 > 0，所以**分档路径永远走不到回落分支**，需求 1/2/3 的行为与设计稿完全一致。
- 未分档时 `candidate.priority == account.Priority`，回落值与原值相同，对上游行为零影响。
- 好处：**不需要修改任何上游测试文件**，rebase 时冲突面更小。

已核对全部 `isOpenAIAccountCandidateBetter` 调用点（`openai_account_scheduler.go:665`、`:714`、`:725`、`:734`、`:1094`），候选都来自 `plan.candidates`，`priority` 均已在打分循环里赋值。
`staleSnapshotCompactRetry` 那批候选的 `priority` 确实是零值，但它们走的是自己的比较器
`sortOpenAICompactRetryCandidates()`（`:1122`），不经过本挂钩点。

---

## 三、怎么配置

上游后台**没有** `ours_tiering` 的点选界面；登记/退出走下面那条 API（一行命令）。要能点是另一个独立的小前端补丁，本次不做。

| 我想做的事 | 用什么 |
|---|---|
| 让某账号参与分档 | `bulk-update` 写 `credentials.ours_tiering: true` |
| 让某账号退出分档（回到上游排法） | 写 `credentials.ours_tiering: false` |
| 新加一个账号进某组、先观察不参与 | 什么都不做——不登记就不参与，它在该池里排第 4 档（最后），`base_score` 会落到该组最低（见第五节的验证方法） |
| 让某账号给某组当备用 / 不再当备用 | 上游后台把它勾进 / 勾出该分组（`group_ids`，本来就有） |
| 指定谁是某组的「本组账号」 | 让它是该组**登记账号**中全局 `priority` 最小的（现状即如此） |
| 指定谁是「兜底」 | 让它是该组登记账号中 `priority` 最大的（monocle 998） |
| 让两个账号并列做某组的本组账号 | 给它们相同且最小的 `priority`（都成档 1，均分） |
| 一键整体关回上游行为 | 环境变量 `SUB2API_OURS_GROUP_TIERING=0` 重启 |

### 登记命令（上线唯一要做的配置）

`POST /api/v1/admin/accounts/bulk-update` 对 `credentials` 做 **JSONB key 级合并**，不会清掉 `model_mapping` 等其它键：

```json
{"account_ids":[1860,1863,1864,1865,1866,1867,1868,1869,1872],"credentials":{"ours_tiering":true}}
```

写后**逐账号 GET 回读**，确认两件事：
1. `credentials.ours_tiering == true`
2. `credentials.model_mapping` 的键数没变

### 已知坑（每次动过账号都要复查）

- 后台对账号做**「重新授权」会整体替换 `credentials`** —— `ours_tiering` 会和 `model_mapping` 一起丢。
- `PUT /api/v1/admin/accounts/{id}` 带 `credentials` 时，**未提交的非敏感键会被清空**（`model_mapping` 就因此丢过）。

→ **在后台改过任何账号之后，复查该账号的 `ours_tiering` 与 `model_mapping`。**

---

## 四、运行约束

- **`lb_top_k` 必须保持 2。** K 放大到池大小时，monocle 的 pf=0 会把归一化下限拉到 0，加权随机权重变成 `10001` vs `5001×N`，本组账号的第一顺位概率会从 ~99.98% 掉到约 22%。
- 其余 settings 保持现状，补丁不需要改任何一项：
  `openai_advanced_scheduler_enabled=true`、`weight_priority=10000`、`weight_error_rate=500`、`weight_load=0`、`weight_ttft=0`。
- 8 个专属账号的全局 priority 梯子（10/20/30/40/50/60/70/80）保持不变——它仍然定义「谁是本组」。monocle 保持 998。
- 24 个分组的 `group_ids` 保持不变。

### 现有配置下的推演

- **有多个备用的组**：本组 pf=1.0 → 10000；备用 pf=0.5 → 5000；monocle pf=0 → 0。TopK=2 = {本组, 当前最空的那个备用}，加权随机权重 5001 : 1 → 本组第一顺位 ≈99.98%。
- **本组失败被 failover 排除后**：档位表来自完整名单，备用仍然全是档 2 → 重新归一化后 pf 相等 → TopK = 在飞最少的两个备用，50/50；有错误的备用 `errorFactor` 低 → 分数低 → 不进 TopK。负载与健康两个维度都起作用。（实测 20000 次抽样：负载 5% 与 80% 的两个备用各占 ~50%，负载 90% 的那个 0%。）
- **只有 {本组, monocle} 的组**：档 1/3 → pf 1.0/0 → 与现状完全一致。
- **池内无登记账号的组**：`tiers == nil` → 全部走原 priority → 与上游逐字节一致。

---

## 五、部署与回滚

部署流程：跑 origin guard → 上传到临时名并核 sha256 → 备份旧二进制 → 换二进制（`chown sub2api:sub2api`）→ 重启 → 回读版本 → 跑验收脚本。

几个实测过的坑：

- **不要跑 `/opt/sub2api/sub2api --version`**：它不是标准 flag，会真的启动进程、抢 8080 端口和数据库连接。
  回读版本用管理 API：`GET /api/v1/admin/system/version`（带 `x-api-key`）→ `{"version":"0.2.4-ours"}`。
  `GET /version` 会被前端 SPA 接管，拿不到后端版本；启动日志里也没有版本横幅。
- **unit 里有 `ExecStartPre=+/usr/local/sbin/sub2api-origin-guard --prestart`**：它查 Cloudflare，确认三个域名 A 记录都指向本机，
  任一对不上就 exit 1、服务起不来。换二进制前先单独跑一次确认放行，否则会误判成补丁的问题。
- **unit 是 `User=sub2api`**：`sshctl put` 上传的文件属主是 root，换完要 `chown sub2api:sub2api` 恢复原状。
- 管理 API 认证头是 `x-api-key`（`Authorization: Bearer` 会 401）；密钥文件在 `/etc/sub2api-tg-bot/sub2api-admin-key`，
  只在服务器上用 `$(tr -d "\n" < 文件)` 引用，不要打印。
- 数据库是容器里的 Postgres（`sub2api-containers.service` 只是 `podman start sub2api-postgres sub2api-redis`），
  本补丁零迁移，不需要备份数据库。

回滚（任选其一，**数据都不需要改**）：

1. 换回上游二进制；
2. systemd unit 加 `Environment=SUB2API_OURS_GROUP_TIERING=0` 后重启（进程启动时读一次）；
3. 把 9 个账号的 `credentials.ours_tiering` 改为 `false`。

### 验证补丁是否生效

管理端账号列表**默认不返回**调度打分，必须带 `include_scheduler_score=true`：

```bash
K=$(tr -d "\n" < /etc/sub2api-tg-bot/sub2api-admin-key)
curl -sS -H "x-api-key: $K" \
  "http://127.0.0.1:8080/api/v1/admin/accounts?include_scheduler_score=true&platform=openai&page=1&page_size=100"
```

每个账号的 `scheduler_scores[]` 按分组给出 `{group_id, base_score, sticky_score}`。
**注意：这里没有 priority 字段，档位不会以 1/2/3/4 的数字形式出现**，只体现在 `base_score` 上。

判读方法：取同一个 `group_id` 下所有账号的 `base_score` 排序看**形态**。
线上权重（`weight_priority=10000`、`weight_error_rate=500`、其余 0）下，快照里 `errorFactor` 固定为 1，
所以 `base_score = 10000 × pf + 500`：

| 状态 | 同组 base_score 形态 |
|---|---|
| **未分档**（无登记账号 / 开关关闭） | 连续梯子，备用们分数**各不相同**（等差递减） |
| **已分档**，组内有 1/2/3 档 | 本组 `10500`，备用们**全部同一个值** `5500`，兜底 `500` |
| **已分档**，组内有 1/2/3/4 档 | 本组 `10500`，备用 `7166.67`，兜底 `3833.33`，未登记 `500` |

备用们的分数从各不相同**塌缩成同一个值**，就是补丁生效的铁证；某个账号落在该组最低，
且它不是兜底，就是漏登记了。这比看命中率快得多，不受上游风暴影响，不用等流量。

#### 生产实测（2026-09-10，Oracle-SGwest-arm）

组 12（本组 1868），登记前后：

| 账号 | priority | 登记前 | 登记后 |
|---|---|---|---|
| 1868 | 30 | 10500.00 | 10500 |
| 1860 | 40 | 10396.69 | **5500** |
| 1866 | 50 | 10293.39 | **5500** |
| 1865 | 60 | 10190.08 | **5500** |
| 1864 | 70 | 10086.78 | **5500** |
| 1863 | 80 | 9983.47 | **5500** |
| 1872 | 998 | 500.00 | 500 |

登记前那列可以手算核对：`pf(40) = 1 − (40−30)/968 = 0.98967` → `10000 × 0.98967 + 500 = 10396.7`，
证明**未登记时新二进制的打分与上游逐字节一致**。

组 2 验证了第 4 档：未登记的 1752 (Nube) 全局 priority=10，与本组 1869 相同，
登记后仍落到 `500`（最后），而 1869 为 `10500`、两个备用同为 `7166.67`、兜底 `3833.33`。

#### 验收脚本的窗口陷阱

`/usr/local/sbin/sub2api-cascade-verify-daily` 的 current 窗口是「此刻往前 3 小时」。
**刚部署或刚登记完就跑，报告里几乎全是旧行为的数据**，测不到补丁。
要等补丁跑满一个窗口（3 小时）再看 `own_hit_pct` 与 `distinct_backup_accounts`。
上线当天就用上面的 `base_score` 形态做即时验证。

---

## 六、跟随上游的 SOP

分支约定：

- `main` —— 只镜像上游，**永远不放补丁**；
- `ours/scheduler-patch` —— 放补丁（1–2 个 commit）；
- 每次发版打 `vX.Y.Z-ours` tag 推到 fork。

每次上游发新 tag，跑 `ours/rebase.sh vX.Y.Z`，它会按顺序做：

1. `git fetch upstream --tags`，把 `main` 硬同步到 `upstream/main` 并 `push -f origin main`；
2. **先看 `backend/migrations/` 的 diff**（`git diff v<旧>..v<新> -- backend/migrations/`）。上游用内嵌迁移、二进制启动时自动执行、**没有 down 迁移**。**迁移 diff 非空 → 脚本停下来把 diff 打出来，必须人工拍板后才继续。这是唯一不能自动化的门。**
3. `git checkout ours/scheduler-patch && git rebase vX.Y.Z`；
4. `make test-backend` + 本补丁的单测全绿，`go build ./...` 通过；
5. 构建 linux/arm64 二进制并输出 sha256；
6. `git push -f origin ours/scheduler-patch`；`git tag vX.Y.Z-ours && git push origin vX.Y.Z-ours`。

### 冲突处理原则

**可以自行解决**（不用问）：

- 纯位置漂移、空白差异、import 顺序、上下文行变化；
- 挂钩函数体内其它行被改动，但那两处 `openAISchedulingPriorityFor(...)` 调用形状不变。

**必须停下来人工确认**：

- 挂钩函数被删除、改签名或改语义（`selectByLoadBalance` / `buildOpenAIAccountLoadPlan` / `buildOpenAIAccountSchedulerScoreSnapshot` / `isOpenAIAccountCandidateBetter`）；
- `selectByLoadBalance` 里那段过滤循环被挪走、或 `listSchedulableAccounts` 的返回语义变化（挂钩点 A 的锚点）；
- `OpenAIAccountScheduleRequest` 不再按值传递、或字段被重排/重写（`oursTiers` 靠它带下去）；
- `openAIAccountCandidateScore` 的字段改名（尤其 `priority`）或候选池构造方式变化；
- `Account.Credentials` 的类型或访问方式变化；
- 打分公式段或 TopK / 加权随机逻辑变化；
- settings 键改名；
- `backend/migrations/` diff 非空。

**如果上游自己实现了同类功能**（读了 `account_groups.priority`、把 `model_routing` 接进 OpenAI、或加了分组内优先级）→ **放弃本补丁，改用上游实现**，并在本文件里记录这个决定。

### fork 上的 GitHub Actions（**默认已整体关闭，见下**）

`.github/workflows/ours-release.yml` 是本 fork 私有的最小工作流：在 `v*-ours` tag 上跑后端测试 +
补丁单测，产出 `sub2api_linux_arm64` 附件（含 sha256）。它不建 Release、不推任何镜像，
并且带 `if: github.repository == 'Cd1s/sub2api'` 双保险。

**但是**：上游 `.github/workflows/release.yml` 的触发条件是 `push: tags: 'v*'`，
**`v0.2.4-ours` 会被它匹配到**。fork 上如果开着 Actions，推我们的 tag 会顺带触发上游那套完整发布流程
（建 GitHub Release、推 GHCR/DockerHub 镜像到你的账号名下）。上游 `backend-ci.yml` 更是 `on: push`，
每次推分支都会跑。

因此 **fork 的 Actions 已经整体关闭**（`repos/Cd1s/sub2api` → Settings → Actions → Disable）。
当前所有构建都在本机做，不依赖 CI。

要启用 CI，顺序**不能反**（先关掉上游工作流，再开 Actions）：

```bash
# 1) 先开 Actions（这一步之后 GitHub 才会注册工作流，才能逐个禁用）
gh api -X PUT repos/Cd1s/sub2api/actions/permissions -F enabled=true

# 2) 立刻把上游那几个工作流禁掉——尤其 release.yml
for w in release.yml backend-ci.yml security-scan.yml cla.yml; do
  gh api -X PUT "repos/Cd1s/sub2api/actions/workflows/$w/disable"
done

# 3) 确认只剩 ours-release.yml 是 active
gh api repos/Cd1s/sub2api/actions/workflows --jq '.workflows[] | "\(.path)\t\(.state)"'
```

随时可以一键关回去：

```bash
gh api -X PUT repos/Cd1s/sub2api/actions/permissions -F enabled=false
```

### 单测就是护栏

`backend/internal/service/openai_group_tiering_test.go` 里的这几个测试分别钉住一个挂钩点，rebase 后如果上游重构了挂钩处，它们会先红：

| 测试 | 保护的挂钩 |
|---|---|
| `TestBuildOpenAIAccountLoadPlan_RosterTiersSurviveExclusion` | A（本组出池后备用仍同档同分 —— 需求 2 的命脉） |
| `TestOpenAIAccountRosterTiers` / `TestOpenAIPlanTiers_PrefersRequestRoster` | A（名单档位表与回落逻辑） |
| `TestBuildOpenAIAccountLoadPlan_EnrolledAccountsUseTiers` | B（真实路由用档位打分） |
| `TestBuildOpenAIAccountLoadPlan_UnenrolledPoolMatchesUpstream` | B（无登记账号时与上游一致） |
| `TestBuildOpenAIAccountSchedulerScoreSnapshot_UsesTiers` | C（诊断快照与真实路由同一套档位） |
| `TestIsOpenAIAccountCandidateBetter_SameTierBreaksByLoadRate` | D（同档按 LoadRate 分散） |
| `TestIsOpenAIAccountCandidateBetter_UnscoredCandidateFallsBackToAccountPriority` | D（零值回落，保上游测试） |
| `TestOpenAITieredPriorities` | 档位表本身（含未登记账号混入的场景） |
