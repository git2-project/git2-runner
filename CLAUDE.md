# git2-runner — notes for Claude

The CI lane of [git2](https://github.com/git2-project/git2): it turns a push into a pipeline and runs each job in its own container on a build host. Owner: Osman (Pipelines). Backup reviewer: Ceyda.

For architecture work, read `docs/design/plan.md` (the plan), `docs/decisions/` (what is decided and why) and `docs/design/assumptions.md` (what we assume about the other repos). The project-wide documents (charter, project plan, kickoff deck, team map) are in the git2 repo under `docs/project_docs/`. They disagree in places; `docs/design/plan.md` §2 says which version we follow.

## Hard rules

- **PRs only.** Never commit to `main`. Work on a branch (`feat/…`, `fix/…`, `docs/…`, `chore/…`). Don't push, open PRs or issues, or merge unless asked. Every PR gets one human review.
- **No migrations here.** The schema lives only in the git2 repo: `migrations/NNN_ci_<what>.sql`, first line a `-- ` comment, our schema is `ci_svc`. A GitHub Action applies them to the shared dev DB after merge. If you need a table or a column, draft the SQL in `docs/design/` and say that it needs a git2 PR. Never edit an applied migration or change the dev DB by hand.
- **A doc with every change.** If anyone outside this repo can notice a change (proto, event, config, error, limit, behaviour), the same PR updates `docs/api/`, adds a version note and adds a line to `docs/version_notes/overview.md`. The rules are in `docs/CLAUDE.md`.
- **Never edit other repositories** (`git2`, `git2-codeplane`, `git2-deploy`). When something there must change, write it down for its owner in `docs/design/assumptions.md` or as the text of an issue.
- **Contracts.** `app/proto/ci/v1/` is the source of truth for our gRPC API. A contract change needs approval from every affected owner.
- **Build hosts are untrusted.** The runner runs other people's code. It never gets the database DSN, the Redis address, the secrets key or bucket credentials, only what one job needs while that job runs.
- **Restore promise.** Durable state lives only in Postgres (`ci_svc`) or the instance bucket. Never on local disk, and never only in Redis, which is not backed up. Any process, Redis included, can be killed, and the instance can be restored from last night's snapshot onto a new server. Code must reach a correct state again from any of these.
- **Secrets** are never logged, never returned by an API once set, and never committed. `.env` is gitignored; `env.example` is the template.

## Stack

Two programs, decided in `docs/decisions/` 0001–0005: **ci-svc** (control plane: gRPC API, pipelines, queue, secrets) and **the runner** (`git2-runner`, runs jobs on build hosts and dials out to ci-svc).

- Go 1.26. **The Go module root is `app/`** (`github.com/git2-project/git2-runner/app`). Run `go` commands from there.
- gRPC with protobuf between services; buf for lint and codegen. REST lives only at git2's gateway.
- PostgreSQL through pgx/v5, on the team's shared Supabase dev DB. Postgres is the source of truth and the job queue (`FOR UPDATE SKIP LOCKED`).
- The Redis shared by all git2 services, through go-redis/v9. ci-svc uses it only for wake-ups, live log fan-out and rate limits. There is no message broker.
- The bucket through `gocloud.dev/blob` (S3-compatible, GCS, Azure, local disk) for logs, artifacts and caches.
- Docker Engine API for job containers. `log/slog` JSON logs, OpenTelemetry.

## Layout

```
.devcontainer/     Go 1.26, psql, redis-cli, Docker CLI + a Redis and a Docker-in-Docker daemon
app/               Go module root, all code (only go.mod so far)
docs/api/          reference of our API for the other lanes (canonical)
docs/version_notes/  one note per change other lanes must know about; overview.md is the index
docs/design/       plan, assumptions, proposals
docs/decisions/    ADRs
env.example        template for .env
```

The planned layout inside `app/` is in `docs/design/plan.md` §6.

## Development

Open the repo in the devcontainer (VS Code → *Reopen in Container*). Its own Redis (`REDIS_ADDR=redis:6379`) stands in for the instance's shared one. Job containers go to its Docker-in-Docker daemon (`DOCKER_HOST=tcp://docker:2375`), never to the host's Docker. That daemon is a different machine from the dev container, so never bind-mount local paths into job containers: use volumes and copy files through the API.

```bash
cp env.example .env    # POSTGRES_DSN: the same shared Supabase DSN as in the git2 repo
cd app && go test ./...
```

There is no Makefile yet. Commands are added here when the code that needs them lands.

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
  - Every key and channel we use starts with `ci:`.
  - It carries signals only. Write to Postgres first, then publish.
  - Correctness never depends on it. When Redis is down or a message is lost, ci-svc falls back to reading Postgres; a Redis fault never makes ci-svc unready.
- **Events** go to `ci_svc.outbox`. The type is the full message name (`ci.v1.PipelineFinished`), the payload is canonical protobuf JSON, and delivery is at least once.
- **Logging.** `log/slog`, JSON to stdout, key/value attrs.
- **Style.** gofmt and goimports, with local imports last. Every package and exported identifier has a doc comment.
- **Tests.**
  - Every PR ships with tests. Use table tests for pure logic.
  - Tests that need Docker carry `//go:build integration`.
  - Postgres tests skip unless `TEST_POSTGRES_DSN` is set.
- **Commits.** Conventional commits (`feat(runner): …`, `fix(ci-svc): …`, `docs: …`), small and focused.
- **Decisions.** A decision the docs don't cover gets a short ADR in `docs/decisions/NNNN-title.md`.

## Docs lookup

Context7 is configured in `.mcp.json`. Check library APIs (grpc-go, pgx, the Docker client, gocloud.dev/blob) there before relying on memory.
