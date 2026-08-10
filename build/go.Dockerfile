# Shared builder for all Go components (five services + gateway).
# Build arg PKG selects the binary, e.g.:
#   docker build -f build/go.Dockerfile --build-arg PKG=ticketing/services/catalog/cmd/catalog .
FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
