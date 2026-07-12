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

.PHONY: check lint test build smoke smoke-hermetic lint-go lint-ts test-go test-ts build-go build-ts build-gate-linux up down clean

check: deps lint test build smoke

## ---- deps (self-contained gate: clean clone needs nothing pre-installed) ----
deps:
	pnpm install --frozen-lockfile

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

## ---- dev conveniences ----
up:
	docker compose up -d --build --wait

down:
	docker compose down -v

clean: down
	rm -rf bin web/scanner/dist
