#!/usr/bin/env bash
# 编译 Windows 单文件二进制（含应用图标），产物输出到 ../output/（或指定目录）
# 图标来自 rsrc_windows_amd64.syso（已提交，go build 自动链接）。
# 若更换图标：修改 static/favicon.ico 后执行
#   go install github.com/akavel/rsrc@latest && ~/go/bin/rsrc -ico ../static/favicon.ico -o rsrc_windows_amd64.syso
set -e
cd "$(dirname "$0")"
OUT="${1:-../output}"
mkdir -p "$OUT"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$OUT/gifts-book-server-windows-amd64.exe" .
echo "已生成："
ls -lh "$OUT"/gifts-book-server-windows-amd64.exe
