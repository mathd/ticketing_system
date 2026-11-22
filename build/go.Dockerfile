# Shared builder for all Go components (five services + gateway).
# Build arg PKG selects the binary, e.g.:
#   docker build -f build/go.Dockerfile --build-arg PKG=ticketing/services/catalog/cmd/catalog .
FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
