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

.PHONY: env-bootstrap check lint test build smoke smoke-hermetic onsale-load-full lint-go lint-ts test-go test-ts build-go build-ts build-gate-linux generate check-generate check-dep-drift up down clean

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
	pnpm --filter storefront generate:api
	pnpm --filter backoffice generate:api

# The gate fails when committed generated code drifts from the spec.
check-generate: generate
	@git diff --exit-code -- services/*/internal/api/openapi_gen.go web/storefront/src/lib/api-types.gen.ts web/backoffice/src/lib/api-types.gen.ts \
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

test-go:
	@for m in $(GO_TEST_MODULES); do \
		echo "go test: $$m"; \
		(cd $$m && go test ./...) || exit 1; \
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

# Full festival-NFR load profile (TKT-82) — on-demand, not part of `make check`.
# Runs the whole smoke suite with the on-sale profile switched to full and
# writes the machine-readable evidence to docs/evidence/TKT-82/.
onsale-load-full: build-gate-linux build-ts
	ONSALE_PROFILE=full \
	ONSALE_REPORT=$(CURDIR)/docs/evidence/TKT-82/full-profile.json \
	SMOKE_TEST_TIMEOUT=40m \
	./scripts/smoke.sh

## ---- dev conveniences ----
# One-time local credential bootstrap (TKT-83): no default ships in the repo.
# Preserves unrelated .env entries, replaces only a missing/retired token,
# never prints the value. Compose reads .env natively, so bare
# `docker compose up` keeps working after the first `make up`.
env-bootstrap:
	@set -e; umask 077; \
	token=$$(grep -E '^INTERNAL_SERVICE_TOKEN[[:space:]]*=' .env 2>/dev/null | tail -n1 \
		| sed -e 's/^[^=]*=[[:space:]]*//' | tr -d '\r' \
		| sed -e 's/^"//' -e 's/"$$//' -e "s/^'//" -e "s/'$$//"); \
	if [ -z "$$token" ] || [ "$$token" = "local-service-token" ]; then \
		new=$$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n'); \
		rm -f .env.tmp; \
		{ grep -vE '^INTERNAL_SERVICE_TOKEN[[:space:]]*=' .env 2>/dev/null || true; printf 'INTERNAL_SERVICE_TOKEN=%s\n' "$$new"; } > .env.tmp; \
		mv .env.tmp .env; \
		echo "generated INTERNAL_SERVICE_TOKEN in .env"; \
	fi; \
	[ ! -f .env ] || chmod 600 .env

up: env-bootstrap
	docker compose up -d --build --wait

down:
	docker compose down -v

clean: down
	rm -rf bin web/scanner/dist web/storefront/dist web/backoffice/dist
