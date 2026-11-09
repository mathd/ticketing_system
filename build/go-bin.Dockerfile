# Smoke-only packaging image: wraps a host-built static binary (see
# `make build-gate-linux`) instead of compiling in-Docker. The hermetic
# compile path stays in build/go.Dockerfile (used by `make up` and the
# hermetic-smoke workflow). Selected via compose.smoke.yaml.
#   docker build -f build/go-bin.Dockerfile --build-arg BIN=catalog .
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
ARG BIN
COPY bin/gate/${BIN} /app
USER nonroot
ENTRYPOINT ["/app"]
