# Local gate (US-001) — `make check` mirrors CI exactly.
# Stages: lint -> test -> build -> smoke (smoke owns its compose lifecycle).

GO_MODULES := shared/go services/catalog services/inventory services/commerce services/payments services/access gateway smoke
# smoke's tests are build-tagged and run in the smoke stage, not test-go
GO_TEST_MODULES := $(filter-out smoke,$(GO_MODULES))
BIN := $(CURDIR)/bin
# pinned so local and CI runs use the same linter (latest at time of pinning).
# v2.13.2 is built with go1.27 — check-go-toolchain asserts that against the
# active Go, so this pin and the CI go-version move together, never apart.
GOLANGCI_VERSION := v2.13.2
# Named after the pin: a version bump names a path that does not exist yet, so
# make installs it instead of reusing the binary already sitting in ./bin.
GOLANGCI := $(BIN)/golangci-lint-$(GOLANGCI_VERSION)

# The smoke stack runs isolated (own compose project + shifted ports);
# lifecycle and env live in scripts/smoke.sh.

.PHONY: env-bootstrap check lint test build smoke smoke-hermetic browser onsale-load-full lint-go lint-ts test-go test-ts build-go build-ts build-gate-linux generate check-generate check-dep-drift check-build-list-lag check-go-standalone check-security-workflow-trigger check-hermetic-workflow-trigger check-adr-numbers check-markdown-links check-hooks check-go-toolchain check-all gate-lock-held check-required-env up down clean

# `make check` IS the wrapper. It holds .gate.lock so nothing edits the tree
# mid-run (TKT-240), captures the log, and writes .gate.verdict from the exit
# code AND the log body — a chained, piped or backgrounded gate otherwise reports
# someone else's status (TKT-71/87/94/101/235). The stages live in check-all,
# which refuses to start without the lock, so there is no spelling of the gate —
# `sh -c`, `eval`, `nohup`, a newline — that runs unlocked.
check:
	@./scripts/gate.sh

check-all: gate-lock-held deps check-generate check-dep-drift check-build-list-lag check-security-workflow-trigger check-hermetic-workflow-trigger check-adr-numbers check-markdown-links check-hooks check-go-toolchain lint check-go-standalone test build smoke

# The lock must be THIS run's, not merely present: a hand-made or stale lock is
# not evidence that a wrapper is holding the tree still. GATE_LOCK is a variable
# so the guard self-test can exercise it without touching the real lock.
GATE_LOCK ?= .gate.lock

gate-lock-held:
	@[ -n "$$GATE_TOKEN" ] && [ -e "$(GATE_LOCK)" ] \
		&& [ "$$(cut -d' ' -f1 < $(GATE_LOCK))" = "$$GATE_TOKEN" ] \
		|| { echo "check-all is internal: run 'make check' (or scripts/gate.sh), which takes the lock first." >&2; exit 2; }

## ---- deps (self-contained gate: clean clone needs nothing pre-installed) ----
deps:
	pnpm install --frozen-lockfile

## ---- generate (contract-first, ADR-009: spec is the source of truth) ----
generate:
	./scripts/generate-api.sh

# The gate fails when committed generated code drifts from the spec.
GENERATED_API_OUTPUTS := $(shell ./scripts/generate-api.sh outputs)
check-generate: generate
	@./scripts/generate-api.sh verify-tracked
	@git diff --exit-code HEAD -- $(GENERATED_API_OUTPUTS) \
		|| { echo "generated code drifted from the OpenAPI spec — commit the output of 'make generate'" >&2; exit 1; }

## ---- dependency declarations (TKT-129, ADR-035: one version per shared dep) ----
# Manifest-only parse, offline, ~0.2s. `go mod tidy` is a no-op on a drifted tree,
# so nothing else in the gate would ever notice.
check-dep-drift:
	@./scripts/check-go-dependency-drift.sh $(GO_MODULES)

