#!/usr/bin/env bash
# 从源码静态构建随包的 LGPL ffmpeg / ffprobe（macOS，TT-T-26，dev/04 TTD-52）。
#
# 用法：build/ffmpeg/build-macos.sh <arm64|x86_64>
#   产物：$OUT/ffmpeg-<版本>-darwin-<arm64|amd64>.tar.xz（OUT 缺省 build/ffmpeg/out）
#   中间目录：$WORK（缺省 /tmp/tabtv-ffmpeg-build），源码缓存 build/third_party/.cache/ffmpeg-src
#
# 为什么自己编：Windows / Linux 有 BtbN 的 lgpl 构建可用，macOS 没有可信的 LGPL 静态构建
# （常见的 evermeet / martin-riedl 都是 GPL，带 libx264）。
#
# ⚠️ LGPL 的三条底线，脚本末尾逐条自检：
#   1. configure 不带 --enable-gpl / --enable-nonfree / --enable-version3（后者会把整体变成 LGPLv3）；
#   2. 第三方库全部是这里自己编的静态库 —— 链到 Homebrew 的动态库的话，换一台没装 brew 的机器就起不来；
#   3. 产物附带每个组件的版本、源码地址与校验和（SOURCES.txt），并在发布时把源码 tarball 一起上传。
#
# 依赖：Xcode 命令行工具 + meson、ninja、nasm、pkg-config（只当构建工具用，不链接它们的库）。
set -euo pipefail

ARCH="${1:-}"
case "$ARCH" in
  arm64) GOARCH=arm64; HOST_TRIPLE=aarch64-apple-darwin; FF_ARCH=aarch64 ;;
  x86_64) GOARCH=amd64; HOST_TRIPLE=x86_64-apple-darwin; FF_ARCH=x86_64 ;;
  *) echo "用法：$0 <arm64|x86_64>" >&2; exit 2 ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SRC_CACHE="${SRC_CACHE:-$ROOT/build/third_party/.cache/ffmpeg-src}"
# ⚠️ 中间目录固定在 /tmp 下：configure 参数（含路径）会被原样写进二进制（ffmpeg -buildconf），
# 放在仓库里的话发布出去的程序会带着构建机上的用户目录。
WORK="${WORK:-/tmp/tabtv-ffmpeg-build}/$ARCH"
OUT="${OUT:-$ROOT/build/ffmpeg/out}"
JOBS="${JOBS:-$(sysctl -n hw.ncpu)}"
export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-11.0}"

FFMPEG_VERSION=8.1.3
# 名称 版本 地址 SHA-256 许可证（版本与校验和写死：换版本 = 改这里 + 重跑，不在构建机上临时取「最新」）
SOURCES=(
  "ffmpeg 8.1.3 https://ffmpeg.org/releases/ffmpeg-8.1.3.tar.xz 7138d28c96d9d3e3af4ee3d8cad72741f8ffb40da90c1112235dea3ecd3178a3 LGPL-2.1-or-later"
  "freetype 2.15.0 https://download.savannah.gnu.org/releases/freetype/freetype-2.15.0.tar.xz 93cd47eb719c325107f2d3cf087d06299e944c3b48a40b55537302304dea1dd5 FTL"
  "fribidi 1.0.18 https://github.com/fribidi/fribidi/releases/download/v1.0.18/fribidi-1.0.18.tar.xz 1de5e8073d035388b71d0b2d452b1da1f8caeea8a7702997b401fe1b6250727f LGPL-2.1-or-later"
  "harfbuzz 15.0.0 https://github.com/harfbuzz/harfbuzz/releases/download/15.0.0/harfbuzz-15.0.0.tar.xz 968536e9abd782f07f7954044c0c4020199fcdd39b41d221ec42d2a3be4a44df MIT"
  "libass 0.17.5 https://github.com/libass/libass/releases/download/0.17.5/libass-0.17.5.tar.xz 2dca25c0e0c837ddf00b52011b3f82cac1e4ddd3ad018227806b0c2288864acc ISC"
  "lame 3.100 https://downloads.sourceforge.net/project/lame/lame/3.100/lame-3.100.tar.gz ddfe36cab873794038ae2c1210557ad34857a4b6bdc515785d1da9e175b1da1e LGPL-2.0-or-later"
  "openh264 2.7.0 https://github.com/cisco/openh264/archive/refs/tags/v2.7.0.tar.gz 5178775c8b440073e70920e97d195efff721c0e56d0a78cc82c919e95d54924d BSD-2-Clause"
)

sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
log() { echo "[$(date +%H:%M:%S)] $*"; }

