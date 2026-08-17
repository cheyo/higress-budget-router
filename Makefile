PLUGIN_NAME ?= higress-budget-router
VERSION     ?= $(shell cat VERSION)
REGISTRY    ?= ghcr.io/OWNER

IMAGE := $(REGISTRY)/$(PLUGIN_NAME):$(VERSION)

.PHONY: all build guards test vet wasm image push clean help

all: build ## 等同于 build

build: guards test vet wasm ## 守卫 + 测试 + vet + 编译 wasm

guards: ## 源码守卫：禁止覆盖式写日志属性
	@# SetUserAttributeMap 在 SDK 里是【整体替换】不是合并（ctx.userAttribute = kvmap）。
	@# 本插件的观测字段需要跨请求/响应阶段保留，一律用逐键的 SetUserAttribute。
	@if grep -rnE '\.SetUserAttributeMap\(' --include='*.go' . ; then \
		echo ""; \
		echo "✗ 禁止使用 SetUserAttributeMap —— 它会整体替换属性表，请改用逐键 SetUserAttribute"; \
		exit 1; \
	fi
	@echo "✓ guards passed"

test: ## 跑单元测试
	go test ./... -count=1

cover: ## 测试并输出覆盖率
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

vet: ## wasm 目标下的 go vet
	GOOS=wasip1 GOARCH=wasm go vet ./...

wasm: ## 编译出 main.wasm
	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o main.wasm ./
	@ls -lh main.wasm

image: wasm ## 打成 OCI 镜像
	docker build -t $(IMAGE) .
	@echo "built $(IMAGE)"

push: image ## 推送镜像；在 WasmPlugin.spec.url 里用 oci://$(IMAGE)
	docker push $(IMAGE)
	@echo
	@echo "  url: oci://$(IMAGE)"

clean: ## 清理产物
	rm -f main.wasm coverage.out

help: ## 显示本帮助
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'