# The VERTICAL direction (TKT-265): a manifest declaring less than the workspace
# selects. Separate script because this one resolves the module graph via
# `go list -m` and so is NOT offline — check-dep-drift's contract above is, and
# folding them together would falsify it. `deps` already needs the network on a
# cold clone, so the gate loses nothing it had.
check-build-list-lag:
	@./scripts/check-go-build-list-lag.sh $(GO_MODULES)

# lint-go loads the workspace dependency graph first. The standalone pass then
# disables the workspace and the network, so every module must carry its own
# complete readonly manifest and checksums.
check-go-standalone: lint-go
	@./scripts/check-go-standalone.sh $(GO_MODULES)

## ---- workflow triggers ----
check-security-workflow-trigger:
	@./scripts/check-security-workflow-trigger.sh

check-hermetic-workflow-trigger:
	@./scripts/check-hermetic-workflow-trigger.sh

## ---- ADR registry (an ADR number is a reference target) ----
# Two ADRs both numbered 055 made every bare `ADR-055` citation in code,
# migrations, OpenAPI and AGENTS.md ambiguous, and nothing in the gate noticed.
check-adr-numbers:
	@./scripts/check-adr-numbers.sh

## ---- agent guards (.claude/hooks) ----
# The PreToolUse guards refuse a merge that cannot contain the work, an
# attributed commit, and anything but waiting and reading while the gate holds
# the lock. A guard that fails open is silent, so the seeded-violation test runs
# in the gate like everything else. The hook cases work in a throwaway repo, so
# holding .gate.lock here does not skew them.
check-hooks:
	@./.claude/hooks/selftest.sh

## ---- Go toolchain agreement (local vs the pinned linter) ----
# A linter built with an older Go minor cannot type-check against a newer Go
# stdlib: it panics mid-lint, which reads as a code defect. Cheap, so it runs
# before the expensive stages rather than after them.
check-go-toolchain: $(GOLANGCI)
	@./scripts/check-go-toolchain.sh --selftest
	@./scripts/check-go-toolchain.sh $(GOLANGCI)

## ---- documentation cross-references ----
# ADR-062 pointed at a filename ADR-010 never had, so an inherited locking
# decision was unreachable and nothing noticed. Local and network-free: external
# URLs are never fetched.
check-markdown-links:
	@./scripts/check-markdown-links.sh

## ---- stack credential bootstrap (TKT-227) ----
# Deliberately NOT in `make check`: it bootstraps into a sandbox, which mints two
# Ed25519 pairs via `go run` and costs seconds, and `make check` never starts the
# `make up` stack it protects. It runs in CI as part of the gate-selftest job,
# where its mutation seed lives — that is the standing guard.
check-required-env:
	@./scripts/check-required-env.sh

## ---- lint ----
lint: lint-go lint-ts

$(GOLANGCI):
	./scripts/install-golangci-lint.sh $(GOLANGCI_VERSION) $(BIN) $(notdir $(GOLANGCI))

lint-go: $(GOLANGCI)
	@for m in $(GO_MODULES); do \
		echo "golangci-lint: $$m"; \
		(cd $$m && $(GOLANGCI) run --build-tags smoke ./...) || exit 1; \
	done

lint-ts:
	pnpm -r lint

## ---- test ----
test: test-go test-ts

# -count=1 disables Go's test cache, deliberately (TKT-210).
#
# shared/go/cachetier's spec audit reads services/*/api/openapi.yaml — files
# OUTSIDE its own module. Go's test caching does not track those as inputs, so
# after a spec change it kept replaying a stale PASS: the audit was green locally
# through four gate runs and only failed in CI, which has no warm cache. A gate
# that can report a pass for code it did not run is not a gate, and this repo has
# been bitten by a falsely-green gate signal twice before (TKT-94, TKT-101).
#
# The cost is seconds on unit tests the gate already spends minutes beside.
test-go:
	@for m in $(GO_TEST_MODULES); do \
		echo "go test: $$m"; \
		(cd $$m && go test -count=1 ./...) || exit 1; \
	done

