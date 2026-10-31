# git2-runner: local development. Run inside the devcontainer, which
# provides Go, psql, the env from ../.env, a throwaway PostgreSQL for tests,
# a Redis and a Docker daemon (see ../CLAUDE.md).
#
# Target names match git2 and git2-codeplane. Every recipe is a single
# command with no shell syntax, so it also runs on a Windows host.

GO     ?= go
GOTOOL := $(GO) tool -modfile=tools.mod

.PHONY: run run-runner tests tests-docker build lint proto proto-check

## run: run ci-svc (internal port :9090, runner port :9091). Ctrl-C stops it.
run:
	$(GO) run ./cmd/ci-svc

## run-runner: run a registered runner (GIT2_RUNNER_CONFIG, see docs/api/runners.md)
run-runner:
	$(GO) run ./cmd/git2-runner run

## tests: every test; the Postgres ones run only when TEST_POSTGRES_DSN is set
tests:
	$(GO) test -count=1 ./...

## tests-docker: also the tests that start containers (needs a Docker daemon)
tests-docker:
	$(GO) test -count=1 -tags integration ./...

## build: compile ci-svc and git2-runner into bin/
build:
	$(GO) build -o bin/ ./cmd/ci-svc ./cmd/git2-runner

## lint: golangci-lint (Go), buf lint and buf format (proto)
lint:
	$(GOTOOL) golangci-lint run ./...
	$(GOTOOL) buf lint
	$(GOTOOL) buf format -d --exit-code

## proto: lint the proto files and regenerate gen/go
proto:
	$(GOTOOL) buf lint
	$(GOTOOL) buf generate

## proto-check: fail if gen/go is out of date with proto/ (compares with the git index)
proto-check:
	$(GOTOOL) buf generate
	git diff --exit-code -- gen/
