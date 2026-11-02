# tabtv 根命令入口
#
# ⚠️ 必须兼容 GNU Make 3.81（macOS 自带）：不用 .ONESHELL、$(file ...)、!= 赋值、.RECIPEPREFIX。

GO_DIR  := backend
GO      := go
PNPM    := pnpm

VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG        := github.com/li904338203/tabtv/backend/internal/platform/buildinfo
LDFLAGS    := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildTime=$(BUILD_TIME)

.PHONY: help
help:
	@echo "tabtv 常用命令："
	@echo "  make check        门禁：scan-internal + fmt-check + lint + gen-check + typecheck + test + build"
	@echo "  make test         Go 单测 + scripts 下独立 module + 前端测试"
	@echo "  make test-it      真库集成测试（内嵌 PG，见 TT-T-04）"
	@echo "  make gen          由 api/ 契约生成 Go 与 TS 代码"
	@echo "  make fonts        下载并校验字体（不进 git）"
	@echo "  make scan         扫描内部信息"

# ---------------------------------------------------------------- 契约

.PHONY: gen gen-check
OAPI := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0

gen:
	@echo "→ 错误码 → Go + TS"
	@cd $(GO_DIR) && $(GO) run ./tools/codegen
	@echo "→ OpenAPI → Go 类型与 server 接口"
	@cd api && $(OAPI) -config oapi-codegen.yaml openapi.yaml
	@echo "→ OpenAPI → TypeScript 类型"
	@$(PNPM) exec openapi-typescript api/openapi.yaml -o packages/api-client/src/generated/api.ts
	@cd $(GO_DIR) && gofmt -w ./internal/transport
	@echo "✅ 生成完成"

# 契约漂移检测：改了契约没重跑 gen、或手改了生成物，都会在这里被拦下。
gen-check:
	@$(MAKE) --no-print-directory gen >/dev/null
	@git diff --quiet -- $(GO_DIR)/internal/transport/errcode/errcode_gen.go $(GO_DIR)/internal/transport/apigen/api_gen.go \
		packages/api-client/src/generated \
		&& echo "✅ 契约与生成物一致" \
		|| (echo "❌ 生成物与契约不一致：请运行 make gen 并提交生成结果" && git diff --stat && exit 1)

# ---------------------------------------------------------------- 检查

.PHONY: scan fmt fmt-check lint typecheck test test-go test-scripts test-race test-it fonts build build-go build-web check

scan:
	@scripts/scan-internal.sh

fmt:
	@cd $(GO_DIR) && $(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l $(GO_DIR) app main.go | tee /dev/stderr)" || (echo "上列文件未格式化，请运行 make fmt" && exit 1)

lint:
	@cd $(GO_DIR) && $(GO) vet ./...
	@$(GO) vet ./app/...

typecheck:
	@$(PNPM) run typecheck

fonts:
	@bash build/fetch-fonts.sh

test: test-go test-scripts
	@$(PNPM) run test

test-go: fonts
	@cd $(GO_DIR) && $(GO) test ./...
	@$(GO) test ./app/...

# scripts/ 下的独立 module（假上游等）不在 `go test ./...` 的范围里，单独编译与测试。
test-scripts:
	@for d in scripts/*/; do \
		[ -f "$$d/go.mod" ] || continue; \
		(cd "$$d" && $(GO) vet ./... && $(GO) test ./...) || exit 1; \
	done

test-race: fonts
	@cd $(GO_DIR) && $(GO) test -race ./...

# 集成测试：共享内嵌 PG 集群（pgembed/testcluster），不依赖 docker（TT-T-04）。
# ⚠️ 退出码取 go test 自己的（不能取管道末端的）。跑完删掉本轮的测试库（集群保留，下次免 initdb）。
IT_LOG := $(GO_DIR)/.test-it.log
test-it: fonts pg
	@sh -c 'cd $(GO_DIR) && $(GO) test -tags=integration -count=1 -p 4 ./... -v' > $(IT_LOG) 2>&1; status=$$?; \
	 grep -E "^(--- FAIL|FAIL|ok  )" $(IT_LOG) || true; \
	 echo "通过用例 $$(grep -c '^--- PASS' $(IT_LOG))，失败 $$(grep -c '^--- FAIL' $(IT_LOG))，完整日志 $(IT_LOG)"; \
	 (cd $(GO_DIR) && $(GO) run ./tools/testpg clean); \
	 exit $$status

# 停掉共享测试集群（数据目录在系统临时目录下的 tabtv-test-pg）。
.PHONY: test-clean pg
test-clean:
	@cd $(GO_DIR) && $(GO) run ./tools/testpg clean && $(GO) run ./tools/testpg stop

# 开发用 PG 二进制（按 build/third_party.lock 下载并校验）。
pg:
	@bash build/fetch-third-party.sh postgres

build: build-go build-web

# TT-T-07 合并装配前只验证能编译；之后改为构建 Wails 应用。
build-go:
	@cd $(GO_DIR) && $(GO) build -ldflags "$(LDFLAGS)" ./...

build-web:
	@$(PNPM) run build

check: scan fmt-check lint gen-check typecheck test build
	@echo ""
	@echo "✅ 门禁通过（test-it 单列）"

# 本机打安装包（与 .github/workflows/release.yml 同一组命令，TT-T-29）。VER 缺省取 git describe。
PLAT ?= $(shell $(GO) env GOOS)-$(shell $(GO) env GOARCH)
VER ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
WAILS_TAGS := $(if $(filter linux-%,$(PLAT)),-tags webkit2_40,)
BUILDINFO := github.com/li904338203/tabtv/backend/internal/platform/buildinfo
.PHONY: package
package: fonts
	@bash build/fetch-third-party.sh postgres $(PLAT)
	@bash build/fetch-third-party.sh ffmpeg $(PLAT)
	@wails build -platform $(subst -,/,$(PLAT)) $(WAILS_TAGS) -trimpath \
		-ldflags "-X $(BUILDINFO).Version=$(VER) -X $(BUILDINFO).Commit=$$(git rev-parse --short HEAD)"
	@bash build/package.sh $(PLAT) $(VER)
	@bash build/check-size.sh build/dist
