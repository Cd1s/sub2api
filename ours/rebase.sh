#!/usr/bin/env bash
# 把 fork 私有补丁分支 ours/scheduler-patch rebase 到上游新 tag 的 SOP 脚本。
#
# 用法：
#   ours/rebase.sh vX.Y.Z                      # 干跑：同步 main、过迁移门、rebase、测试、构建
#   ours/rebase.sh vX.Y.Z --migrations-reviewed # 迁移 diff 已人工看过，放行
#   ours/rebase.sh vX.Y.Z --push                # 上面都过了之后，推 fork 并打 vX.Y.Z-ours tag
#
# 硬规则：只 push origin(Cd1s/sub2api)。脚本会拒绝任何指向上游的 push，
# 也永远不会 gh pr create / gh issue create。
set -euo pipefail

PATCH_BRANCH="ours/scheduler-patch"
REPO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"

NEW_TAG="${1:-}"
shift || true
MIGRATIONS_REVIEWED=0
DO_PUSH=0
for arg in "$@"; do
  case "$arg" in
    --migrations-reviewed) MIGRATIONS_REVIEWED=1 ;;
    --push) DO_PUSH=1 ;;
    *) echo "未知参数：$arg" >&2; exit 2 ;;
  esac
done

if [ -z "$NEW_TAG" ]; then
  echo "用法：ours/rebase.sh vX.Y.Z [--migrations-reviewed] [--push]" >&2
  exit 2
fi

die() { echo; echo "❌ $*" >&2; exit 1; }

# 挂钩锚点检查：rebase 后逐个 grep 本补丁在上游文件里的挂钩行，缺任何一个就停下，
# 交人工判断上游是否重构了挂钩处（见 PATCH-NOTES「冲突处理原则」）。
# 格式：文件|期望次数|扩展正则
HOOKS=(
  "backend/internal/service/openai_account_scheduler.go|1|oursTiers[[:space:]]+map\[int64\]int"
  "backend/internal/service/openai_account_scheduler.go|1|oursQuietEscape[[:space:]]+bool"
  "backend/internal/service/openai_account_scheduler.go|1|s\.oursTryReturnHome\(ctx, req, &decision\)"
  "backend/internal/service/openai_account_scheduler.go|2|oursStickyEscapeInfo\(req\)\(\"sticky_escape_triggered\","
  "backend/internal/service/openai_account_scheduler.go|1|openAICandidateOrderingPriority\(left\), openAICandidateOrderingPriority\(right\)"
  "backend/internal/service/openai_account_scheduler.go|1|tiers := openAIPlanTiers\(req, candidates\)"
  "backend/internal/service/openai_account_scheduler.go|2|openAISchedulingPriorityFor\(candidate\.account, tiers\)"
  "backend/internal/service/openai_account_scheduler.go|1|req\.oursTiers = s\.oursRosterTiers\(req, accounts\)"
  "backend/internal/service/openai_account_scheduler.go|1|oursGroupID[[:space:]]+\*int64"
  "backend/internal/service/openai_account_scheduler.go|1|oursWeightsWithScoreGroup\(ctx, gateway\.openAIWSSchedulerWeightsForRequest\(ctx\)\)"
  "backend/internal/service/openai_account_scheduler.go|1|tiers := oursSnapshotTiers\(accounts, weights\)"
  "backend/internal/handler/admin/account_handler.go|1|service\.OursWithSchedulerScoreGroup\(ctx, groupID\)"
  "backend/internal/repository/scheduler_cache.go|1|\"ours_tiering\", \"ours_home_groups\"\}"
  "frontend/src/components/account/EditAccountModal.vue|1|data-testid=\"ours-tiering-toggle\""
)

check_hooks() {
  local failed=0 entry file want pattern got
  for entry in "${HOOKS[@]}"; do
    IFS='|' read -r file want pattern <<<"$entry"
    got="$(grep -cE -- "$pattern" "$file" 2>/dev/null || true)"
    if [ "$got" != "$want" ]; then
      echo "    ✗ $file 期望 $want 处，实际 ${got:-0} 处：$pattern" >&2
      failed=1
    fi
  done
  [ "$failed" = "0" ] || die "挂钩锚点缺失或重复。上游可能重构了挂钩处，按 PATCH-NOTES「冲突处理原则」人工处理后再重跑。"
  echo "    ${#HOOKS[@]} 个挂钩锚点全部就位。"
}

# ---------------------------------------------------------------- 前置检查
ORIGIN_URL="$(git remote get-url origin)"
UPSTREAM_URL="$(git remote get-url upstream 2>/dev/null || true)"
case "$ORIGIN_URL" in
  *Cd1s/sub2api*) ;;
  *) die "origin 不是 fork（当前 $ORIGIN_URL）。拒绝继续。" ;;
esac
case "$UPSTREAM_URL" in
  *Wei-Shaw/sub2api*) ;;
  *) die "upstream 未指向 Wei-Shaw/sub2api（当前 ${UPSTREAM_URL:-未设置}）。" ;;
