# 五条验收命令的快捷方式。CI 跑的是同一批（见 .github/workflows/ci.yml）。
SHELL := /bin/bash
BIN   := ./toy

.PHONY: all fmt build vet test e2e e2e-inline check index-check index-gen snapshot verify-toyfile clean

all: check

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "这些文件没格式化:"; echo "$$out"; exit 1; fi

build:
	go build -o $(BIN) ./cmd/toy

vet:
	go vet ./...

test:
	go test ./... -race -count=1

e2e:
	bash scripts/e2e.sh

# 索引内联 toyfile 的六条规则。单独一个脚本是因为它要起一个真的 HTTP 服务器：
# 「上游 404」和「上游 500」的区别是那六条里最要紧的一条，file:// 造不出来。
e2e-inline:
	bash scripts/e2e-inline.sh

check: fmt build vet test e2e e2e-inline

# 索引回归检查：把 index/toy-index.json 里的每一条真的装一遍、跑一遍、卸一遍。
# **故意不进 check** —— 它要联网、要真的下载几 MB，属于 index-check.yml 那条
# 定时线，不属于每次提交都跑的那一批。
#   make index-check                          全量
#   make index-check ARGS="--only owner/repo" 只测一条（PR 上就是这么跑的）
index-check:
	bash scripts/index-check.sh $(ARGS)

# 批量生成索引条目（cmd/toy-index）。**故意不进 check** —— 它要联网、要调
# 托管方的 API，是维护者手动跑的离线批处理，不是每次提交都跑的那一批。
# 客户端（cmd/toy）永远不用 API，这两个二进制分开的理由见 the design notes §14.11。
#   make index-gen ARGS="--repos index/repos.txt"
#   make index-gen ARGS="--repos index/repos.txt --update --probe"
# 不带 $GITHUB_TOKEN 只有 60 次/小时，很快就撞限流。
index-gen:
	go run ./cmd/toy-index gen $(ARGS)

# 本地按 release 的方式构建一遍（要装了 goreleaser）。
snapshot:
	goreleaser build --snapshot --clean

# 仓库根的 toy.toml 和 .goreleaser.yaml 的资产名必须逐字对得上。
# 这里造一份和 goreleaser 同名的假 dist，用 toy init 反推一份 toy.toml，
# 再 diff 两边的 [artifact] 段 —— 有输出就是模板写歪了。
#
# 平台矩阵是从 .goreleaser.yaml 里【读出来】的，不是写死在这里的：写死的话
# 给 goreleaser 加一个 goarch（比如 arm）之后，这个检查照样打勾，而 toy.toml
# 少了那一段 —— 症状是那台机器上「没有适配当前平台的构建」，而两个文件各自
# 看都没毛病。用 awk 而不是 yaml 库，是为了不给 CI 加一个依赖。
verify-toyfile:
	@set -euo pipefail; \
	work=$$(mktemp -d); trap 'rm -rf "$$work"' EXIT; \
	mkdir -p "$$work/dist"; \
	list() { awk -v k="$$1" '$$1==k":"{f=1;next} f&&/^[[:space:]]*-[[:space:]]/{gsub(/^[[:space:]]*-[[:space:]]*/,"");print;next} f{exit}' .goreleaser.yaml; }; \
	oses=$$(list goos); arches=$$(list goarch); \
	if [ -z "$$oses" ] || [ -z "$$arches" ]; then echo "✗ 从 .goreleaser.yaml 里读不出 goos/goarch"; exit 1; fi; \
	n=0; \
	for o in $$oses; do for a in $$arches; do \
	  printf 'x' > "$$work/toy"; \
	  tar -czf "$$work/dist/toy_9.9.9_$${o}_$${a}.tar.gz" -C "$$work" toy; \
	  n=$$((n+1)); \
	done; done; \
	rm -f "$$work/toy"; \
	echo "从 .goreleaser.yaml 读出 $$n 个平台：$$(echo $$oses | tr '\n' ' ')× $$(echo $$arches | tr '\n' ' ')"; \
	go run ./cmd/toy init --force --dist "$$work/dist" --name toy "$$work" >/dev/null 2>&1; \
	artifacts() { awk '/^\[artifact\]/{f=1;next} /^\[/{f=0} f && NF && $$1 !~ /^#/' "$$1" | tr -d ' ' | sort; }; \
	if diff <(artifacts toy.toml) <(artifacts "$$work/toy.toml"); then \
	  echo "✓ toy.toml 的 [artifact] 和 goreleaser 的资产名对得上"; \
	else \
	  echo "✗ 对不上：左边是仓库根的 toy.toml，右边是从资产名反推出来的"; exit 1; \
	fi

clean:
	rm -rf dist $(BIN)
