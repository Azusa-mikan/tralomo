#!/usr/bin/env bash
set -euo pipefail

# ---- 配置 ----
# GitHub 仓库（owner/repo），可用环境变量 TRALOMO_REPO 覆盖。
REPO="${TRALOMO_REPO:-Azusa-mikan/tralomo}"
BIN="tralomo"
# 安装目录，可用环境变量 INSTALL_DIR 覆盖。
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
# 指定版本（如 v1.2.3）；留空则取最新 Release。
VERSION="${TRALOMO_VERSION:-}"

# 发布资产命名约定（workflow 必须一致）：
#   tralomo_<os>_<arch>.tar.gz   （os: linux/darwin，arch: amd64/arm64）
#   checksums.txt                （sha256，可选）
# 注意：/releases/latest/download/ 按「固定文件名」解析，故资产名不能带版本号。

die() { echo "错误：$*" >&2; exit 1; }

# ---- 下载器 ----
if command -v curl >/dev/null 2>&1; then
    download() { curl -fsSL "$1" -o "$2"; }
    fetch()    { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
    download() { wget -qO "$2" "$1"; }
    fetch()    { wget -qO- "$1"; }
else
    die "需要 curl 或 wget"
fi

checksum_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# ---- 系统 / 架构 ----
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
    linux | darwin) ;;
    *) die "不支持的系统：$os（可用：linux、darwin）" ;;
esac

case "$(uname -m)" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *) die "不支持的系统架构：$(uname -m)（可用：amd64、arm64）" ;;
esac

asset="${BIN}_${os}_${arch}.tar.gz"
if [[ -n "$VERSION" ]]; then
    base="https://github.com/${REPO}/releases/download/${VERSION}"
else
    base="https://github.com/${REPO}/releases/latest/download"
fi
url="${base}/${asset}"

# ---- 下载 ----
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "下载 ${url}"
download "$url" "$tmp/$asset" || die "下载失败：$url"

# ---- 校验和（发布里带 checksums.txt 时）----
if fetch "${base}/checksums.txt" >"$tmp/checksums.txt" 2>/dev/null && [[ -s "$tmp/checksums.txt" ]]; then
    want="$(grep -E "[[:space:]]${asset}\$" "$tmp/checksums.txt" | awk '{print $1}' | head -n1)"
    [[ -n "$want" ]] || die "checksums.txt 中没有 ${asset}"
    got="$(checksum_of "$tmp/$asset")"
    if [[ -z "$got" ]]; then
        echo "警告：系统缺少 sha256 工具，跳过校验" >&2
    elif [[ "$want" != "$got" ]]; then
        die "校验和不匹配（期望 ${want}，实际 ${got}）"
    else
        echo "校验和 OK"
    fi
else
    echo "警告：未获取到 checksums.txt，跳过校验" >&2
fi

# ---- 解包并安装 ----
tar -xzf "$tmp/$asset" -C "$tmp"
binpath="$tmp/$BIN"
[[ -f "$binpath" ]] || binpath="$(find "$tmp" -type f -name "$BIN" | head -n1)"
[[ -n "$binpath" && -f "$binpath" ]] || die "压缩包中找不到 ${BIN}"

install -d "$INSTALL_DIR"
install -m 0755 "$binpath" "$INSTALL_DIR/$BIN"
echo "已安装到 ${INSTALL_DIR}/${BIN}"

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        echo "提示：请将 ${INSTALL_DIR} 加入 PATH，例如："
        echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
        ;;
esac