esac
[ -z "$(git status --porcelain)" ] || die "工作区不干净，先提交或 stash。"

# ------------------------------------------------------- 1) main 只镜像上游
echo "==> [1/6] 同步 upstream 与 main"
git fetch upstream --tags --prune
git rev-parse -q --verify "refs/tags/$NEW_TAG" >/dev/null || die "上游没有 tag $NEW_TAG"

OLD_TAG="$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-ours' "$PATCH_BRANCH" 2>/dev/null || true)"
[ -n "$OLD_TAG" ] || die "无法确定 $PATCH_BRANCH 当前基于哪个上游 tag。"
echo "    当前基线：$OLD_TAG  →  目标：$NEW_TAG"

git checkout main
git reset --hard upstream/main
if [ "$DO_PUSH" = "1" ]; then
  git push -f origin main
else
  echo "    （干跑：跳过 git push -f origin main）"
fi

# ------------------------------------------- 2) 迁移门：唯一不能自动化的一步
echo "==> [2/6] 检查 backend/migrations/ 差异（$OLD_TAG..$NEW_TAG）"
MIGRATION_DIFF="$(git diff --stat "$OLD_TAG..$NEW_TAG" -- backend/migrations/ || true)"
if [ -n "$MIGRATION_DIFF" ]; then
  echo
  echo "$MIGRATION_DIFF"
  echo
  git diff "$OLD_TAG..$NEW_TAG" -- backend/migrations/
  echo
  if [ "$MIGRATIONS_REVIEWED" != "1" ]; then
    die "迁移 diff 非空。上游迁移内嵌在二进制里、启动时自动执行且没有 down 迁移。
    请人工看完上面的 diff、确认可以接受之后，再加 --migrations-reviewed 重跑。"
  fi
  echo "    ⚠️  迁移 diff 非空，但已带 --migrations-reviewed，继续。"
else
  echo "    迁移无变化。"
fi

# ------------------------------------------------------------ 3) rebase 补丁
echo "==> [3/6] rebase $PATCH_BRANCH 到 $NEW_TAG"
git checkout "$PATCH_BRANCH"
if ! git rebase "$NEW_TAG"; then
  cat >&2 <<'EOF'

rebase 有冲突。处理原则（详见 PATCH-NOTES.md「冲突处理原则」）：

  可以自行解决：纯位置漂移、空白、import 顺序、上下文行变化。
  必须停下来人工确认：
    - 挂钩函数被删/改签名/改语义
      (Select / selectBySessionHash / selectByLoadBalance / buildOpenAIAccountLoadPlan /
       buildOpenAIAccountSchedulerScoreSnapshot / isOpenAIAccountCandidateBetter)
    - openAIAccountCandidateScore 字段改名（尤其 priority）
    - Account.Credentials 类型或访问方式变化
    - 打分公式段 / TopK / 加权随机逻辑变化
    - settings 键改名

  如果上游自己实现了同类功能（读了 account_groups.priority、把 model_routing
  接进 OpenAI、或加了分组内优先级）→ 放弃本补丁、采用上游实现，并在
  PATCH-NOTES.md 里记录这个决定。

解决完执行 git rebase --continue，然后重跑本脚本。
EOF
  exit 1
fi

# ------------------------------------------------------------ 4) 测试与编译
echo "==> [4/6] 挂钩锚点检查"
check_hooks

echo "==> [4/6] go build + 全量后端测试"
(cd backend && go build ./...)
(cd backend && go test ./...)
echo "    补丁自带单测："
(cd backend && go test ./internal/service/ -count=1 -run 'Ours|Tiering|CandidateBetter|LoadPlan|ScoreSnapshot|SchedulingPriorityFor|RateLimitServiceSchedulerScore' -v \
  | grep -E '^--- (PASS|FAIL)' || true)
echo "    前端开关单测："
(cd frontend && npx vitest run src/components/account/__tests__/EditAccountModal.oursTiering.spec.ts)

if command -v golangci-lint >/dev/null 2>&1; then
  echo "    golangci-lint："
  (cd backend && golangci-lint run ./... --timeout=30m)
else
  echo "    （未安装 golangci-lint，跳过；CI 用 v2.13）"
fi

# ---------------------------------------------------------------- 5) 出二进制
echo "==> [5/6] 构建 linux/arm64 + linux/amd64 发布包"
ours/build-release.sh

# -------------------------------------------------------------------- 6) 推送
OURS_TAG="${NEW_TAG}-ours"
if [ "$DO_PUSH" = "1" ]; then
  echo "==> [6/6] 推送 fork 并打 tag $OURS_TAG"
  git push -f origin "$PATCH_BRANCH"
  git tag -f "$OURS_TAG"
  git push -f origin "$OURS_TAG"
else
  echo "==> [6/6] 干跑结束。确认无误后执行："
  echo "    git push -f origin main"
  echo "    git push -f origin $PATCH_BRANCH"
  echo "    git tag $OURS_TAG && git push origin $OURS_TAG"
fi

echo
echo "✅ 完成。禁止 git push upstream / gh pr create / gh issue create。"
