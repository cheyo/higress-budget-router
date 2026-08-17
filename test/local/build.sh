#!/usr/bin/env bash
# 在容器里构建 main.wasm —— 本机不装 Go 也能出产物。
#
# 等价于 Makefile 的 test + vet + wasm，区别是 go test 必须在默认 GOOS 下跑
# （wasip1 目标下测试二进制无法在容器里直接执行）。
#
# GOPROXY 走国内代理；如果你的网络能直连，可以 GOPROXY=direct ./build.sh

cd "$(dirname "$0")/../.."
source test/local/lib.sh

GO_IMAGE=${GO_IMAGE:-golang:1.24}
GOPROXY_VAL=${GOPROXY:-https://goproxy.cn,direct}
SRC=$(pwd -W 2>/dev/null || pwd)   # git-bash 下取 Windows 形式路径给 docker -v

docker run --rm -v "$SRC:/src" -w /src \
  -e GOFLAGS=-buildvcs=false -e GOPROXY="$GOPROXY_VAL" \
  "$GO_IMAGE" sh -c '
set -e
echo "### go test";        go test ./... -count=1
echo "### go vet wasip1";  GOOS=wasip1 GOARCH=wasm go vet ./...
echo "### go build wasip1"; GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm ./
ls -l main.wasm
'
