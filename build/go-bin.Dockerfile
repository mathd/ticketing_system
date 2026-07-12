# Smoke-only packaging image: wraps a host-built static binary (see
# `make build-gate-linux`) instead of compiling in-Docker. The hermetic
# compile path stays in build/go.Dockerfile (used by `make up` and the
# hermetic-smoke workflow). Selected via compose.smoke.yaml.
#   docker build -f build/go-bin.Dockerfile --build-arg BIN=catalog .
FROM gcr.io/distroless/static-debian12:nonroot
ARG BIN
COPY bin/gate/${BIN} /app
USER nonroot
ENTRYPOINT ["/app"]
