# Local gate (US-001) — `make check` mirrors CI exactly.
# Stages: lint -> test -> build -> smoke (smoke owns its compose lifecycle).

GO_MODULES := shared/go services/catalog services/inventory services/commerce services/payments services/access gateway smoke
# smoke's tests are build-tagged and run in the smoke stage, not test-go
GO_TEST_MODULES := $(filter-out smoke,$(GO_MODULES))
BIN := $(CURDIR)/bin
# pinned so local and CI runs use the same linter (latest at time of pinning)
GOLANGCI_VERSION := v2.12.2
GOLANGCI := $(BIN)/golangci-lint

# The smoke stack runs isolated (own compose project + shifted ports);
# lifecycle and env live in scripts/smoke.sh.

.PHONY: env-bootstrap check lint test build smoke smoke-hermetic browser onsale-load-full lint-go lint-ts test-go test-ts build-go build-ts build-gate-linux generate check-generate check-dep-drift up down clean

check: deps check-generate check-dep-drift lint test build smoke

## ---- deps (self-contained gate: clean clone needs nothing pre-installed) ----
deps:
	pnpm install --frozen-lockfile

## ---- generate (contract-first, ADR-009: spec is the source of truth) ----
generate:
	cd services/catalog/api && go tool oapi-codegen -config codegen.yaml openapi.yaml
	cd services/catalog && go tool oapi-codegen -package api -generate models -o ../inventory/internal/api/openapi_gen.go ../inventory/api/openapi.yaml
	cd services/catalog && go tool oapi-codegen -package api -generate models -o ../commerce/internal/api/openapi_gen.go ../commerce/api/openapi.yaml
	cd services/catalog && go tool oapi-codegen -package api -generate models -o ../payments/internal/api/openapi_gen.go ../payments/api/openapi.yaml
	cd services/catalog && go tool oapi-codegen -package api -generate models -o ../access/internal/api/openapi_gen.go ../access/api/openapi.yaml
	# Runs from the workspace root: openapi-typescript drives the TS compiler API, which
	# TypeScript 7 does not ship, so it keeps its own TS 6 there instead of in the web apps.
	pnpm run generate:api

# The gate fails when committed generated code drifts from the spec.
#
# This list must name EVERY file `generate:api` writes. A generated file added to
# the generator but not to this diff is regenerated and then never compared, so
# the gate goes green over a client that has drifted from its contract — the
# exact failure ADR-009 has this target for. Edit the two together (TKT-220).
check-generate: generate
	@git diff --exit-code -- services/*/internal/api/openapi_gen.go web/storefront/src/lib/api-types.gen.ts web/storefront/src/lib/commerce-api-types.gen.ts web/backoffice/src/lib/api-types.gen.ts web/backoffice/src/lib/inventory-api-types.gen.ts \
		|| { echo "generated code drifted from the OpenAPI spec — commit the output of 'make generate'" >&2; exit 1; }

## ---- dependency declarations (TKT-129, ADR-035: one version per shared dep) ----
# Manifest-only parse, offline, ~0.2s. `go mod tidy` is a no-op on a drifted tree,
# so nothing else in the gate would ever notice.
check-dep-drift:
	@./scripts/check-go-dependency-drift.sh $(GO_MODULES)

## ---- lint ----
lint: lint-go lint-ts

$(GOLANGCI):
	./scripts/install-golangci-lint.sh $(GOLANGCI_VERSION) $(BIN)

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
# Run it for any ticket that adds or changes a storefront/back-office write form —
# the smoke suite only RENDERS those pages, so everything between the browser and
# the handler (checkOrigin, base-path rewrites, redirects, cookie paths, cache and
# referrer headers) is invisible to it. See scripts/browser.sh (TKT-228).
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