PREFIX="$WORK/prefix"
BUILD="$WORK/src"
rm -rf "$WORK"
mkdir -p "$SRC_CACHE" "$PREFIX" "$BUILD" "$OUT"

# 只认自己编出来的 .pc：PKG_CONFIG_LIBDIR 会**替换**默认搜索路径，Homebrew 的库因此对构建不可见。
export PKG_CONFIG_LIBDIR="$PREFIX/lib/pkgconfig"
unset PKG_CONFIG_PATH
export CC="clang -arch $ARCH"
export CXX="clang++ -arch $ARCH"
export CFLAGS="-O2 -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
export CXXFLAGS="$CFLAGS"
export LDFLAGS="-arch $ARCH -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
export CPPFLAGS="-I$PREFIX/include"

CROSS=0
[ "$(uname -m)" != "$ARCH" ] && CROSS=1
AUTOTOOLS_HOST=()
[ "$CROSS" = 1 ] && AUTOTOOLS_HOST=(--host="$HOST_TRIPLE")

# meson：交叉时用 cross file（CPU 族与 clang -arch），本机时用 native file 固定编译器。
MESON_FILE="$WORK/meson.ini"
cpu_family=$([ "$ARCH" = arm64 ] && echo aarch64 || echo x86_64)
cat >"$MESON_FILE" <<EOF
[binaries]
c = ['clang', '-arch', '$ARCH']
cpp = ['clang++', '-arch', '$ARCH']
objc = ['clang', '-arch', '$ARCH']
ar = 'ar'
strip = 'strip'
pkg-config = 'pkg-config'

[built-in options]
c_args = ['-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET']
cpp_args = ['-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET']
c_link_args = ['-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET']
cpp_link_args = ['-mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET']

[host_machine]
system = 'darwin'
cpu_family = '$cpu_family'
cpu = '$ARCH'
endian = 'little'
EOF
MESON_ARGS=(--prefix="$PREFIX" --libdir=lib --buildtype=release --default-library=static --wrap-mode=nofallback)
if [ "$CROSS" = 1 ]; then MESON_ARGS+=(--cross-file "$MESON_FILE"); else MESON_ARGS+=(--native-file "$MESON_FILE"); fi

fetch() { # 名称 → 解压后的源码目录
  local line name ver url sum file dir
  for line in "${SOURCES[@]}"; do
    read -r name ver url sum _ <<<"$line"
    [ "$name" = "$1" ] || continue
    file="$SRC_CACHE/$name-$ver.tar.${url##*.tar.}"
    if [ ! -f "$file" ] || [ "$(sha256 "$file")" != "$sum" ]; then
      log "下载 $name $ver" >&2
      curl -sSfL -o "$file.part" "$url"
      mv "$file.part" "$file"
    fi
    if [ "$(sha256 "$file")" != "$sum" ]; then
      echo "❌ $name 校验失败：$(sha256 "$file") ≠ $sum" >&2
      exit 1
    fi
    dir="$BUILD/$name"
    mkdir -p "$dir"
    tar -xf "$file" -C "$dir" --strip-components=1
    echo "$dir"
    return
  done
  echo "❌ 没有源码条目：$1" >&2
  exit 1
}

started=$(date +%s)

log "freetype"
d=$(fetch freetype)
# 关掉 zlib / png / bzip2 / brotli / harfbuzz：字幕只读未压缩的 TTF/OTF；harfbuzz 依赖 freetype，反过来会成环。
meson setup "$d/_b" "$d" "${MESON_ARGS[@]}" -Dzlib=disabled -Dpng=disabled -Dbzip2=disabled -Dbrotli=disabled -Dharfbuzz=disabled -Dtests=disabled >/dev/null
ninja -C "$d/_b" install >/dev/null

log "fribidi"
d=$(fetch fribidi)
meson setup "$d/_b" "$d" "${MESON_ARGS[@]}" -Ddocs=false -Dbin=false -Dtests=false >/dev/null
ninja -C "$d/_b" install >/dev/null

log "harfbuzz"
d=$(fetch harfbuzz)
meson setup "$d/_b" "$d" "${MESON_ARGS[@]}" -Dfreetype=enabled -Dglib=disabled -Dgobject=disabled -Dcairo=disabled \
  -Dicu=disabled -Dchafa=disabled -Dcoretext=disabled -Dtests=disabled -Ddocs=disabled -Dutilities=disabled \
  -Dintrospection=disabled -Dbenchmark=disabled >/dev/null
ninja -C "$d/_b" install >/dev/null

