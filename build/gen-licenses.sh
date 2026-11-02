#!/usr/bin/env bash
# 生成依赖许可清单并拦截 GPL / AGPL（TT-T-32，TT-AC-24）。
#
# 输出：build/licenses/dependencies.json
#   { "go": [{name, version, license, file}], "npm": [{name, version, license}], "lgpl": [...], "blocked": [...] }
#
# 口径：
#   - Go：只算**链进应用二进制的模块**（`go list -deps .`），测试与工具依赖不分发，不算。
#     本机没有 go-licenses 时，从模块目录里的 LICENSE / COPYING 文件按关键字判类型。
#     ⚠️ go list 只看当前平台：Wails 在 Linux / Windows 会多链几个模块，三平台 CI 各跑一次才完整。
#   - npm：`pnpm licenses list --json --prod`（前端是打包产物，devDependencies 不进安装包）。
#   - 「A OR B」的双许可只要有一个可选项不是 GPL / AGPL 就放行（使用者可选宽松的那一个）。
#   - LGPL 不拦截但单列：动态链接 / 独立进程使用时可接受，要人看一眼使用方式。
#   - 判不出类型的记为 UNKNOWN，不拦截但单列在 unknown 里。
#
# 退出码：0 无 GPL / AGPL；1 出现 GPL / AGPL；2 脚本自身出错。
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/build/licenses/dependencies.json"
mkdir -p "$(dirname "$out")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cd "$root"
go list -deps -f '{{with .Module}}{{.Path}}	{{.Version}}	{{.Dir}}{{end}}' . | sort -u | grep -v '^$' >"$tmp/go.tsv" || exit 2
pnpm licenses list --json --prod >"$tmp/npm.json" 2>/dev/null || exit 2

python3 - "$tmp/go.tsv" "$tmp/npm.json" "$out" <<'PY'
import json, os, re, sys

go_tsv, npm_json, out = sys.argv[1:4]
SELF = ("github.com/li904338203/tabtv",)

def classify(text):
    t = text.lower()
    if "gnu affero general public license" in t: return "AGPL"
    if "gnu lesser general public license" in t or "gnu library general public license" in t: return "LGPL"
    if "gnu general public license" in t: return "GPL"
    if "mozilla public license" in t: return "MPL-2.0"
    if "apache license" in t: return "Apache-2.0"
    if "permission is hereby granted, free of charge" in t: return "MIT"
    if "redistribution and use in source and binary forms" in t:
        return "BSD-3-Clause" if "neither the name" in t or "names of its" in t else "BSD-2-Clause"
    if re.search(r"permission to use, copy, modify, (and/or|and) distribute this software for any", t): return "ISC"
    if "unlicense" in t: return "Unlicense"
    return "UNKNOWN"

def find_license(d):
    if not d or not os.path.isdir(d): return None
    for name in sorted(os.listdir(d)):
        if re.match(r"(?i)^(license|licence|copying)(\.|$|-)", name):
            return os.path.join(d, name)
    return None

go = []
for line in open(go_tsv, encoding="utf-8"):
    parts = line.rstrip("\n").split("\t")
    if len(parts) < 3 or parts[0].startswith(SELF): continue
    path, ver, d = parts
    f = find_license(d)
    lic = classify(open(f, encoding="utf-8", errors="replace").read()) if f else "UNKNOWN"
    go.append({"name": path, "version": ver, "license": lic, "file": os.path.basename(f) if f else ""})

def bad(expr):
    # 双许可：任一可选项不是 GPL/AGPL 即放行。
    alts = re.split(r"\s+OR\s+", expr.strip("() "))
    return all(re.search(r"(?<!L)(A?GPL)", a) for a in alts)

npm = []
for lic, pkgs in json.load(open(npm_json, encoding="utf-8")).items():
    for p in pkgs:
        for v in p.get("versions", [p.get("version", "")]):
            npm.append({"name": p["name"], "version": v, "license": lic})

blocked, lgpl, unknown = [], [], []
for src, items in (("go", go), ("npm", npm)):
    for it in items:
        lic = it["license"]
        tag = {"source": src, "name": it["name"], "version": it["version"], "license": lic}
        if lic == "UNKNOWN": unknown.append(tag)
        elif bad(lic): blocked.append(tag)
        elif "LGPL" in lic: lgpl.append(tag)

json.dump({"go": go, "npm": sorted(npm, key=lambda x: (x["name"], x["version"])),
           "lgpl": lgpl, "unknown": unknown, "blocked": blocked},
          open(out, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
open(out, "a").write("\n")

print(f"Go 模块 {len(go)} 个，npm 包 {len(npm)} 个 → {os.path.relpath(out)}")
for name, rows in (("LGPL（不拦截，需确认使用方式）", lgpl), ("无法判定（不拦截）", unknown)):
    if rows:
        print(f"{name}：")
        for r in rows: print(f"  [{r['source']}] {r['name']}@{r['version']}  {r['license']}")
if blocked:
    print("❌ 出现 GPL / AGPL 依赖：")
    for r in blocked: print(f"  [{r['source']}] {r['name']}@{r['version']}  {r['license']}")
    sys.exit(1)
print("✅ 无 GPL / AGPL 依赖")
PY
