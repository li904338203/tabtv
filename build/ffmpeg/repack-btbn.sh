#!/usr/bin/env bash
# 把 BtbN/FFmpeg-Builds 的 lgpl 构建重打包成 tabtv 的统一布局（TT-T-26，Windows / Linux）。
#
# 用法：build/ffmpeg/repack-btbn.sh <BtbN 包（.zip / .tar.xz）> <平台 GOOS-GOARCH> <版本> <BtbN tag> [BtbN 构建脚本 commit]
#   产物：$OUT/ffmpeg-<版本>-<平台>.tar.xz（OUT 缺省 build/ffmpeg/out）
#
# 统一布局（fetch-third-party.sh 与 ffbin 都按它找）：
#   ffmpeg-<版本>-<平台>/bin/ffmpeg[.exe]、bin/ffprobe[.exe]（Windows 的 dll 也在 bin/，与 exe 同目录才找得到）
#                       lib/*.so.N（只有 Linux 的 shared 包有；可执行文件的 RUNPATH 是 $ORIGIN/../lib）
#                       licenses/、SOURCES.txt、buildconf.txt
#
# ⚠️ 选 lgpl-**shared** 而不是静态包：BtbN 的静态包里 ffmpeg 与 ffprobe 各自带一整套库，
#    Linux 上两个可执行文件合计 280MB；shared 包两者共用 libav*，约 170MB（压缩后 62MB 对 128MB）。
# ⚠️ BtbN 的 lgpl 变体带 --enable-version3，整体许可是 **LGPL-3.0-or-later**（macOS 自编的是 LGPL-2.1）。
#    tabtv 以独立进程调用 ffmpeg、不链接它，两种都只要求随附许可证与提供源码。
# ⚠️ 不要 ffplay：用不到，Linux 上单个 22MB。
set -euo pipefail

ARCHIVE="${1:?BtbN 包路径}"
PLAT="${2:?平台，如 linux-amd64 / windows-amd64}"
VERSION="${3:?版本，如 8.1.3}"
TAG="${4:?BtbN tag，如 autobuild-2026-11-01-12-49}"
COMMIT="${5:-}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${OUT:-$ROOT/build/ffmpeg/out}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

fail() { echo "❌ $*" >&2; exit 1; }
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

EXT=""
case "$PLAT" in windows-*) EXT=".exe" ;; linux-*) ;; *) fail "不支持的平台 ${PLAT}（macOS 用 build-macos.sh）" ;; esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
case "$ARCHIVE" in
  *.zip) unzip -q "$ARCHIVE" -d "$TMP/x" ;;
  *.tar.xz) mkdir -p "$TMP/x" && tar -xJf "$ARCHIVE" -C "$TMP/x" ;;
  *) fail "不认识的包格式：$ARCHIVE" ;;
esac
SRC="$(find "$TMP/x" -mindepth 1 -maxdepth 1 -type d | head -1)"
[ -f "$SRC/bin/ffmpeg$EXT" ] || fail "包里没有 bin/ffmpeg$EXT"
case "$(basename "$SRC")" in *-lgpl*) ;; *) fail "不是 lgpl 变体：$(basename "$SRC")" ;; esac

NAME="ffmpeg-$VERSION-$PLAT"
STAGE="$TMP/$NAME"
mkdir -p "$STAGE/bin" "$STAGE/licenses"
cp "$SRC/bin/ffmpeg$EXT" "$SRC/bin/ffprobe$EXT" "$STAGE/bin/"
if [ -n "$EXT" ]; then
  # Windows：dll 与 exe 同目录（Windows 按可执行文件所在目录找 dll）。
  find "$SRC/bin" -maxdepth 1 -name '*.dll' -exec cp {} "$STAGE/bin/" \;
elif [ -d "$SRC/lib" ]; then
  # Linux：只要 soname 那一层（libavcodec.so.62），用 -L 把软链接解成实体文件 ——
  # 软链接进了 AppImage / zip 之后不一定还是软链接，多存一份就是多 90MB。
  mkdir -p "$STAGE/lib"
  for f in "$SRC"/lib/*.so.*; do
    base="$(basename "$f")"
    [[ "$base" =~ ^[^.]+\.so\.[0-9]+$ ]] || continue
    cp -L "$f" "$STAGE/lib/$base"
  done
  ls "$STAGE"/lib/*.so.* >/dev/null 2>&1 || fail "shared 包里没有 lib/*.so.N"
fi
chmod +x "$STAGE"/bin/* 2>/dev/null || true
[ -f "$SRC/LICENSE.txt" ] && cp "$SRC/LICENSE.txt" "$STAGE/licenses/ffmpeg-LICENSE.txt"

# configure 参数原样写在库里（FFMPEG_CONFIGURATION），静态包在 exe 里，shared 包在 avutil 里。
conf=""
for f in "$STAGE"/bin/* "$STAGE"/lib/* ; do
  [ -f "$f" ] || continue
  conf="$(LC_ALL=C strings -n 30 "$f" | grep -m1 -- '--prefix=' || true)"
  [ -n "$conf" ] && break
done
[ -n "$conf" ] || fail "找不到 configure 参数（strings 里没有 --prefix=）"
echo "$conf" >"$STAGE/buildconf.txt"
for bad in enable-gpl enable-nonfree; do
  grep -q -- "--$bad" <<<"$conf" && fail "configure 含 --$bad"
done
for need in enable-libass enable-libopenh264 enable-libmp3lame; do
  grep -q -- "--$need" <<<"$conf" || fail "configure 缺 --${need}（字幕 / 回落编码器 / mp3 会用到）"
done
LICENSE_ID="LGPL-2.1-or-later"
grep -q -- '--enable-version3' <<<"$conf" && LICENSE_ID="LGPL-3.0-or-later"

ffver="$(LC_ALL=C strings -n 8 "$STAGE"/bin/ffmpeg$EXT "$STAGE"/bin/*.dll "$STAGE"/lib/* 2>/dev/null | grep -m1 -oE '\bn[0-9]+\.[0-9]+(\.[0-9]+)?-[0-9]+-g[0-9a-f]+' || true)"
{
  echo "# tabtv 随包 ffmpeg 的组成（许可证随附于 licenses/；源码获取方式见下）"
  echo "ffmpeg ${ffver:-$VERSION} $LICENSE_ID"
  echo "  构建：BtbN/FFmpeg-Builds（https://github.com/BtbN/FFmpeg-Builds）tag $TAG${COMMIT:+，构建脚本 commit $COMMIT}"
  echo "  原始包：$(basename "$ARCHIVE") sha256 $(sha256 "$ARCHIVE")"
  if [[ "$ffver" =~ -g([0-9a-f]+)$ ]]; then
    echo "  ffmpeg 源码：https://github.com/FFmpeg/FFmpeg/archive/${BASH_REMATCH[1]}.tar.gz（tabtv 的发布页另附一份）"
  fi
  echo "  各依赖库的版本与源码地址由该 commit 的 scripts.d/ 逐个固定。"
  echo "  configure 参数见 buildconf.txt。"
  echo "openh264 由源码编译，不在 Cisco 的 H.264 专利授权覆盖范围内（该授权只覆盖 Cisco 分发的二进制）。"
} >"$STAGE/SOURCES.txt"

tarball="$OUT/$NAME.tar.xz"
tar -C "$TMP" -cJf "$tarball" "$NAME"
echo "✓ ${tarball}（$(du -h "$tarball" | cut -f1)，解压后 $(du -sh "$STAGE" | cut -f1)，${LICENSE_ID}）"
echo "sha256 $(sha256 "$tarball")  $(basename "$tarball")"
