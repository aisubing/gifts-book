#!/usr/bin/env bash
# 编译 Windows 单文件二进制，产物输出到 ../output/（或指定目录）
set -e
cd "$(dirname "$0")"
OUT="${1:-../output}"
mkdir -p "$OUT"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$OUT/gifts-book-server-windows-amd64.exe" .
echo "已生成："
ls -lh "$OUT"/gifts-book-server-windows-amd64.exe
