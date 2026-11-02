#!/usr/bin/env bash
# 把 `wails build` 的产物与随包资源装成安装包（TT-T-29，dev/04 §3、R-7）。CI 与 `make package` 共用。
#
# 用法：build/package.sh <平台 GOOS-GOARCH> <版本（不带 v）>
#   前提：已执行 `wails build`（产物在 build/bin/）、build/fetch-third-party.sh <平台>、build/fetch-fonts.sh。
#   产出：build/dist/ 下的安装包（文件名照 dev/04 R-7，自动更新按名字找包，改名会让应用内更新失效）。
#
# 随包资源的布局（程序按这个找，见 platform/resources、ffbin、typeset）：
#   resources/pg/{bin,lib,share}   内嵌 PostgreSQL
#   resources/bin/ffmpeg|ffprobe   LGPL ffmpeg；Linux 另有 resources/lib/（ffmpeg 的共享库，二进制按 $ORIGIN/../lib 找）
#   resources/fonts/               字幕排版字体
#   resources/licenses/            全部许可原文（关于页「第三方许可证」）
# 位置：可执行文件旁的 resources/；macOS 是 tabtv.app/Contents/Resources/。
set -euo pipefail

PLAT="${1:?用法：build/package.sh <GOOS-GOARCH> <版本>}"
VER="${2:?缺版本号}"
VER="${VER#v}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OS="${PLAT%-*}"
ARCH="${PLAT#*-}"
TP="$ROOT/build/third_party"
DIST="$ROOT/build/dist"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$DIST"

die() { echo "❌ $*" >&2; exit 1; }