test-ts:
	pnpm -r test

## ---- build ----
build: build-go build-ts

build-go:
	@for m in $(GO_MODULES); do \
		echo "go build: $$m"; \
		(cd $$m && { [ -d cmd ] && go build -o $(BIN)/gate/ ./... || go build ./...; } && go vet -tags smoke ./...) || exit 1; \
	done

build-ts:
	pnpm -r build

# Static linux binaries for the smoke packaging images (compose.smoke.yaml).
# Separate from build-go: smoke images run distroless/static, so these must
# be CGO-free and linux-targeted regardless of the host.
GO_GATE_PKGS := \
	catalog=ticketing/services/catalog/cmd/catalog \
	inventory=ticketing/services/inventory/cmd/inventory \
	commerce=ticketing/services/commerce/cmd/commerce \
	payments=ticketing/services/payments/cmd/payments \
	access=ticketing/services/access/cmd/access \
	gateway=ticketing/gateway/cmd/gateway

build-gate-linux:
	@mkdir -p $(BIN)/gate
	@for e in $(GO_GATE_PKGS); do \
		n=$${e%%=*}; p=$${e#*=}; \
		echo "go build (linux static): $$n"; \
		CGO_ENABLED=0 GOOS=linux go build -trimpath -o $(BIN)/gate/$$n $$p || exit 1; \
	done

## ---- smoke (integration seam; owns the stack lifecycle) ----
# Per-PR smoke runs against host-built artifacts (fast). The hermetic
# in-Docker build path is covered by `smoke-hermetic` (scheduled CI +
# triggered on PRs touching the build files) — see docs/testing.md.
smoke: build-gate-linux build-ts
	./scripts/smoke.sh

smoke-hermetic:
	SMOKE_HERMETIC=1 ./scripts/smoke.sh

## ---- browser-submit gate (AGENTS.md) ----
# Deliberately NOT part of `make check`: it drives the host's real Chrome, so CI
# cannot run it and a developer without one must still be able to pass the gate.
# Run it for any ticket that adds or changes a storefront/back-office write form.
# Smoke submits selected forms with a Go client, but that client chooses the target,
# headers, cookies, and redirects itself. It cannot prove the rendered action,
# browser-generated Origin/Referer and SameSite behavior, JavaScript, or CSP.
# See scripts/browser.sh (TKT-228; review R6, 2026-08-19).
browser: build-gate-linux build-ts
	./scripts/browser.sh

# Full festival-NFR load profile (TKT-82) — on-demand, not part of `make check`.
# Runs the whole smoke suite with the on-sale profile switched to full and
# writes the machine-readable evidence to docs/evidence/TKT-82/.
onsale-load-full: build-gate-linux build-ts
	ONSALE_PROFILE=full \
	ONSALE_REPORT=$(CURDIR)/docs/evidence/TKT-82/full-profile.json \
	SMOKE_TEST_TIMEOUT=40m \
	./scripts/smoke.sh

## ---- dev conveniences ----
# One-time local credential bootstrap: no secret ships in this repo with a
# working default (TKT-83, extended to the three signing keys by ai-review S5).
# The logic moved to a script when it grew Ed25519 pairs — see
# scripts/env-bootstrap.sh for what is generated and why each value gets its own
# draw.
env-bootstrap:
	@./scripts/env-bootstrap.sh

# The direct-ports overlay is a DEV convenience (ai-review S11): compose.yaml
# publishes only the gateway, and local work wants psql, Grafana and the staff
# surfaces on loopback. A deployment starting from compose.yaml gets neither.
up: env-bootstrap
	docker compose -f compose.yaml -f compose.direct-ports.yaml up -d --build --wait

down:
	docker compose -f compose.yaml -f compose.direct-ports.yaml down -v

clean: down
	rm -rf bin web/scanner/dist web/storefront/dist web/backoffice/dist
