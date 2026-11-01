#!/usr/bin/env python3
"""文档自检：编号有定义、需求有卡承接、卡依赖无环、相对链接可打开。

用法：python3 scripts/doccheck.py   （在仓库根目录执行；有问题时非零退出）
"""
import re
import sys
from pathlib import Path
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parent.parent
DOCS = ROOT / "docs"
SPEC = DOCS / "01-需求规格说明书.md"
TASKS = DOCS / "tasks"

ID_RE = re.compile(r"\b(TT-(?:K|F|N|D|UX|AC|PARAM)-\d+|TTD-\d+|TTR-\d+|V-\d+|TT-T-\d+)\b")
LINK_RE = re.compile(r"\]\(([^)#\s]+)(?:#[^)]*)?\)")

errors: list[str] = []


def defined_in(path: Path, pattern: str) -> set[str]:
    """表格首列（`| 编号 |`）里出现的编号视为定义。"""
    out = set()
    for line in path.read_text(encoding="utf-8").splitlines():
        m = re.match(r"\|\s*(" + pattern + r")\s*\|", line)
        if m:
            out.add(m.group(1))
    return out


spec_ids = defined_in(SPEC, r"TT-(?:K|F|N|D|UX|AC|PARAM)-\d+")
design_ids = set()
for f in (DOCS / "dev").glob("*.md"):
    design_ids |= defined_in(f, r"TTD-\d+|TTR-\d+|V-\d+")

cards = {}
for f in sorted(TASKS.glob("TT-T-*.md")):
    m = re.match(r"(TT-T-\d+)", f.name)
    if m:
        cards[m.group(1)] = f
card_ids = set(cards)
defined = spec_ids | design_ids | card_ids

# 1. 所有文档里引用的编号都要有定义
for f in DOCS.rglob("*.md"):
    for i, line in enumerate(f.read_text(encoding="utf-8").splitlines(), 1):
        for ref in ID_RE.findall(line):
            if ref not in defined:
                errors.append(f"{f.relative_to(ROOT)}:{i} 引用了未定义的编号 {ref}")

# 2. 每条 TT-F / TT-N / TT-D / TT-UX / TT-AC 至少被一张卡的「对应需求」引用
covered = set()
deps: dict[str, set[str]] = {}
for cid, f in cards.items():
    text = f.read_text(encoding="utf-8")
    req = re.search(r"\|\s*对应需求\s*\|(.*)\|", text)
    if not req:
        errors.append(f"{f.name} 缺少「对应需求」行")
    else:
        covered |= set(ID_RE.findall(req.group(1)))
    dep = re.search(r"\|\s*依赖\s*\|(.*)\|", text)
    deps[cid] = set(ID_RE.findall(dep.group(1))) & card_ids if dep else set()

# 范围展开：如 TT-F-90~95
for cid, f in cards.items():
    req = re.search(r"\|\s*对应需求\s*\|(.*)\|", f.read_text(encoding="utf-8"))
    if req:
        for pre, a, b in re.findall(r"(TT-[A-Z]+-)(\d+)\s*[~～]\s*(\d+)", req.group(1)):
            for n in range(int(a), int(b) + 1):
                covered.add(f"{pre}{n:02d}" if len(a) == 2 else f"{pre}{n}")

must_cover = {i for i in spec_ids if re.match(r"TT-(F|N|D|UX|AC)-", i)}
for i in sorted(must_cover - covered):
    errors.append(f"需求 {i} 没有任何任务卡承接")

# 3. 看板里列出的卡都要存在，依赖无环
board = set(re.findall(r"\[(TT-T-\d+)\]", (TASKS / "README.md").read_text(encoding="utf-8")))
for cid in sorted(board - card_ids):
    errors.append(f"看板列出了 {cid}，但卡片文件不存在")
for cid in sorted(card_ids - board):
    errors.append(f"卡片 {cid} 不在看板里")

state: dict[str, int] = {}


def visit(n: str, stack: list[str]) -> None:
    if state.get(n) == 1:
        errors.append("任务依赖有环：" + " → ".join(stack + [n]))
        return
    if state.get(n) == 2:
        return
    state[n] = 1
    for d in sorted(deps.get(n, ())):
        visit(d, stack + [n])
    state[n] = 2


for c in sorted(deps):
    visit(c, [])

# 4. 相对链接可打开
for f in DOCS.rglob("*.md"):
    for target in LINK_RE.findall(f.read_text(encoding="utf-8")):
        if re.match(r"[a-z]+://", target):
            continue
        if not (f.parent / unquote(target)).resolve().exists():
            errors.append(f"{f.relative_to(ROOT)} 链接打不开：{target}")

print(f"规格编号 {len(spec_ids)} · 设计编号 {len(design_ids)} · 任务卡 {len(card_ids)} · 需承接需求 {len(must_cover)}")
if errors:
    print(f"\n发现 {len(errors)} 个问题：")
    for e in errors:
        print(" -", e)
    sys.exit(1)
print("全部通过")