# ---------------------------------------------------------------- 资源
RES="$STAGE/resources"
mkdir -p "$RES/licenses"
[ -d "$TP/postgres/$PLAT/bin" ] || die "缺 PostgreSQL（先跑 build/fetch-third-party.sh postgres $PLAT）"
[ -d "$TP/ffmpeg/$PLAT/bin" ] || die "缺 ffmpeg（先跑 build/fetch-third-party.sh ffmpeg $PLAT）"
FONTS="$ROOT/backend/assets/fonts"
ls "$FONTS"/*.otf >/dev/null 2>&1 || die "缺字体（先跑 build/fetch-fonts.sh）"

cp -R "$TP/postgres/$PLAT" "$RES/pg"
rm -f "$RES/pg/.tabtv-sha256"
# ⚠️ zonky 的 PG 包把库的版本别名（libicudata.dylib → .68.dylib → .68.2.dylib）存成了三份完整文件，
#    ICU 一项就三份 57MB。同目录内内容相同的文件只留名字最长的那份（真实版本号），其余换成相对符号链接。
#    Windows 的 DLL 没有这种别名，也不适合用符号链接，跳过。
dedupe_libs() {
  local dir="$1"
  [ -d "$dir" ] || return 0
  python3 - "$dir" <<'PY'
import hashlib, os, sys
d = sys.argv[1]
groups = {}
for n in os.listdir(d):
    p = os.path.join(d, n)
    if os.path.islink(p) or not os.path.isfile(p):
        continue
    h = hashlib.sha256(open(p, "rb").read()).hexdigest()
    groups.setdefault(h, []).append(n)
saved = 0
for names in groups.values():
    if len(names) < 2:
        continue
    keep = max(names, key=len)
    for n in names:
        if n != keep:
            p = os.path.join(d, n)
            saved += os.path.getsize(p)
            os.remove(p)
            os.symlink(keep, p)
print(f"  去重 {d}：省下 {saved // (1 << 20)}MB")
PY
}
if [ "$OS" != "windows" ]; then dedupe_libs "$RES/pg/lib"; fi
cp -R "$TP/ffmpeg/$PLAT/bin" "$RES/bin"
[ -d "$TP/ffmpeg/$PLAT/lib" ] && cp -R "$TP/ffmpeg/$PLAT/lib" "$RES/lib"
mkdir -p "$RES/fonts"
cp "$FONTS"/*.otf "$FONTS"/*.ttf "$RES/fonts/" 2>/dev/null || true
# 许可：项目本身、PostgreSQL、ffmpeg 及其依赖（含构建参数与源码地址）、字体。
cp "$ROOT/LICENSE" "$ROOT/NOTICE" "$RES/licenses/"
cp "$ROOT"/build/licenses/*.txt "$RES/licenses/" 2>/dev/null || true
[ -f "$ROOT/build/licenses/dependencies.json" ] && cp "$ROOT/build/licenses/dependencies.json" "$RES/licenses/"
if [ -d "$TP/ffmpeg/$PLAT/licenses" ]; then
  mkdir -p "$RES/licenses/ffmpeg" && cp -R "$TP/ffmpeg/$PLAT/licenses/." "$RES/licenses/ffmpeg/"
fi
for f in SOURCES.txt buildconf.txt; do
  [ -f "$TP/ffmpeg/$PLAT/$f" ] && cp "$TP/ffmpeg/$PLAT/$f" "$RES/licenses/ffmpeg-$f"
done
cp "$FONTS"/*.txt "$RES/licenses/" 2>/dev/null || true

# ---------------------------------------------------------------- 各平台
case "$OS" in
  darwin)
    APP="$ROOT/build/bin/tabtv.app"
    [ -d "$APP" ] || die "缺 $APP（先 wails build -platform darwin/$ARCH）"
    cp -R "$APP" "$STAGE/tabtv.app"
    cp -R "$RES/." "$STAGE/tabtv.app/Contents/Resources/"
    # 未签名：去掉 ffmpeg / PG 二进制可能带的隔离属性（从 Release 下载解压来的）。
    xattr -cr "$STAGE/tabtv.app" 2>/dev/null || true
    # ⚠️ 只做 ad-hoc 签名（不是开发者签名）：Apple 芯片上未签名的 Mach-O 根本不让运行，
    #    ad-hoc 签名之后仍会被 Gatekeeper 拦一次（README 写了「右键 → 打开」）。
    codesign --force --deep --sign - "$STAGE/tabtv.app"
    mkdir -p "$STAGE/dmg" && mv "$STAGE/tabtv.app" "$STAGE/dmg/" && ln -s /Applications "$STAGE/dmg/Applications"
    OUT="$DIST/tabtv-$VER-darwin-$ARCH.dmg"
    rm -f "$OUT"
    hdiutil create -volname "tabtv" -srcfolder "$STAGE/dmg" -ov -format UDZO "$OUT" >/dev/null
    ;;
  windows)
    EXE="$ROOT/build/bin/tabtv.exe"
    [ -f "$EXE" ] || die "缺 $EXE（先 wails build -platform windows/$ARCH）"
    command -v makensis >/dev/null 2>&1 || die "缺 makensis（NSIS）"
    mkdir -p "$STAGE/app"
    cp "$EXE" "$STAGE/app/"
    cp -R "$RES" "$STAGE/app/resources"
    OUT="$DIST/tabtv-$VER-windows-$ARCH-setup.exe"
    # makensis 在 Windows 上要 Windows 路径。
    to_native() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else echo "$1"; fi; }
    makensis -V2 -DVERSION="$VER" -DSRC="$(to_native "$STAGE/app")" -DOUT="$(to_native "$OUT")" \
      -DICON="$(to_native "$ROOT/build/windows/icon.ico")" "$(to_native "$ROOT/build/windows/tabtv.nsi")"
    ;;
  linux)
    BIN="$ROOT/build/bin/tabtv"
    [ -f "$BIN" ] || die "缺 $BIN（先 wails build -tags webkit2_40）"
    # ---- AppImage：可执行文件与 resources/ 放在一起（resources.Root 按可执行文件旁找）
    APPDIR="$STAGE/AppDir"
    mkdir -p "$APPDIR/usr/bin"
    cp "$BIN" "$APPDIR/usr/bin/tabtv"
    cp -R "$RES" "$APPDIR/usr/bin/resources"
    cp "$ROOT/build/linux/tabtv.desktop" "$APPDIR/tabtv.desktop"
    cp "$ROOT/build/appicon.png" "$APPDIR/tabtv.png"
    cat > "$APPDIR/AppRun" <<'EOF'
#!/bin/sh
HERE="$(dirname "$(readlink -f "$0")")"
exec "$HERE/usr/bin/tabtv" "$@"
EOF
    chmod +x "$APPDIR/AppRun"
    TOOL="$TP/appimagetool/linux-amd64/appimagetool"
    RT="$TP/appimage-runtime/linux-amd64/appimage-runtime"
    [ -x "$TOOL" ] && [ -f "$RT" ] || die "缺 appimagetool / 运行时（build/fetch-third-party.sh appimagetool linux-amd64 等）"
    OUT="$DIST/tabtv-$VER-linux-$ARCH.AppImage"
    # --appimage-extract-and-run：CI 机器上没有 FUSE。
    ARCH=x86_64 "$TOOL" --appimage-extract-and-run --no-appstream --runtime-file "$RT" "$APPDIR" "$OUT" >/dev/null
    # ---- .deb：装到 /opt/tabtv，没有自动更新（没有权限改系统目录，dev/04 R-7）
    DEB="$STAGE/deb"
    mkdir -p "$DEB/DEBIAN" "$DEB/opt/tabtv" "$DEB/usr/share/applications" "$DEB/usr/share/icons/hicolor/512x512/apps" "$DEB/usr/bin"
    cp "$BIN" "$DEB/opt/tabtv/tabtv"
    cp -R "$RES" "$DEB/opt/tabtv/resources"
    ln -s /opt/tabtv/tabtv "$DEB/usr/bin/tabtv"
    sed 's#^Exec=.*#Exec=/opt/tabtv/tabtv#' "$ROOT/build/linux/tabtv.desktop" > "$DEB/usr/share/applications/tabtv.desktop"
    cp "$ROOT/build/appicon.png" "$DEB/usr/share/icons/hicolor/512x512/apps/tabtv.png"
    DEBARCH="$ARCH"
    cat > "$DEB/DEBIAN/control" <<EOF
Package: tabtv
Version: $VER
Section: video
Priority: optional
Architecture: $DEBARCH
Maintainer: tabtv contributors
Depends: libgtk-3-0, libwebkit2gtk-4.0-37
Recommends: gstreamer1.0-libav, gstreamer1.0-plugins-good
Description: 开源的 AI 画布与视频创作桌面应用
EOF
    dpkg-deb --root-owner-group --build "$DEB" "$DIST/tabtv_${VER}_${DEBARCH}.deb" >/dev/null
    ;;
  *) die "不支持的平台 $PLAT" ;;
esac

echo "✓ $PLAT → $(ls -1 "$DIST" | grep -E "$VER" | tr '\n' ' ')"
