#!/usr/bin/env bash
# 跑一轮用例并打印结果表：./cases.sh [标签]
#
# 每个用例都先把 Redis 里的「已用微单位」直接设成目标值，
# 从而精确构造出想要的水位——不必反复发请求把预算烧到某个比例。
#
# 上游 mock-llm 会把收到的 model 原样回显在响应里，
# 所以响应中的 model 就是「实际发出去的模型」，可直接用来判断改写是否发生。

cd "$(dirname "$0")"
source ./lib.sh

LABEL=${1:-run}

printf '\n===== %s =====\n' "$LABEL"
printf '%-11s %-9s %-7s %-18s %-18s %-6s\n' 用例 已用micro 剩余 期望 实际 HTTP

run_case() {
  local name=$1 used=$2 expect=$3
  local out code actual remain

  redis_cli DEL "$BUDGET_KEY" >/dev/null
  [ "$used" != "0" ] && redis_cli SET "$BUDGET_KEY" "$used" EX "$PERIOD" >/dev/null
  remain=$(awk "BEGIN{printf \"%.2f\", ($QUOTA_MICRO-$used)/$QUOTA_MICRO}")

  out=$(ask mock-chat -w $'\n%{http_code}')
  code=$(printf '%s' "$out" | tail -1)
  # exhausted 档返回的是 429 错误体，里面没有 model 字段，grep 无匹配会返回 1；
  # 脚本开了 pipefail，这里必须兜住，否则整轮用例会在最后一行静默中断
  actual=$(printf '%s' "$out" | grep -o '"model": "[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/' || true)

  printf '%-11s %-9s %-7s %-18s %-18s %-6s\n' \
    "$name" "$used" "$remain" "$expect" "${actual:--}" "$code"
}

# 档位表：warn<=0.50 -> mock-chat-backup；degrade<=0.20 -> mock-chat-mini；exhausted<=0.00 -> 429
run_case 充足      0       mock-chat
run_case warn      600000  mock-chat-backup
run_case degrade   850000  mock-chat-mini
run_case exhausted 1000000 429

echo
echo "--- 计费扣减：原模型 vs 降级后模型 ---"
redis_cli DEL "$BUDGET_KEY" >/dev/null
ask mock-chat >/dev/null; sleep 1
echo "  水位充足（走 mock-chat, 100/百万token）: used=$(redis_cli GET "$BUDGET_KEY")"

redis_cli SET "$BUDGET_KEY" 600000 EX "$PERIOD" >/dev/null
ask mock-chat >/dev/null; sleep 1
echo "  warn 档（降到 mock-chat-backup, 10/百万token）: used=$(redis_cli GET "$BUDGET_KEY") （减去 600000 即为本次扣减）"

echo
echo "--- 流式 SSE ---"
redis_cli DEL "$BUDGET_KEY" >/dev/null
ask_stream mock-chat | tail -2
sleep 1
echo "  带 include_usage: $(show_budget)"

redis_cli DEL "$BUDGET_KEY" >/dev/null
ask_stream_no_usage mock-chat >/dev/null
sleep 1
echo "  不带 include_usage（应无扣减）: $(show_budget)"

redis_cli DEL "$BUDGET_KEY" >/dev/null
