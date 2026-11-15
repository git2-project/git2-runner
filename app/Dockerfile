# syntax=docker/dockerfile:1
#
# Images for ci-svc and git2-runner (git2-runner #17). Build from app/:
#
#   docker buildx build --target ci-svc      --platform linux/amd64,linux/arm64 -t ci-svc .
#   docker buildx build --target git2-runner --platform linux/amd64,linux/arm64 -t git2-runner .
#
# Both are static Go binaries on distroless, running as the non-root user
# 65532. The Go build runs on the build machine's platform and
# cross-compiles, so arm64 needs no emulation. docs/api/config.md and
# docs/api/runners.md say how to run them.

ARG GO_VERSION=1.26.10

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
# Reported by `git2-runner version` and sent to ci-svc at registration.
ARG VERSION=0.1.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/git2-project/git2-runner/app/internal/runner.Version=${VERSION}" \
      -o /out/bin/ ./cmd/ci-svc ./cmd/git2-runner \
 && mkdir -p /out/etc/git2-runner

# ci-svc: internal port 9090 (instance network only) and runner port 9091.
FROM gcr.io/distroless/static-debian12:nonroot AS ci-svc
COPY --from=build /out/bin/ci-svc /usr/local/bin/ci-svc
EXPOSE 9090 9091
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ci-svc"]

# git2-runner: `register` once, then `run` (the default). The config file,
# which holds the runner token, lives in the /etc/git2-runner volume. The
# runner needs a Docker daemon: DOCKER_HOST, or the host's socket mounted
# with its group added (docs/api/runners.md).
FROM gcr.io/distroless/static-debian12:nonroot AS git2-runner
COPY --from=build /out/bin/git2-runner /usr/local/bin/git2-runner
COPY --from=build --chown=65532:65532 /out/etc/git2-runner /etc/git2-runner
ENV GIT2_RUNNER_CONFIG=/etc/git2-runner/config.json
VOLUME ["/etc/git2-runner"]
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/git2-runner"]
CMD ["run"]
