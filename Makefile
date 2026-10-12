# Shortcuts for the acceptance commands. CI runs the same set (see .github/workflows/ci.yml).
SHELL := /bin/bash
BIN   := ./toy

.PHONY: all fmt build vet test index-urls e2e e2e-inline check index-check index-gen site snapshot verify-toyfile clean

all: check

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "these files are not gofmt'd:"; echo "$$out"; exit 1; fi

build:
	go build -o $(BIN) ./cmd/toy

vet:
	go vet ./...

test:
	go test ./... -race -count=1

e2e:
	bash scripts/e2e.sh

# The six rules for index-inlined toyfiles. It gets its own script because it needs a real
# HTTP server: telling an upstream 404 apart from an upstream 500 is the most important of
# the six, and file:// cannot produce that distinction.
e2e-inline:
	bash scripts/e2e-inline.sh

check: fmt build vet test e2e e2e-inline

# Index regression check: actually install, run and uninstall every entry in
# index/toy-index.json.
# **Deliberately not part of check** — it needs the network and really downloads a few MB.
# It belongs to the scheduled index-check.yml line, not to what runs on every commit.
#   make index-check                          everything
#   make index-check ARGS="--only owner/repo" one entry (this is how PRs run it)
# Every declared platform's download URL, not just this machine's. index-check is
# stronger where it can run; this one is the only thing that sees darwin from linux.
index-urls:
	@bash scripts/index-urls.sh $(ARGS)

index-check:
	bash scripts/index-check.sh $(ARGS)

# Bulk-generate index entries (cmd/toy-index). **Deliberately not part of check** — it
# needs the network and calls host APIs. It is an offline batch job a maintainer runs by
# hand, not something every commit runs.
# The client (cmd/toy) never uses an API. Keeping the generator a separate binary is what
# keeps that rule checkable: grep cmd/toy and internal/ for API URLs and expect nothing.
#   make index-gen ARGS="--repos index/repos.txt"
#   make index-gen ARGS="--repos index/repos.txt --update --probe"
# Without $GITHUB_TOKEN you get 60 requests/hour and hit the rate limit fast.
index-gen:
	go run ./cmd/toy-index gen $(ARGS)

# The static site (site/). Two jobs: copy index/toy-index.json to site/index.json and
# validate it, then generate three static pages from index.template.html + i18n.json
# (Simplified Chinese / Traditional Chinese / English).
# .github/workflows/pages.yml runs exactly this script and publishes site/ to GitHub Pages.
site:
	bash scripts/build-site.sh

# Build locally the way a release does (needs goreleaser installed).
snapshot:
	goreleaser build --snapshot --clean

# The asset names in the repo-root toy.toml and in .goreleaser.yaml must match character
# for character. This builds a fake dist using goreleaser's naming, has toy init infer a
# toy.toml from it, and diffs the two [artifact] sections — any output means the template
# has drifted.
#
# The platform matrix is READ OUT of .goreleaser.yaml, not hard-coded here: hard-coded, it
# would keep passing after someone adds a goarch (say arm) to goreleaser while toy.toml
# still lacks that key — and the symptom is "no build for this platform" on that machine
# while both files look fine on their own. awk rather than a YAML library, so CI gains no
# dependency.
verify-toyfile:
	@set -euo pipefail; \
	work=$$(mktemp -d); trap 'rm -rf "$$work"' EXIT; \
	mkdir -p "$$work/dist"; \
	list() { awk -v k="$$1" '$$1==k":"{f=1;next} f&&/^[[:space:]]*-[[:space:]]/{gsub(/^[[:space:]]*-[[:space:]]*/,"");print;next} f{exit}' .goreleaser.yaml; }; \
	oses=$$(list goos); arches=$$(list goarch); \
	if [ -z "$$oses" ] || [ -z "$$arches" ]; then echo "✗ cannot read goos/goarch out of .goreleaser.yaml"; exit 1; fi; \
	n=0; \
	for o in $$oses; do for a in $$arches; do \
	  printf 'x' > "$$work/toy"; \
	  tar -czf "$$work/dist/toy_9.9.9_$${o}_$${a}.tar.gz" -C "$$work" toy; \
	  n=$$((n+1)); \
	done; done; \
	rm -f "$$work/toy"; \
	echo "read $$n platforms from .goreleaser.yaml: $$(echo $$oses | tr '\n' ' ')x $$(echo $$arches | tr '\n' ' ')"; \
	go run ./cmd/toy init --force --dist "$$work/dist" --name toy "$$work" >/dev/null 2>&1; \
	artifacts() { awk '/^\[artifact\]/{f=1;next} /^\[/{f=0} f && NF && $$1 !~ /^#/' "$$1" | tr -d ' ' | sort; }; \
	if diff <(artifacts toy.toml) <(artifacts "$$work/toy.toml"); then \
	  echo "✓ toy.toml's [artifact] matches goreleaser's asset names"; \
	else \
	  echo "✗ mismatch: left is the repo-root toy.toml, right is what the asset names imply"; exit 1; \
	fi

clean:
	rm -rf dist $(BIN)
