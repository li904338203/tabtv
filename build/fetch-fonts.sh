#!/usr/bin/env bash
# 下载智能剪辑排版用的字体（docs/agent/smartcut SCT-13，SCD-28）。
#
# 为什么要服务端字体：本机 ffmpeg 既没有 libass 也没有 drawtext，字幕与文字由
# internal/timeline/typeset 排版成透明 PNG 再叠加；预览与成片用的是同一张图（SC-N-06）。
#
# ⚠️ 字体**不进 git**（思源黑体一个字重约 16 MB）：本脚本按固定地址 + sha256 下载到
#    backend/assets/fonts/（该目录在 .gitignore 里）。`make fonts` 调用它，
#    test-go / test-race / test-it 依赖它；部署镜像里要把这个目录一起拷进去，或用
#    SEEAITV_TYPESET_FONT_DIR 指到别处。已下载且 sha256 对得上的文件不会重下。
# ⚠️ 地址都钉在**固定的版本**上（思源黑体 tag 2.004R、google/fonts 的一次提交），
#    再加 sha256：上游换了文件（哪怕 tag 被移动）这里会直接报错，而不是悄悄换了字形。
#
# 来源与许可（都是 SIL Open Font License 1.1，03 C-3 允许 OFL 只用于字体）：
#   - 思源黑体 Source Han Sans SC Regular（语言版 OTF，含中日韩全部字形，默认简中字形；
#     只取常规体 —— 文字样式里没有粗细字段）
#     https://github.com/adobe-fonts/source-han-sans  tag 2.004R
#   - Noto Emoji（单色，可变字重）
#     https://github.com/google/fonts/tree/8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5/ofl/notoemoji
#
# 用法：bash scripts/fetch-fonts.sh [目标目录]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="${1:-${SEEAITV_TYPESET_FONT_DIR:-$ROOT/backend/assets/fonts}}"
mkdir -p "$DIR"

SHA="https://raw.githubusercontent.com/adobe-fonts/source-han-sans/2.004R"
NOTO="https://raw.githubusercontent.com/google/fonts/8b0a1d0f5983c89bc2b93f1b5fb55f9e252744b5/ofl/notoemoji"

# 文件名|地址|sha256
FILES="
SourceHanSansSC-Regular.otf|$SHA/OTF/SimplifiedChinese/SourceHanSansSC-Regular.otf|84bbd4ace91d327b3ad1a581c688196278a4e41308520176f419180064e4af2b
LICENSE-SourceHanSans.txt|$SHA/LICENSE.txt|f55c2d43dd905011515f5e46ba78d180027e314ef8ccaaf53a9e88fe316767cd
NotoEmoji-VariableFont.ttf|$NOTO/NotoEmoji%5Bwght%5D.ttf|de6c18832938afc99caf132b39d6a30a19bac7f2e812e28db2535b4608d27551
OFL-NotoEmoji.txt|$NOTO/OFL.txt|500bb1ccf43df7bbb522112f9133a52b16e1c35e809632f5d8609b179152de5b
"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

fetched=0
for line in $FILES; do
	name="${line%%|*}"
	rest="${line#*|}"
	url="${rest%|*}"
	want="${rest##*|}"
	dst="$DIR/$name"
	if [ -f "$dst" ] && [ "$(sha256_of "$dst")" = "$want" ]; then
		continue
	fi
	tmp="$dst.part"
	echo "↓ $name"
	curl -fsSL --retry 3 --connect-timeout 20 -m 900 -o "$tmp" "$url"
	got="$(sha256_of "$tmp")"
	if [ "$got" != "$want" ]; then
		rm -f "$tmp"
		echo "❌ $name 的 sha256 不符：期望 $want，实际 $got（上游换了文件？）" >&2
		exit 1
	fi
	mv "$tmp" "$dst"
	fetched=$((fetched + 1))
done

# 来源说明随字体一起放（部署时整个目录拷走，许可与出处跟着走）。
{
	echo "智能剪辑排版字体（SCT-13）。由 scripts/fetch-fonts.sh 生成，勿手改。"
	echo "许可：SIL Open Font License 1.1（见同目录的 LICENSE-SourceHanSans.txt、OFL-NotoEmoji.txt）。"
	echo
	for line in $FILES; do
		echo "${line%%|*}"
		rest="${line#*|}"
		echo "  来源   ${rest%|*}"
		echo "  sha256 ${rest##*|}"
	done
} >"$DIR/SOURCES.txt"

if [ "$fetched" -gt 0 ]; then
	echo "✅ 字体已就绪（新下载 $fetched 个）：$DIR"
fi
