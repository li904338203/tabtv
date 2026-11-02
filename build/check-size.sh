#!/usr/bin/env bash
# 安装包体积报告（TT-T-29，TT-PARAM-02）：Markdown 表写进 $GITHUB_STEP_SUMMARY（本地打到标准输出），
# 超预算的打 ::warning（不阻塞发布）。⚠️ 工作流命令只认标准输出，所以表和告警不能走同一个流。
# 预算（压缩后）：dev/04 §3 的解压合计约 270MB，安装包按 200MB 告警。
set -euo pipefail
DIR="${1:-build/dist}"
LIMIT_MB="${TABTV_SIZE_LIMIT_MB:-200}"
OUT="${GITHUB_STEP_SUMMARY:-/dev/stdout}"
{ echo "| 安装包 | 体积 |"; echo "| --- | --- |"; } >> "$OUT"
for f in "$DIR"/*; do
  [ -f "$f" ] || continue
  name="$(basename "$f")"
  [ "$name" = SHA256SUMS ] && continue
  bytes=$(wc -c < "$f" | tr -d ' ')
  mb=$(( bytes / 1048576 ))
  echo "| $name | ${mb} MB |" >> "$OUT"
  if [ "$mb" -gt "$LIMIT_MB" ]; then
    echo "::warning::$name 为 ${mb}MB，超过 ${LIMIT_MB}MB 预算（TT-PARAM-02）"
  fi
done
