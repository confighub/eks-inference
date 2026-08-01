# eks-inference — building the config bundles and the plugin
#
# THIS MAKEFILE IS FOR DEVELOPING THIS REPO, NOT FOR USING THE STACK.
#
# Everything a consumer does lives in the cub plugin:
#
#   cub plugin install confighub/eks-inference
#   cub eksinf install | deploy | enroll | creds | link-profile | status
#
# What remains here needs the source tree, helm, and GNU tar, and only ever runs
# here or in CI:
#
#   make render    helm charts + handwritten CRs -> configs/   (commits the diff)
#   make guard     fail on Helm constructs that do not survive flattening
#   make verify    render into a temp dir and diff against configs/ (CI drift gate)
#   make bundles   configs/ -> dist/<component>.tar.gz  (reproducible tarballs)
#   make push      dist/ -> $(REGISTRY)/<component>:latest
#   make plugin    build the eksinf binary locally
#   make check     go vet + go test + gofmt, as CI runs them
#
# Everything CI does, it does by calling these targets. There is deliberately no
# build logic inlined in the GitHub Actions workflows: if CI can do something you
# cannot reproduce locally with `make`, the flattening is not reviewable.
#
# See docs/flattening.md for why the rendered output is committed.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

include versions.env
export

# Read from components.yaml so the Makefile never drifts from the manifest.
# Only components that actually rendered are packaged — one whose renderer is not
# implemented yet has no configs/ directory and would otherwise fail bundling.
COMPONENTS = $(shell yq -r '.components[].name' components.yaml)
BUNDLEABLE = $(shell for c in $(COMPONENTS); do [ -d configs/$$c ] && echo $$c; done)

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: render
render: ## Render helm charts and copy CRs into configs/
	@scripts/render.sh configs

.PHONY: guard
guard: ## Fail on Helm constructs that do not survive flattening
	@scripts/guard.sh configs

.PHONY: verify
verify: ## Render to a temp dir and diff against committed configs/ (CI gate)
	@tmp=$$(mktemp -d); \
	scripts/render.sh "$$tmp" >/dev/null; \
	if ! diff -rq "$$tmp" configs >/dev/null 2>&1; then \
		echo ""; \
		echo "ERROR: configs/ is out of sync with the sources."; \
		echo ""; \
		diff -r "$$tmp" configs | head -60 || true; \
		echo ""; \
		echo "Run 'make render' and commit the result."; \
		rm -rf "$$tmp"; exit 1; \
	fi; \
	rm -rf "$$tmp"; \
	echo "configs/ is in sync with sources."

.PHONY: bundles
bundles: guard ## Package configs/ into reproducible dist/<component>.tar.gz
	@scripts/bundle.sh package $(BUNDLEABLE)

.PHONY: push
push: bundles ## Push bundles to $(REGISTRY)
	@scripts/bundle.sh push $(BUNDLEABLE)

.PHONY: plugin
plugin: ## Build the eksinf plugin binary locally
	@go build -o eksinf . && echo "built ./eksinf"

.PHONY: check
check: ## go vet + go test + gofmt, as CI runs them
	@go vet ./...
	@go test ./...
	@unformatted="$$(gofmt -l . | grep -v '^configs/' || true)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	@echo "check passed"

.PHONY: clean
clean: ## Remove build output
	@rm -rf dist eksinf
	@echo "removed dist/ and ./eksinf"
