# CI lane plan (draft v0)

Status: **Q1–Q5 decided on 2026-10-26, details in ADRs 0006–0009 on 2026-10-29** ([§8](#8-decisions)) · Owner: Osman · updated 2026-10-29

> The contract described in §3.2 now exists. Where this plan and [docs/api/](../api/README.md) or the `.proto` files disagree, those win.

How the CI lane gets from an empty repo to the M3 "green pipeline" demo (11 Dec) and through the M4 restore demo (18 Dec). Agreed decisions are recorded as ADRs in [`docs/decisions/`](../decisions/). What we assume about the other repos is in [assumptions.md](assumptions.md).

## 1. What we owe

From the charter and the project plan. All of WBS-10 is on the critical path.

| WBS | What | Weeks | Done when |
|---|---|---|---|
| 1.1 | Contracts v0 | W1 | Our proto merged and referenced from git2's `contracts/` |
| 10.1 | `.git2-ci.yml` v0 spec | W1 (26 Oct–1 Nov) | Stages, jobs, images, scripts, artifacts and caches documented and validated by a JSON schema |
| 10.2 | Runner v0 | W2–W3 | A runner registers, pulls a job, runs it in its own container and reports the exit status |
| 10.3 | ci-svc: pipelines and job queue | W4–W5 | A push creates a pipeline; jobs run in stage order on runners |
| 10.4 | Live logs and artifacts | W6–W7 | Logs stream while a job runs; artifacts are stored in the bucket and can be downloaded |
| 10.5 | Pipeline views (Bilge builds them) | W6–W7 | Built by Dash from our docs |
| 10.6 | CI caching (optional) | W8 | Only if we are on plan at the W7 review |

Risks we own: **R-03** (durable data kept only in Redis), **R-05** (an untrusted job compromises the server), and our part of **R-09** (a restore misses a secret).

Targets that touch us: a queued job starts on an idle runner in **< 5 s**; a log line reaches the browser **< 2 s** after the job prints it; jobs run unprivileged with CPU and memory limits, on a VM separate from git2; services restart cleanly; events are delivered at least once.

## 2. Where the project documents disagree

The documents agree on the big picture and disagree on details that matter to this lane. The team map is the newest; the charter and the project plan are what the course grades.

- **Who plans pipelines and queues jobs.** The kickoff deck, the charter, the project plan and git2's README all put `ci-svc` in the git2 repo, owned by Osman. The team map has no ci-svc at all: codeplane hands CI jobs straight to the runner, and Bilge and Beste are the people committing to git2.
  → **Decided: ci-svc lives in this repo** (ADR 0001). It keeps its name, so every document still reads right except for which repo it sits in.
- **Redis.** The kickoff says "no Redis, Postgres is enough". The charter and the plan use Redis for the job queue and for live logs (pub/sub). git2 core uses Redis for sessions; codeplane uses none.
  → **Decided: one Redis, shared by all services** (ADR 0003). In this lane, Postgres stays the source of truth for jobs, leases and logs. Redis only wakes waiting runners, carries live log lines between ci-svc replicas and holds rate limits (§3.6). Nothing durable is ever only in Redis, which keeps R-03 closed.
- **Dev database and migrations.** The team map: one shared Supabase dev DB, migrations only in git2, applied by a GitHub Action after merge. git2 core: `POSTGRES_DSN` and a psql-based `make migrate`. codeplane first had its own Postgres, `GIT2_DATABASE_URL` and `migrate` command; since its ADRs 0006 and 0010 (26 Oct) it matches git2.
  → We use the shared Supabase DB through `POSTGRES_DSN`, and our migrations go to git2 by PR.
- **Where contracts live.** The plan says "merged in `contracts/`" of git2. codeplane's ADR 0004 keeps the proto next to its code, and git2's `contracts/` holds a pointer to it.
  → We follow codeplane's ADR 0004 and ADR 0008, so all gRPC APIs in git2 look alike.
- **How live logs travel.** The charter says Redis pub/sub.
  → Partly. Inside ci-svc, Redis pub/sub carries "the log grew" signals between replicas; the lines themselves are read from Postgres. Toward Dash it is a gRPC stream, which dash-api turns into SSE or WebSocket for the browser (A-07).
- **Go version.** git2 uses 1.26; codeplane moved from 1.27 to 1.26 (its ADR 0010).
  → 1.26 everywhere, pinned to 1.26.10 in the devcontainers (A-14, confirmed).
- **Who manages runners in the UI.** The charter says admin-api and admin-web. The team map says Beste's console, built from Osman's docs.
  → These describe the same thing. Beste builds it against our `RunnerAdminService`.

Also noticed, not ours to fix: the team map routes the console's "users · settings · runners" to codeplane instead of auth-svc and ci-svc.

## 3. Proposed architecture

Two programs, one repo:

```
        dash-api · admin-api · codeplane (push)
                         │
                         │ gRPC, internal port
                         ▼
                 ┌───────────────┐ ──► PostgreSQL, schema ci_svc   source of truth
                 │    ci-svc     │ ──► Redis, shared, keys ci:*    wake-ups, live log fan-out
                 │ N replicas    │ ──► bucket, prefix ci/          logs, artifacts, caches
                 └───────────────┘
                         ▲
                         │ gRPC + TLS, runner port (runners dial out)
        ┌────────────────┼─────────────────────────┐
        │                │                         │
   git2-runner      git2-runner                git2-runner
   on a VM          on a dedicated server      in Kubernetes
   └ Docker: a      (systemd)                  └ a container per job
     container      └ Docker: a container        (later: a pod per job)
     per job          per job
```

**ci-svc**, the control plane (what the plan calls ci-svc, plus the server side of the runner protocol):

- reads `.git2-ci.yml`, turns a trigger into a pipeline of jobs and queues them in Postgres;
- hands jobs to runners, takes their logs and results, and keeps pipeline state;
- stores CI secrets encrypted, and writes logs, artifacts and caches to the bucket;
- writes `PipelineFinished` and the other events to its outbox;
- uses the shared Redis only for signals that make it faster, never for state (§3.6);
- is the only part of the lane with the database DSN, the Redis address and the secrets key;
- is stateless, so it runs as one or many replicas next to the other git2 services.

**The runner** (`git2-runner` binary), the data plane, one per build host:

- dials out to ci-svc, so it needs nothing but network access to the instance, as the README promises;
- asks for a job, runs each step in a container, streams the log, uploads artifacts through ci-svc and reports the result;
- holds only its own runner token. It has no DSN, no secrets key and no bucket credentials (R-05);
- runs jobs through an executor interface: `docker` first (VM, dedicated server, any host with a Docker daemon), `kubernetes` (a pod per job) later.

Why two programs instead of one runner that talks to the database:

1. **Security.** A build host runs other people's code. If it held the DSN or the secrets key, one malicious `.git2-ci.yml` could read every secret of every repository. With the split, a compromised host leaks at most the secrets of the jobs it ran.
2. **Placement.** Runners go wherever the build capacity is (another VM, a Kubernetes cluster, a laptop) and need only outbound access. The database never has to be reachable from a build host.
3. **Restore.** All state sits behind ci-svc, in Postgres and the bucket, so the backup covers it. Runners can be thrown away and recreated.

GitLab (Rails plus gitlab-runner) and GitHub Actions (the service plus self-hosted runners) are built the same way, and it matches the flow in this repo's README: register, pick up jobs, run, stream logs, report.

### 3.1 Two ports

- **Internal port** (for example `:9090`): `PipelineService`, `RunnerAdminService`, `SecretService`. Only reachable inside the instance network. Like git-svc, ci-svc trusts its callers here: dash-api and admin-api check the user's permissions before they call us (A-10).
- **Runner port** (for example `:9091`): `RunnerService` only. TLS, and every call is authenticated with the runner token. This is the only port build hosts can reach (A-09).

### 3.2 gRPC API v0 (package `ci.v1`)

Built on 2026-10-29: [`app/proto/ci/v1/`](../../app/proto/ci/v1/), documented in [docs/api/](../api/README.md). Changes from the first sketch: `AppendLog` is unary and idempotent by offset (not a client stream); `ValidateConfig` was added; `RetryJob` (W4–W5) and the artifact RPCs (W6–W7) come later.

| Service | RPC | Called by | What it does |
|---|---|---|---|
| PipelineService | `TriggerPipeline` | whoever delivers codeplane's push event; dash-api ("run pipeline") | Creates a pipeline for (repository_id, ref, commit_sha). Idempotent on `idempotency_key` (the push event's ID), so at-least-once delivery never runs a pipeline twice |
| | `GetPipeline`, `ListPipelines` | dash-api | A repository's or ref's pipelines, with their jobs and statuses |
| | `CancelPipeline`, `RetryJob` | dash-api | |
| | `GetJob`, `StreamJobLog` | dash-api | The log from a byte offset. With `follow`, the stream stays open until the job ends |
| | `ListArtifacts`, `DownloadArtifact` | dash-api | `DownloadArtifact` streams the bytes |
| RunnerAdminService | `CreateRegistrationToken` | admin-api (console) | A one-time token, shown once, to register a runner |
| | `ListRunners`, `GetRunner`, `UpdateRunner`, `DeleteRunner` | admin-api | Status (online, offline, paused), labels, last seen, version |
| SecretService | `SetSecret`, `ListSecrets`, `DeleteSecret` | admin-api, dash-api (repository settings) | Values are write-only: `ListSecrets` returns names and dates, never values |
| RunnerService | `RegisterRunner` | runner | Trades a registration token for a runner ID and a runner token (returned once) |
| | `RequestJob` | runner | Long poll, up to about 30 s. Returns a job or nothing |
| | `UpdateJob` | runner | State changes, and the heartbeat. The reply says whether the job was cancelled |
| | `AppendLog` | runner | One chunk of log at a byte offset; idempotent, so retries are safe |
| | `UploadArtifact` | runner | A client stream; ci-svc writes it to the bucket |

Events in `ci_svc.outbox`: `ci.v1.PipelineCreated`, `ci.v1.PipelineFinished` (the kickoff's `pipeline.finished`, which drives the ✓ on merge requests) and `ci.v1.JobFinished`.

`RequestJob` is a long poll rather than a bidirectional stream: it is simpler, survives proxies and load balancers, and lets any ci-svc replica answer.

### 3.3 `.git2-ci.yml` v0 (W1)

Small on purpose, and a subset of GitLab CI so people recognise it:

```yaml
stages: [build, test]

variables:
  CGO_ENABLED: "0"

build:
  stage: build
  image: golang:1.26
  script:
    - go build -o bin/ ./...
  artifacts:
    paths: [bin/]
    expire_in: 7d

test:
  stage: test
  image: golang:1.26
  tags: [linux]
  timeout: 10m
  script:
    - go test ./...
```

v0 keys: `stages`, `variables`, and for each job `stage`, `image`, `script`, `variables`, `tags` (which runners may take it), `timeout`, `artifacts.paths`, `artifacts.expire_in` and `cache.paths` (WBS-10.6). Left out of v0: `services`, `rules`, `include`, `needs`, matrices. The JSON schema is published in `docs/api/ci-yaml.md` so editors can validate the file.

ci-svc reads the file from the repository at the pushed commit through codeplane (A-03).

### 3.4 Data: schema `ci_svc`, migrations in git2

The SQL goes to a git2 PR in W1.

| Table | Holds |
|---|---|
| `runners` | id, name, labels, executor, version, token hash (SHA-256), paused, last_seen_at |
| `registration_tokens` | token hash, expires_at, used_at |
| `pipelines` | id, repository_id, ref, commit_sha, trigger, idempotency_key (unique), status, timestamps |
| `jobs` | id, pipeline_id, stage, name, spec (JSONB), status, runner_id, lease_expires_at, exit_code, failure_reason, timestamps |
| `job_log_chunks` | job_id, byte offset, data: the live part of a running job's log |
| `artifacts` | job_id, name, object key, size, sha256, expires_at |
| `secrets` | scope (instance, owner, repository), scope_id, name, ciphertext, nonce, key_id |
| `outbox` | the team's outbox format (A-13) |

**Queue.** The queue is the `jobs` table. A runner leases a job with one statement: `UPDATE … WHERE id = (SELECT id FROM ci_svc.jobs WHERE status = 'pending' AND <tags match> ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)`. Every `UpdateJob` renews the lease, and a reaper fails jobs whose lease ran out (`runner_lost`).

How a waiting runner hears about a job:

1. After the transaction that makes a job pending commits, ci-svc publishes on `ci:jobs:wake`.
2. Every waiting `RequestJob`, on any replica, tries the lease statement at once. `SKIP LOCKED` lets exactly one of them win, so the job starts in well under a second.
3. Pub/sub can drop a message, so a waiting long poll also tries the lease every 3 s. Even with Redis down, jobs start within the 5 s target.

The fallback is a timer and not `LISTEN/NOTIFY`, because the Supabase transaction pooler does not support `LISTEN`.

**Live logs.** How a log line gets from the runner to Dash:

1. `AppendLog` writes each chunk to `job_log_chunks` first, then publishes a signal (no data) on `ci:log:<job_id>`.
2. `StreamJobLog` sends what Postgres holds from the caller's byte offset on, then waits for the signal and reads Postgres again. A line reaches a follower on any replica within milliseconds.
3. Because followers always read Postgres, a lost signal only delays them: they also read every second, inside the 2 s target with Redis down.

When the job ends, the chunks are joined into one object, `ci/logs/<job_id>.log`, and the rows are deleted. In v0 a job log is capped (for example at 4 MiB) and then truncated with a notice.

### 3.5 Secrets

- **At rest.** Each value is encrypted with AES-256-GCM and a random nonce. The additional data is scope, scope_id and name, so a ciphertext copied into another row does not decrypt. The key (`GIT2_CI_SECRETS_KEY`, 32 bytes) comes from config, never from the database. A `key_id` column leaves room for key rotation.
- **Through the API.** Write-only. No RPC returns a value.
- **To a job.** ci-svc decrypts the secrets the job's repository may use and puts them in the job it hands to the runner that leased it, over TLS. The runner passes them to the container as environment variables and masks them in the log before sending it. They are never written to the build host's disk.
- **In the backup.** The database dump holds only ciphertext, so the key must travel in the snapshot's "secrets" part. Without it, a restore brings back secrets nobody can read (R-09, A-05).
- Runner tokens and registration tokens are stored only as SHA-256 hashes.

### 3.6 What goes in Redis

The instance has one Redis, shared by every git2 service (auth-svc keeps its sessions there). It is not part of the backup. Our rules:

- Every key and channel we use starts with `ci:`, so we never collide with another service.
- Only ci-svc connects to it. The runner never gets its address.
- It carries signals, never state. ci-svc writes to Postgres first and publishes afterwards.
- ci-svc works correctly without it, only slower. A Redis fault does not make ci-svc unready.

| Key or channel | Holds | If it is lost |
|---|---|---|
| `ci:jobs:wake` (channel) | "A job is waiting" for long-polling runners | Runners find the job on their next 3 s check |
| `ci:log:<job_id>` (channel) | "The log grew or the job ended", for followers on any replica | Followers read Postgres every second |
| `ci:rl:<action>:<key>` | Rate-limit counters, for example registration attempts per IP address | Limits start again from zero |

Never in Redis: job or pipeline state, queue order, leases, logs, secrets, tokens.

## 4. Backup and restore

The promise: one bucket holds the database, the repositories, `git2.yaml` and the secrets, and a new server restores from that bucket alone. For this lane that means:

1. **Nothing durable outside Postgres and the bucket.** ci-svc keeps no local state, and Redis holds only signals (§3.6), so a restored instance starts with an empty Redis and loses nothing. A runner's only state is its token, in its config file on the build host. Job workspaces are thrown away.
2. **Object keys in the database, not URLs.** Rows store `ci/artifacts/<job_id>/<name>`, never the bucket's name or URL, so moving to another bucket (Ceyda's bucket migration) is a copy plus a config change.
3. **A snapshot taken mid-run recovers.** After a restore, jobs that were `running` have no runner anymore. Their lease expires, they fail as `runner_lost`, and their pipelines finish. Pending jobs simply run. Nothing stays "running" forever.
4. **Missing objects are normal.** A row may point to an artifact or log that expired, or that was written after the snapshot. The API answers `NOT_FOUND` and nothing crashes.
5. **Runners reconnect.** Token hashes are in the database, so runners keep working after a restore. A runner registered after the last snapshot must register again, and the console should say so.
6. **Caches are not precious.** `ci/cache/` can be left out of backups and bucket migrations; jobs rebuild it.
7. **The secrets key is in the snapshot** (§3.5).
8. **Restore test.** From W5, the weekly restore test (R-09) covers this lane: run a pipeline, back up, restore on a fresh VM, then check pipeline history, logs, artifacts and secrets, and that a runner reconnects and runs a new job.

## 5. Where it runs

| Setup | ci-svc | Runner |
|---|---|---|
| One machine (Docker Compose) | A container next to git2 (A-08) | A container on a **second** VM, using that VM's Docker (R-05). On the same machine only for demos |
| Dedicated build server | — | A static binary with a systemd unit, Docker Engine on the host |
| Kubernetes (Helm, Ceyda) | A Deployment, 2 or more replicas | A Deployment. v0 uses a Docker-in-Docker sidecar; later a `kubernetes` executor starts a pod per job, with no privileged containers |

Both programs take their config from environment variables (later also from `git2.yaml`), serve `/healthz`, `/readyz` and Prometheus `/metrics`, log JSON, and shut down gracefully: on SIGTERM the runner finishes or hands back its current job. Images are built for amd64 and arm64.

Job isolation with the `docker` executor (R-05): no `--privileged`, no Docker socket inside the job container, CPU, memory and process limits, a non-root user where the image allows it, a fresh network and volume per job, and everything removed afterwards.

## 6. Layout

As built on 2026-10-29:

```
app/                       Go module root: github.com/git2-project/git2-runner/app
  cmd/ci-svc/              control plane
  cmd/git2-runner/         the runner: register, run
  proto/ci/v1/             gRPC contract (source of truth)
  gen/go/ci/v1/            generated Go code, committed (as in codeplane)
  internal/
    ciyaml/                .git2-ci.yml parsing, validation, JSON schema
    cisvc/                 ci-svc only; the runner binary imports none of it
      config/              env parsing
      db/, testdb/         Postgres pool (pooler-aware), schema check, test helper
      pipeline/            trigger, stages, queue and leases, reaper, logs
      runners/             registration, tokens, admin
      secrets/, secretbox/ CI secrets and their encryption
      notify/, ratelimit/  Redis signals with polling fallback; rate limits
      outbox/, source/     events; reading .git2-ci.yml (dev dir until A-03)
      server/              gRPC servers, interceptors, runner auth, error mapping
      apierr/, page/, token/
    runner/                agent loop, heartbeats, log shipping, masking
    runner/docker/         Docker Engine API executor
migrations/                copy of our git2 migration, for tests (ADR 0009)
```

Still to come: `internal/cisvc/blob/` (bucket, W6) and a codeplane client in `source/` (A-03).

A Go module in a subdirectory is tagged with the directory as a prefix (`app/v0.1.0`), and then `go get github.com/git2-project/git2-runner/app@v0.1.0` works. Two binaries keep database code out of the runner; `go list -deps ./cmd/git2-runner` shows no pgx and no Redis client.

## 7. Roadmap

| Week | Dates | Goal | Ships |
|---|---|---|---|
| W1 | 26 Oct–1 Nov | Contract and spec | `.git2-ci.yml` v0 with JSON schema and parser; `ci.v1` proto v0 and its `docs/api/` pages; `ci_svc` migration PR to git2; pointer in git2's `contracts/ci/`; assumptions sent to their owners |
| W2–W3 | 2–15 Nov | Runner v0 | ci-svc: register, request job (Postgres lease, Redis wake-up), update, log chunks. Runner: `docker` executor. Demo: a job inserted by hand runs in a container and its log can be read |
| W4–W5 | 16–29 Nov | Pipelines | `TriggerPipeline`, stage order, cancel and retry, secrets, `PipelineFinished`. Wired to codeplane's push once A-02 lands. Restore tests start in W5 |
| W6–W7 | 30 Nov–13 Dec | Logs and artifacts | `StreamJobLog` with follow, artifacts in the bucket. **M3 green pipeline on 11 Dec**, with Bilge's views |
| W8 | 14–20 Dec | Hardening | Fault test (kill Redis, ci-svc and a runner mid-pipeline), restore test (**M4, 18 Dec**), caching if on plan |
| W9 | 21–27 Dec | Bug bash, demo | |

## 8. Decisions

Decided by Osman on 2026-10-26.

| # | Question | Decision | ADR |
|---|---|---|---|
| Q1 | Does ci-svc live in this repo? | Yes, under its existing name | [0001](../decisions/0001-ci-svc-lives-in-git2-runner.md) |
| Q2 | Two programs (ci-svc and the runner) or one? | Two (§3) | [0002](../decisions/0002-control-plane-and-runner.md) |
| Q3 | What holds the queue and the live logs? | Postgres, as the source of truth. The Redis shared by all services carries wake-ups and log fan-out (§3.4, §3.6). This changed from the proposal, which had no Redis | [0003](../decisions/0003-postgres-truth-shared-redis-signals.md) |
| Q4 | How does a job get the source code in v0? | ci-svc fetches the commit as an archive from codeplane and passes it to the runner. Later, the runner clones with a short-lived job token (A-04) | [0004](../decisions/0004-job-source-archive-v0.md) |
| Q5 | Runner protocol: long poll or a bidirectional stream? | Long poll (§3.2) | [0005](../decisions/0005-runner-long-poll.md) |
| Q6 | A message broker or the Postgres table as the queue? Job states, leases, cancel, logs | Postgres table with leases; details fixed | [0006](../decisions/0006-job-queue-and-lifecycle.md) |
| Q7 | Where do CI secrets live and get their API? | In ci-svc (`SecretService`), not in git2 | [0007](../decisions/0007-ci-secrets-live-in-ci-svc.md) |
| Q8 | Docker CLI or Engine API in the runner? | Engine API | [0008](../decisions/0008-runner-uses-the-docker-engine-api.md) |
| Q9 | How do our tests get the schema? | A copy of the git2 migration, as in codeplane | [0009](../decisions/0009-schema-copy-tooling-and-tests.md) |

The questions for the other lanes are in [assumptions.md](assumptions.md).

## 9. First steps (W1)

1. ✅ `.git2-ci.yml` v0: the spec, the JSON schema and the parser, with tests (WBS-10.1, due 1 Nov).
2. ✅ `ci.v1` proto v0 with buf lint, its `docs/api/` pages and a version note (29 Oct).
3. ✅ The `ci_svc` migration, on git2 branch `feat/ci-svc-schema`; the PR is Osman's to open.
4. ⏳ Send the action items of the 29 Oct note to their owners as a git2 issue.

Ahead of plan: ci-svc already implements the runner protocol, the queue, cancel, the reaper, live logs and secrets, and `git2-runner` runs jobs with Docker (W2–W3 work). Next: codeplane's file and archive RPCs (A-03, A-04) to wire real pushes, then retry, artifacts and the bucket.
