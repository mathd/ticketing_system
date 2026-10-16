# Smoke-only packaging image: wraps a host-built static binary (see
# `make build-gate-linux`) instead of compiling in-Docker. The hermetic
# compile path stays in build/go.Dockerfile (used by `make up` and the
# hermetic-smoke workflow). Selected via compose.smoke.yaml.
#   docker build -f build/go-bin.Dockerfile --build-arg BIN=catalog .
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
ARG BIN
COPY bin/gate/${BIN} /app
USER nonroot
ENTRYPOINT ["/app"]
