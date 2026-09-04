GO ?= go
GIT ?= git
REMOTE ?= origin
RELEASE_BRANCH ?= master

BINARY ?= llm_usage
VERSION ?= dev
TAG := v$(VERSION)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: test build release

test:
	CGO_ENABLED=0 $(GO) test ./...

build:
	CGO_ENABLED=0 $(GO) build \
	-trimpath \
	-ldflags "$(LDFLAGS)" \
	-o "$(BINARY)" \
	.

release: test
	@set -eu; \
	if ! printf '%s\n' "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "usage: make release VERSION=1.2.3" >&2; \
		exit 2; \
	fi; \
	if [ "$$( $(GIT) branch --show-current )" != "$(RELEASE_BRANCH)" ]; then \
		echo "release must be run from the $(RELEASE_BRANCH) branch" >&2; \
		exit 1; \
	fi; \
	if [ -n "$$($(GIT) status --porcelain)" ]; then \
		echo "release requires a clean working tree" >&2; \
		exit 1; \
	fi; \
	$(GIT) fetch --no-tags "$(REMOTE)" "$(RELEASE_BRANCH)"; \
	remote_head="$$($(GIT) rev-parse --verify "$(REMOTE)/$(RELEASE_BRANCH)")"; \
	local_head="$$($(GIT) rev-parse --verify HEAD)"; \
	if [ "$$local_head" != "$$remote_head" ]; then \
		echo "local HEAD must match $(REMOTE)/$(RELEASE_BRANCH); push the branch first" >&2; \
		exit 1; \
	fi; \
	if $(GIT) rev-parse --verify --quiet "refs/tags/$(TAG)" >/dev/null; then \
		echo "tag $(TAG) already exists locally" >&2; \
		exit 1; \
	fi; \
	if $(GIT) ls-remote --exit-code --refs "$(REMOTE)" "refs/tags/$(TAG)" >/dev/null 2>&1; then \
		echo "tag $(TAG) already exists on $(REMOTE)" >&2; \
		exit 1; \
	fi; \
	tmp_binary="$$(mktemp)"; \
	trap 'rm -f "$$tmp_binary"' EXIT; \
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$$tmp_binary" .; \
	actual_version=$$("$$tmp_binary" --version); \
	if [ "$$actual_version" != "$(VERSION)" ]; then \
		echo "built binary reports $$actual_version, expected $(VERSION)" >&2; \
		exit 1; \
	fi; \
	$(GIT) tag -a "$(TAG)" -m "Release $(TAG)"; \
	$(GIT) push "$(REMOTE)" "$(TAG)"
