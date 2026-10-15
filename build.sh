#!/bin/sh
# build.sh —— 一次编译三平台交付二进制，并处理 winres.syso 的跨平台冲突。
#
# 背景：
#   winres.syso 是 Windows 专用的 COFF 资源目标文件（图标 + 版本信息），
#   Go 在构建 windows 目标时会自动链接同目录下的 *.syso。
#   但构建 linux 目标时，Go 也会尝试解析它，导致：
#       unknown ARM64 relocation type 7
#   因此 Linux 构建前必须把 *.syso 临时移开，构建完再移回。
#
# 用法：
#   sh build.sh [输出目录]     默认输出到 ./dist
#
# 依赖：Go 1.19（Win7 兼容性硬要求，见 README）

set -e

OUT="${1:-dist}"
GO="${GO:-go}"
UPX="${UPX:-}"

mkdir -p "$OUT"

LDFLAGS="-s -w"
BUILDFLAGS="-trimpath -buildvcs=false"

echo "== 目标平台 =="
echo "  windows/386      -> $OUT/pdfcompress.exe"
echo "  linux/arm64      -> $OUT/pdfcompress-linux-arm64"
echo "  linux/amd64      -> $OUT/pdfcompress-linux-x86_64"
echo

# ---- Windows / 386 ----
echo "[1/3] 构建 windows/386 ..."
CGO_ENABLED=0 GOOS=windows GOARCH=386 \
  "$GO" build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUT/pdfcompress.exe" .

# ---- Linux 目标：临时移开 syso ----
SYSO_MOVED=0
if ls ./*.syso >/dev/null 2>&1; then
  mkdir -p .syso_backup
  mv ./*.syso .syso_backup/
  SYSO_MOVED=1
  echo "      （已临时移开 *.syso，供 Linux 链接）"
fi

restore_syso() {
  if [ "$SYSO_MOVED" = "1" ] && ls .syso_backup/*.syso >/dev/null 2>&1; then
    mv .syso_backup/*.syso ./
    rmdir .syso_backup 2>/dev/null || true
  fi
}
trap restore_syso EXIT

echo "[2/3] 构建 linux/arm64 ..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  "$GO" build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUT/pdfcompress-linux-arm64" .

echo "[3/3] 构建 linux/amd64 ..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  "$GO" build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUT/pdfcompress-linux-x86_64" .

restore_syso
trap - EXIT

# ---- 可选 UPX 加壳 ----
if [ -n "$UPX" ] && command -v "$UPX" >/dev/null 2>&1; then
  echo
  echo "== UPX 加壳 =="
  "$UPX" -9 "$OUT/pdfcompress.exe" || true
fi

echo
echo "== 产物 =="
ls -la "$OUT"
