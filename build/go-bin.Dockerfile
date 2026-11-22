# Smoke-only packaging image: wraps a host-built static binary (see
# `make build-gate-linux`) instead of compiling in-Docker. The hermetic
# compile path stays in build/go.Dockerfile (used by `make up` and the
# hermetic-smoke workflow). Selected via compose.smoke.yaml.
#   docker build -f build/go-bin.Dockerfile --build-arg BIN=catalog .
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG BIN
COPY bin/gate/${BIN} /app
USER nonroot
ENTRYPOINT ["/app"]
