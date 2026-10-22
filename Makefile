BINARY := pmss-fleet-manager
# Stamped into the binary as main.version. The release workflow overrides
# this with the tag; local builds report the git description, or "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run test vet jscheck notices notices-check license-headers license-check clean

# `notices` runs FIRST: THIRD_PARTY_LICENSES.md is embedded into the binary
# (legal.go), so regenerating it here means what ships always matches what
# is actually linked in — no separate step to forget after a dependency bump.
# The committed copy exists only so a bare `go build` still works.
build: notices
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/$(BINARY)

run:
	go run ./cmd/$(BINARY)

test:
	go test ./...

vet:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

# The frontend has no build step and no test runner, so the only thing
# that catches a reference to an undefined name (a ReferenceError at
# click time, invisible to `node --check`) is a linter. ESLint is the one
# npm dependency, pinned with its whole tree by package-lock.json and
# installed with `npm ci` (scripts off: nothing from the registry runs at
# install), so CI and a release lint with exactly the code the lockfile
# names. The small committed config checks no-undef, unreachable code and
# duplicate keys. Needs node; without npm it is skipped with a notice
# locally, but under CI that is a failure, never a weaker pass.
jscheck:
	node --check internal/web/static/app.js
	node --check internal/web/static/revisions.js
	node --test internal/web/revisions_test.mjs
	@if command -v npm >/dev/null 2>&1; then \
		npm ci --ignore-scripts --no-audit --no-fund --silent && \
		npm exec --no -- eslint -c scripts/eslint.config.mjs internal/web/static/app.js internal/web/static/revisions.js internal/web/revisions_test.mjs; \
	elif [ -n "$$CI" ]; then echo "jscheck: npm not found and CI is set; ESLint is required here" >&2; exit 1; \
	else echo "jscheck: npm not found, ESLint skipped"; fi

# Regenerate the third-party notices from the modules actually linked in.
notices:
	@./scripts/gen-third-party-licenses.sh >/dev/null

# Fail if the committed notices are stale. Deliberately NOT part of the CI
# gate: Dependabot bumps go.mod but cannot regenerate this file, so gating
# every PR on it would make each gomod PR red for a reason the bot can't fix.
# The release workflow runs it instead — that is the point where the notice
# legally has to match the artifact being published.
notices-check:
	@tmp=$$(mktemp); \
	./scripts/gen-third-party-licenses.sh "$$tmp" >/dev/null; \
	if ! diff -q "$$tmp" THIRD_PARTY_LICENSES.md >/dev/null; then \
		echo "FAIL: THIRD_PARTY_LICENSES.md is stale — run 'make notices'"; \
		diff -u THIRD_PARTY_LICENSES.md "$$tmp" | head -40; \
		rm -f "$$tmp"; exit 1; \
	fi; \
	rm -f "$$tmp"; \
	echo "notices-check: THIRD_PARTY_LICENSES.md is current"

# Stamp the AGPL notice on any first-party source missing it.
license-headers:
	@./scripts/license-headers.sh

# Cheap and deterministic (no network, no build), so this one DOES gate CI.
license-check:
	@./scripts/license-headers.sh --check

clean:
	rm -f $(BINARY)
