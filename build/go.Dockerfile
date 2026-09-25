# Shared builder for all Go components (five services + gateway).
# Build arg PKG selects the binary, e.g.:
#   docker build -f build/go.Dockerfile --build-arg PKG=ticketing/services/catalog/cmd/catalog .
FROM golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS build
WORKDIR /src
COPY go.work go.work.sum* ./
COPY shared/ shared/
COPY services/ services/
COPY gateway/ gateway/
COPY smoke/ smoke/
ARG PKG
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/app "$PKG"

# Distroless: no shell, no curl — the container healthcheck exec's the
# binary's own `healthcheck` subcommand instead.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
