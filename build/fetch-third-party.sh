#!/usr/bin/env bash
# 按 build/third_party.lock 下载并校验随包第三方二进制，解到 build/third_party/<名称>/<平台>/。
#
# 用法：build/fetch-third-party.sh [名称] [平台]
#   缺省：全部名称、当前平台（go env GOOS/GOARCH）。平台写 all 表示全部平台（打包机用）。
# 已解压且校验标记对得上的不会重下。下载缓存在 build/third_party/.cache/。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="$ROOT/build/third_party.lock"
OUT="$ROOT/build/third_party"
CACHE="$OUT/.cache"
mkdir -p "$CACHE"

want_name="${1:-}"
want_plat="${2:-$(go env GOOS)-$(go env GOARCH)}"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

extract_postgres() { # jar → 目录（jar 里是一个 .txz，解出 bin/ lib/ share/）
  local jar="$1" dest="$2" tmp
  tmp="$(mktemp -d)"
  unzip -q -o "$jar" '*.txz' -d "$tmp"
  mkdir -p "$dest"
  tar -xJf "$tmp"/*.txz -C "$dest"
  rm -rf "$tmp"
}

extract_ffmpeg() { # tar.xz（顶层一个 ffmpeg-<版本>-<平台>/ 目录）→ 目录（bin/ffmpeg[.exe]、bin/ffprobe[.exe]、licenses/、SOURCES.txt）
  local file="$1" dest="$2"
  mkdir -p "$dest"
  tar -xJf "$file" -C "$dest" --strip-components=1
  chmod +x "$dest"/bin/* 2>/dev/null || true
}

# download 下载一个地址。本仓库自己的 Release（ffmpeg 镜像）在私有阶段匿名 curl 拿到的是 404，
# 有 gh 且已登录时改用 `gh release download`（CI 里由 GH_TOKEN 提供权限）；公开之后两条路都通。
download() {
  local url="$1" out="$2"
  if [[ "$url" =~ ^https://github\.com/([^/]+/[^/]+)/releases/download/([^/]+)/([^/]+)$ ]] \
    && command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    local dir
    dir="$(mktemp -d)"
    if gh release download "${BASH_REMATCH[2]}" -R "${BASH_REMATCH[1]}" -p "${BASH_REMATCH[3]}" -D "$dir" >/dev/null 2>&1; then
      mv "$dir/${BASH_REMATCH[3]}" "$out"
      rm -rf "$dir"
      return 0
    fi
    rm -rf "$dir"
  fi
  curl -sSfL -o "$out" "$url"
}

found=0
while read -r name plat ver url sum; do
  sum="${sum%$'\r'}" # Windows 上 git 可能把锁文件检出成 CRLF：不去掉的话哈希永远对不上（.gitattributes 也钉了 LF）
  case "$name" in ''|'#'*) continue ;; esac
  [ -n "$want_name" ] && [ "$want_name" != "$name" ] && continue
  [ "$want_plat" != "all" ] && [ "$want_plat" != "$plat" ] && continue
  found=1
  dest="$OUT/$name/$plat"
  stamp="$dest/.tabtv-sha256"
  if [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$sum" ]; then
    echo "✓ $name $plat $ver 已就绪"
    continue
  fi
  file="$CACHE/$(basename "$url")"
  if [ ! -f "$file" ] || [ "$(sha256 "$file")" != "$sum" ]; then
    echo "→ 下载 $name $plat $ver"
    download "$url" "$file.part"
    mv "$file.part" "$file"
  fi
  got="$(sha256 "$file")"
  if [ "$got" != "$sum" ]; then
    echo "❌ $name $plat 校验失败：得到 ${got}，锁文件 $sum" >&2
    exit 1
  fi
  rm -rf "$dest"
  case "$name" in
    postgres) extract_postgres "$file" "$dest" ;;
    ffmpeg) extract_ffmpeg "$file" "$dest" ;;
    appimagetool|appimage-runtime) # 单文件工具：原样放好、加可执行位（只打包机用）
      mkdir -p "$dest" && cp "$file" "$dest/$name" && chmod +x "$dest/$name" ;;
    *) echo "❌ 未知组件 $name" >&2; exit 1 ;;
  esac
  echo "$sum" > "$stamp"
  echo "✓ $name $plat $ver → $dest"
done < "$LOCK"

if [ "$found" = 0 ]; then
  echo "❌ 锁文件里没有匹配项：名称=${want_name:-全部} 平台=$want_plat" >&2
  exit 1
fi