log "libass"
d=$(fetch libass)
# 字体后端用 CoreText（系统自带），不要 fontconfig：后者要带一套配置文件与缓存目录，打包后路径全对不上。
# 字幕实际用的字体是随包的思源黑体，由渲染侧用 fontsdir 指定（TT-T-25）。
(cd "$d" && ./configure --prefix="$PREFIX" ${AUTOTOOLS_HOST[@]+"${AUTOTOOLS_HOST[@]}"} --disable-shared --enable-static \
  --disable-fontconfig --enable-coretext --disable-require-system-font-provider --disable-asm >/dev/null && make -j"$JOBS" install >/dev/null 2>&1)

log "lame"
d=$(fetch lame)
# lame 3.100 的导出符号表里有一个不存在的 lame_init_old，只影响动态库；这里只编静态库。
(cd "$d" && ./configure --prefix="$PREFIX" ${AUTOTOOLS_HOST[@]+"${AUTOTOOLS_HOST[@]}"} --disable-shared --enable-static --disable-frontend \
  --disable-gtktest --disable-dependency-tracking >/dev/null && make -j"$JOBS" install >/dev/null 2>&1)

log "openh264"
d=$(fetch openh264)
# 回落编码器：VideoToolbox 在虚拟机 / 部分机型上不可用时才用它（TT-T-27）。
# ⚠️ 自己从源码编的 openh264 **不享受** Cisco 的专利授权（那只覆盖 Cisco 分发的二进制），写进 NOTICE。
(cd "$d" && make -j"$JOBS" OS=darwin ARCH="$ARCH" CC="clang" CXX="clang++" \
  CFLAGS_OPT="-O2" LDFLAGS="$LDFLAGS" PREFIX="$PREFIX" \
  CFLAGS="-arch $ARCH -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
  CXXFLAGS="-arch $ARCH -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
  install-static >/dev/null 2>&1)

log "ffmpeg $FFMPEG_VERSION"
d=$(fetch ffmpeg)
FF_CROSS=()
[ "$CROSS" = 1 ] && FF_CROSS=(--enable-cross-compile --target-os=darwin)
# --disable-autodetect：不自动探测任何外部库，构建机上装了什么都不会悄悄链进去；
# 需要的逐个 --enable。系统框架（VideoToolbox / AudioToolbox）与 /usr/lib 的 zlib、iconv 显式打开。
(cd "$d" && ./configure \
  --prefix="$PREFIX" --arch="$FF_ARCH" ${FF_CROSS[@]+"${FF_CROSS[@]}"} \
  --cc="clang -arch $ARCH" --cxx="clang++ -arch $ARCH" \
  --extra-cflags="-I$PREFIX/include -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
  --extra-ldflags="-L$PREFIX/lib -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET" \
  --pkg-config=pkg-config --pkg-config-flags=--static \
  --disable-shared --enable-static --disable-autodetect \
  --disable-doc --disable-ffplay --disable-debug \
  --enable-videotoolbox --enable-audiotoolbox --enable-zlib --enable-iconv \
  --enable-libass --enable-libfreetype --enable-libfribidi --enable-libharfbuzz \
  --enable-libmp3lame --enable-libopenh264 >"$WORK/ffmpeg-configure.log" 2>&1 \
  || { tail -40 "$WORK/ffmpeg-configure.log"; tail -60 ffbuild/config.log; exit 1; })
(cd "$d" && make -j"$JOBS" >/dev/null 2>"$WORK/ffmpeg-make.log" || { tail -40 "$WORK/ffmpeg-make.log"; exit 1; })

log "打包"
NAME="ffmpeg-$FFMPEG_VERSION-darwin-$GOARCH"
STAGE="$WORK/$NAME"
mkdir -p "$STAGE/bin" "$STAGE/licenses"
cp "$d/ffmpeg" "$d/ffprobe" "$STAGE/bin/"
strip -x "$STAGE/bin/ffmpeg" "$STAGE/bin/ffprobe"
# 苹果芯片要求可执行文件至少带 ad-hoc 签名，strip 会让原签名失效，重签一次。
codesign --force -s - "$STAGE/bin/ffmpeg" "$STAGE/bin/ffprobe" 2>/dev/null || true

cp "$d/COPYING.LGPLv2.1" "$STAGE/licenses/ffmpeg-COPYING.LGPLv2.1"
cp "$d/LICENSE.md" "$STAGE/licenses/ffmpeg-LICENSE.md"
cp "$BUILD/freetype/LICENSE.TXT" "$STAGE/licenses/freetype-LICENSE.TXT"
cp "$BUILD/freetype/docs/FTL.TXT" "$STAGE/licenses/freetype-FTL.TXT"
cp "$BUILD/fribidi/COPYING" "$STAGE/licenses/fribidi-COPYING"
cp "$BUILD/harfbuzz/COPYING" "$STAGE/licenses/harfbuzz-COPYING"
cp "$BUILD/libass/COPYING" "$STAGE/licenses/libass-COPYING"
cp "$BUILD/lame/COPYING" "$STAGE/licenses/lame-COPYING"
cp "$BUILD/openh264/LICENSE" "$STAGE/licenses/openh264-LICENSE"

