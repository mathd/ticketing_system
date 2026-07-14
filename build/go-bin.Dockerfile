# Smoke-only packaging image: wraps a host-built static binary (see
# `make build-gate-linux`) instead of compiling in-Docker. The hermetic
# compile path stays in build/go.Dockerfile (used by `make up` and the
# hermetic-smoke workflow). Selected via compose.smoke.yaml.
#   docker build -f build/go-bin.Dockerfile --build-arg BIN=catalog .
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
ARG BIN
COPY bin/gate/${BIN} /app
USER nonroot
ENTRYPOINT ["/app"]
