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

.PHONY: check lint test build smoke lint-go lint-ts test-go test-ts build-go build-ts up down clean

check: deps lint test build smoke

## ---- deps (self-contained gate: clean clone needs nothing pre-installed) ----
deps:
	pnpm install --frozen-lockfile

## ---- lint ----
lint: lint-go lint-ts

$(GOLANGCI):
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

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

## ---- smoke (integration seam; owns the stack lifecycle) ----
smoke:
	./scripts/smoke.sh

## ---- dev conveniences ----
up:
	docker compose up -d --build --wait

down:
	docker compose down -v

clean: down
	rm -rf bin web/scanner/dist