{
  echo "# tabtv 随包 ffmpeg 的组成（LGPL-2.1 第 6 条：可从下列地址取得对应源码；发布页同时附带源码包）"
  echo "# 名称 版本 许可证 源码地址 SHA-256"
  for line in "${SOURCES[@]}"; do
    read -r name ver url sum lic <<<"$line"
    echo "$name $ver $lic $url $sum"
  done
  echo
  echo "构建脚本：build/ffmpeg/build-macos.sh（tabtv 仓库），目标 ${ARCH}，最低 macOS $MACOSX_DEPLOYMENT_TARGET"
  echo "openh264 由源码编译，不在 Cisco 的 H.264 专利授权覆盖范围内（该授权只覆盖 Cisco 分发的二进制）。"
} >"$STAGE/SOURCES.txt"

# ---- 自检（任何一条不过都不出产物）----
fail() { echo "❌ 自检失败：$*" >&2; exit 1; }
RUN=("$STAGE/bin/ffmpeg")
if [ "$CROSS" = 1 ]; then
  if [ "$ARCH" = x86_64 ] && arch -x86_64 /usr/bin/true 2>/dev/null; then RUN=(arch -x86_64 "$STAGE/bin/ffmpeg"); else RUN=(); fi
fi

conf=$(grep -o 'FFMPEG_CONFIGURATION "[^"]*"' "$d/config.h" | sed 's/^FFMPEG_CONFIGURATION //; s/"//g')
echo "$conf" >"$STAGE/buildconf.txt"
for bad in enable-gpl enable-nonfree enable-version3; do
  grep -q -- "--$bad" <<<"$conf" && fail "configure 含 --$bad"
done
grep -q 'CONFIG_GPL 1' "$d/config.h" && fail "config.h 里 CONFIG_GPL=1"
grep -q 'CONFIG_NONFREE 1' "$d/config.h" && fail "config.h 里 CONFIG_NONFREE=1"
grep -q 'CONFIG_VERSION3 1' "$d/config.h" && fail "config.h 里 CONFIG_VERSION3=1"

for bin in ffmpeg ffprobe; do
  deps=$(otool -L "$STAGE/bin/$bin" | tail -n +2 | awk '{print $1}')
  bad=$(grep -vE '^(/usr/lib/|/System/Library/)' <<<"$deps" || true)
  [ -n "$bad" ] && fail "$bin 依赖了系统之外的动态库：$bad"
  lipo -archs "$STAGE/bin/$bin" | grep -qw "$ARCH" || fail "$bin 不是 $ARCH"
done

if [ "${#RUN[@]}" -gt 0 ]; then
  enc=$("${RUN[@]}" -hide_banner -encoders 2>/dev/null)
  for e in h264_videotoolbox libopenh264 aac libmp3lame png mjpeg pcm_s16le; do
    grep -qw "$e" <<<"$enc" || fail "缺编码器 $e"
  done
  grep -qw libx264 <<<"$enc" && fail "不该有 libx264（GPL）"
  flt=$("${RUN[@]}" -hide_banner -filters 2>/dev/null)
  for f in subtitles ass xfade loudnorm blackdetect tile overlay colorchannelmixer atempo amix; do
    grep -qE "^ [.A-Z|]+ $f " <<<"$flt" || fail "缺滤镜 $f"
  done
  # 真编一帧：编码器在列表里不等于能用（VideoToolbox 在无 GPU 的虚拟机里会失败，那时 openh264 必须能用）。
  "${RUN[@]}" -hide_banner -loglevel error -f lavfi -i color=c=red:s=64x64:d=0.1 -frames:v 1 -c:v libopenh264 -f null - \
    || fail "libopenh264 试编码失败"
  echo "✓ 编码器与滤镜自检通过"
else
  echo "⚠️ 本机无法运行 $ARCH 产物（交叉构建且无 Rosetta），跳过运行期自检；静态自检已通过"
fi

tarball="$OUT/$NAME.tar.xz"
tar -C "$WORK" -cJf "$tarball" "$NAME"
elapsed=$(( $(date +%s) - started ))
log "完成：${tarball}（$(du -h "$tarball" | cut -f1)，解压后 $(du -sh "$STAGE" | cut -f1)，耗时 ${elapsed}s）"
echo "sha256 $(sha256 "$tarball")  $(basename "$tarball")"
