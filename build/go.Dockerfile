# Shared builder for all Go components (five services + gateway).
# Build arg PKG selects the binary, e.g.:
#   docker build -f build/go.Dockerfile --build-arg PKG=ticketing/services/catalog/cmd/catalog .
FROM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
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
