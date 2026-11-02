#!/usr/bin/env bash
# 扫描仓库里不该进开源仓库的内部信息（TT-T-02 / TT-AC-23 的第一道关）。
#
# 规则两类：
#   ① 类别规则写在下面（中转域名、本机路径、形似密钥的串）；
#   ② 具体的内部值（来源系统的域名、桶名、主机名、邮箱）只放在不进 git 的
#      scripts/internal-patterns.local —— 规则文件本身若写着这些值，扫描就成了泄漏源。
#      该文件不存在时只跑第①类（CI 上就是这样），并打一行提醒。
#
# 用法：scripts/scan-internal.sh [路径...]   命中时非零退出并列出文件与行号。
set -euo pipefail
cd "$(dirname "$0")/.."

targets=("$@")
[ ${#targets[@]} -eq 0 ] && targets=(.)

patterns=(
  'claudecode\.top'
  '/Volumes/[A-Za-z]'
  '/Users/[a-z]'
  'sk-[A-Za-z0-9]{24,}'
  'AKLT[A-Za-z0-9]{16,}'
  'AKIA[0-9A-Z]{16}'
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
  'gh[pousr]_[A-Za-z0-9]{30,}'
)

local_file=scripts/internal-patterns.local
if [ -f "$local_file" ]; then
  while IFS= read -r line; do
    case "$line" in ''|'#'*) continue ;; esac
    patterns+=("$line")
  done < "$local_file"
else
  echo "提示：未找到 $local_file，只按类别规则扫描" >&2
fi

joined=$(IFS='|'; echo "${patterns[*]}")

# 允许清单：说明扫描规则本身的位置；行内带 `scan-internal:allow`（并写明理由）的行放行。
hits=$(grep -rniIE "$joined" "${targets[@]}" \
  --exclude-dir=node_modules --exclude-dir=.git --exclude-dir=.nuxt --exclude-dir=.output \
  --exclude-dir=third_party \
  --exclude=scan-internal.sh --exclude=internal-patterns.local \
  2>/dev/null | grep -v 'scan-internal:allow' || true)

if [ -n "$hits" ]; then
  echo "❌ 发现疑似内部信息："
  echo "$hits" | cut -c1-200
  echo "共 $(echo "$hits" | wc -l | tr -d ' ') 处"
  exit 1
fi
echo "✅ 未发现内部信息"
