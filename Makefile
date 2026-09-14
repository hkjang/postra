GO ?= go
NPM ?= npm
VERSION ?= dev

.PHONY: build build-offline frontend frontend-test frontend-check test

# Rebuild the browser bundle before embedding it in the executable.
build: frontend
	$(GO) build -trimpath -ldflags="-X postra/internal/platform/build.Version=$(VERSION)" -o postra ./cmd/postra

frontend:
	cd web && $(NPM) ci --no-audit --no-fund
	cd web && $(NPM) run build

frontend-test:
	cd web && $(NPM) run typecheck
	cd web && $(NPM) test

# Includes untracked hashes: git diff alone misses newly generated assets.
frontend-check: frontend
	@test -z "$$(git status --porcelain --untracked-files=all -- internal/transport/spa/assets)" || \
		{ git status --short -- internal/transport/spa/assets; echo "Commit the rebuilt /app assets with the frontend source." >&2; exit 1; }

# Uses the checked-in /app bundle. The Go toolchain/module cache must already
# be available offline; this target never installs or runs Node packages.
build-offline:
	GOPROXY=off GOTOOLCHAIN=local $(GO) build -trimpath -ldflags="-X postra/internal/platform/build.Version=$(VERSION)" -o postra ./cmd/postra

test:
	$(GO) test -race ./...
