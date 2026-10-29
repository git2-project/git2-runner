# git2-runner — notes for Claude

The CI lane of [git2](https://github.com/git2-project/git2): it turns a push into a pipeline and runs each job in its own container on a build host. Owner: Osman (Pipelines). Backup reviewer: Ceyda.

For architecture work, read `docs/design/plan.md` (the plan), `docs/decisions/` (what is decided and why) and `docs/design/assumptions.md` (what we assume about the other repos). The project-wide documents (charter, project plan, kickoff deck, team map) are in the git2 repo under `docs/project_docs/`. They disagree in places; `docs/design/plan.md` §2 says which version we follow.

## Hard rules

- **PRs only.** Never commit to `main`. Work on a branch (`feat/…`, `fix/…`, `docs/…`, `chore/…`). Don't push, open PRs or issues, or merge unless asked. Every PR gets one human review.
- **No migrations at runtime, and their source of truth is git2** (ADR 0009).
  - The schema lives in the git2 repo: `migrations/NNN_ci_<what>.sql`, first line a `-- ` comment, idempotent statements, no Down part. Our schema is `ci_svc`; today's file is `003_ci_pipelines.sql`.
  - This repo keeps a byte-identical copy in `migrations/` at the repo root. Only tests and the devcontainer's test database read it. Change both files together, in a git2 PR and a PR here.
  - ci-svc never changes the schema; it checks its tables at startup. Never edit an applied migration or change the shared DB by hand.
- **A doc with every change.** If anyone outside this repo can notice a change (proto, event, config, error, limit, behaviour), the same PR updates `docs/api/`, adds a version note and adds a line to `docs/version_notes/overview.md`. The rules are in `docs/CLAUDE.md`.
- **Never edit other repositories** (`git2`, `git2-codeplane`, `git2-deploy`). When something there must change, write it down for its owner in `docs/design/assumptions.md` or as the text of an issue. The one exception: our own migration file (and `contracts/ci/`) in git2, on a branch, when Osman asks.
- **Contracts.** `app/proto/ci/v1/` is the source of truth for our gRPC API. A contract change needs approval from every affected owner.
- **Build hosts are untrusted.** The runner runs other people's code. It never gets the database DSN, the Redis address, the secrets key or bucket credentials, only what one job needs while that job runs.
- **Restore promise.** Durable state lives only in Postgres (`ci_svc`) or the instance bucket. Never on local disk, and never only in Redis, which is not backed up. Any process, Redis included, can be killed, and the instance can be restored from last night's snapshot onto a new server. Code must reach a correct state again from any of these.
- **Secrets** are never logged, never returned by an API once set, and never committed. `.env` is gitignored; `env.example` is the template.

## Stack

Two programs, decided in `docs/decisions/` (0001–0009): **ci-svc** (control plane: gRPC API, pipelines, queue, secrets) and **the runner** (`git2-runner`, runs jobs on build hosts and dials out to ci-svc).

- Go 1.26. **The Go module root is `app/`** (`github.com/git2-project/git2-runner/app`). Run `go` commands from there.
- gRPC with protobuf between services; buf for lint and codegen. REST lives only at git2's gateway.
- PostgreSQL through pgx/v5, on the team's shared Supabase dev DB. Postgres is the source of truth and the job queue (`FOR UPDATE SKIP LOCKED`).
- The Redis shared by all git2 services, through go-redis/v9. ci-svc uses it only for wake-ups, live log fan-out and rate limits. There is no message broker.
- The bucket through `gocloud.dev/blob` (S3-compatible, GCS, Azure, local disk) for logs, artifacts and caches: planned for W6, not in the code yet.
- The Docker Engine API (`github.com/moby/moby/client`) for job containers, never the `docker` CLI (ADR 0008).
- `log/slog` JSON logs.
- Dev tools (buf, protoc plugins, golangci-lint, grpcurl) are pinned in `app/tools.mod` at codeplane's versions and run with `go tool -modfile=tools.mod <tool>` (needs Go 1.26.7+; the devcontainer has 1.26.10).

## Layout

```
.devcontainer/          Go 1.26.10, psql, redis-cli, Docker CLI + a test PostgreSQL, a Redis and a Docker-in-Docker daemon
app/                    Go module root, all code
  cmd/ci-svc            main: config, startup checks, both gRPC servers, reaper, graceful shutdown
  cmd/git2-runner       main: register, run, version
  proto/ci/v1/          the gRPC contract (source of truth); gen/go/ holds the generated code (make proto)
  internal/ciyaml       .git2-ci.yml parser, validation and JSON schema (shared by both programs)
  internal/cisvc/...    ci-svc only: config, db, testdb, pipeline, runners, secrets, secretbox,
                        notify, ratelimit, outbox, source, server, apierr, page, token
  internal/runner       the agent: loop, heartbeats, log shipping, secret masking
  internal/runner/docker  the Docker executor
migrations/             copy of our git2 migration, for tests (ADR 0009)
docs/api/               reference of our API for the other lanes (canonical)
docs/version_notes/     one note per change other lanes must know about; overview.md is the index
docs/design/            plan, assumptions, proposals
docs/decisions/         ADRs
env.example             template for .env
```

A request flows `server → pipeline | runners | secrets → (db, outbox, notify)`. Domain packages know nothing about gRPC and return `apierr.Error` for anything a client may see; `server` knows no business rules. **The runner must never import `internal/cisvc/...`**: check with `go list -deps ./cmd/git2-runner | grep -E 'pgx|go-redis|cisvc'`, which must print nothing.

## Development

Open the repo in the devcontainer (VS Code → *Reopen in Container*). It runs:
- a throwaway PostgreSQL (`postgres:5432`), which loads `migrations/` on its first start; `TEST_POSTGRES_DSN` points at it;
- its own Redis (`REDIS_ADDR=redis:6379`), standing in for the instance's shared one;
- a Docker-in-Docker daemon (`DOCKER_HOST=tcp://docker:2375`) for job containers, never the host's Docker. It is a different machine from the dev container, so never bind-mount local paths into job containers: use volumes and the API.

```bash
cp env.example .env    # POSTGRES_DSN, and GIT2_CI_SECRETS_KEY from `openssl rand -base64 32`
cd app
make tests             # every test; Postgres tests run because TEST_POSTGRES_DSN is set
make tests-docker      # plus the tests that start containers
make lint              # golangci-lint, buf lint, buf format
```

Until the shared DB has the `ci_svc` schema (the git2 migration must be merged and applied first), point `POSTGRES_DSN` at the test database: `postgres://git2:git2@postgres:5432/git2_test?sslmode=disable`.

| Task | Command (in `app/`) |
|---|---|
| Run ci-svc | `make run` (internal `:9090`, runner `:9091`) |
| Register a runner | `go run ./cmd/git2-runner register -url http://localhost:9091 -token <g2rt_…>` |
| Run the runner | `make run-runner` |
| All tests / with Docker | `make tests` / `make tests-docker` |
| One test | `go test ./internal/ciyaml -run TestParseInvalid` |
| Build | `make build` (output in `bin/`) |
| Lint | `make lint` |
| Regenerate proto code | `make proto` (after every `.proto` change; commit `gen/` with it) |

Call the running service with grpcurl (needs `GIT2_CISVC_DEV=true`, which turns on reflection):

```sh
go tool -modfile=tools.mod grpcurl -plaintext localhost:9090 list
go tool -modfile=tools.mod grpcurl -plaintext -d '{"description":"dev"}' localhost:9090 ci.v1.RunnerAdminService/CreateRegistrationToken
```

To run a pipeline without codeplane, set `GIT2_CISVC_DEV_CONFIG_DIR` and put a file at `<dir>/<repository_id>/.git2-ci.yml`, then call `TriggerPipeline` with that repository ID. Every variable is described in `docs/api/config.md`.

## Conventions

These match git2-codeplane, so all git2 services look alike.

- **gRPC API** (codeplane ADR 0008):
  - Every RPC has its own `XRequest`/`XResponse`.
  - IDs are UUID strings, and a malformed one is `INVALID_ARGUMENT`.
  - List RPCs paginate per AIP-158: `page_size` 0 means 50, values above 100 are capped at 100, and `page_token` is opaque.
  - Every service, RPC, message and field has a comment (buf lint `COMMENTS`).
- **Errors.**
  - Wrap with context: `fmt.Errorf("lease job: %w", err)`. Domain errors are sentinels, checked with `errors.Is`.
  - Clients see only standard codes plus a generic `INTERNAL`. They never see paths, SQL, secrets or raw Docker errors; log those instead.
  - A database or bucket fault is `UNAVAILABLE` or `INTERNAL`, never `UNAUTHENTICATED`, because a runner that gets `UNAUTHENTICATED` throws its token away.
- **SQL.**
  - Schema-qualify every table (`ci_svc.jobs`) and never rely on `search_path`.
  - A state change and its outbox row go in one transaction.
  - The Supabase transaction pooler (port 6543) keeps no session state: no `LISTEN/NOTIFY`, no session advisory locks, no prepared statements.
- **Redis** is shared by every git2 service and is not backed up.
  - Every key and channel we use starts with `ci:`. Signals carry no data: whoever wakes up reads Postgres.
  - It carries signals only. Write to Postgres first, then publish.
  - Correctness never depends on it. When Redis is down or a message is lost, ci-svc falls back to reading Postgres; a Redis fault never makes ci-svc unready.
- **Events** go to `ci_svc.outbox`. The type is the full message name (`ci.v1.PipelineFinished`), the payload is canonical protobuf JSON, and delivery is at least once.
- **Logging.** `log/slog`, JSON to stdout, key/value attrs.
- **Style.** gofmt and goimports, with local imports last. Every package and exported identifier has a doc comment.
- **Tests.**
  - Every PR ships with tests. Use table tests for pure logic; miniredis for Redis.
  - Tests that need Docker carry `//go:build integration` (`make tests-docker`).
  - Postgres tests live in `*_integration_test.go`, skip unless `TEST_POSTGRES_DSN` is set, get their pool from `testdb.Open`, run once per pgx mode (`testdb.Modes`), and delete their own rows.
  - Tests that lease jobs give every job and runner a tag unique to the test, so parallel tests never take each other's jobs.
- **SQL in pooler mode.** Queries must also work in pgx's exec mode: send JSON as text with a cast (`$1::jsonb`), arrays with a cast (`$1::text[]`, `$1::uuid[]` from `[]string`), intervals as `make_interval(secs => $1::float8)`.
- **Commits.** Conventional commits (`feat(runner): …`, `fix(ci-svc): …`, `docs: …`), small and focused.
- **Decisions.** A decision the docs don't cover gets a short ADR in `docs/decisions/NNNN-title.md`.

## Docs lookup

Context7 is configured in `.mcp.json`. Check library APIs (grpc-go, pgx, the Docker client, gocloud.dev/blob) there before relying on memory.
