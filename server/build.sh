#!/usr/bin/env bash
# 交叉编译三平台单文件二进制，产物输出到 ../output/
set -e
cd "$(dirname "$0")"
OUT="${1:-../output}"
mkdir -p "$OUT"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$OUT/gifts-book-server-windows-amd64.exe" .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$OUT/gifts-book-server-linux-amd64" .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$OUT/gifts-book-server-macos-arm64" .
echo "已生成："
ls -lh "$OUT"/gifts-book-server-*
