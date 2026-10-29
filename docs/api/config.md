# Configuration, deployment and backup

For the Deploy lane (Ceyda). Two programs come out of this repo:

| Program | Runs where | Needs |
|---|---|---|
| `ci-svc` | next to the other git2 services; stateless, 1 or more replicas | PostgreSQL (schema `ci_svc`), the shared Redis (optional but recommended), the secrets key |
| `git2-runner` | on build hosts, ideally another VM than git2 | Docker Engine, outbound access to ci-svc's runner port. Nothing else. See [runners.md](runners.md#installing-a-runner) |

Both log JSON to stdout and stop gracefully on SIGTERM (ci-svc within 20 s; the runner after its current job).

## ci-svc environment variables

| Variable | Default | Meaning |
|---|---|---|
| `POSTGRES_DSN` | required | PostgreSQL connection string, the same variable as git2's other services. Supabase's transaction pooler (port 6543) works. Secret |
| `POSTGRES_MAX_CONNS` | `10` | Connection pool size, 1 to 1000 |
| `REDIS_ADDR` | empty | The instance's shared Redis, `host:port`. Empty: no Redis, which is fine for **one** replica only (wake-ups then reach only that replica; the others poll every few seconds) |
| `REDIS_PASSWORD` | empty | Secret |
| `GIT2_CI_SECRETS_KEY` | required | 32 random bytes in base64 (`openssl rand -base64 32`). Encrypts every CI secret. **Secret, and must be in every backup** (below) |
| `GIT2_CISVC_GRPC_ADDR` | `:9090` | Internal port: pipelines, runner admin, secrets. Expose only inside the instance network |
| `GIT2_CISVC_RUNNER_ADDR` | `:9091` | Runner port: the only port build hosts reach |
| `GIT2_CISVC_RUNNER_TLS_CERT`, `GIT2_CISVC_RUNNER_TLS_KEY` | empty | PEM files for TLS on the runner port. Set both, or neither and terminate TLS in front of the port (an ingress that passes HTTP/2 through) |
| `GIT2_CISVC_DEV` | `false` | Development only: gRPC reflection, and allows `GIT2_CISVC_DEV_CONFIG_DIR` |
| `GIT2_CISVC_DEV_CONFIG_DIR` | empty | Development only: read `.git2-ci.yml` from `<dir>/<repository_id>/.git2-ci.yml` instead of git-svc |
| `GIT2_CISVC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

ci-svc lists every invalid variable at once and exits with code 1. It never logs the DSN, the Redis password or the key.

## Startup, health and readiness

At startup ci-svc:

1. connects to PostgreSQL and **refuses to start if a `ci_svc` table is missing** (the git2 migration `003_ci_pipelines.sql` has not been applied);
2. **refuses to start if stored CI secrets were encrypted with a different key** than `GIT2_CI_SECRETS_KEY`, naming both key IDs (`k1:…`): you restored a database without its key;
3. connects to Redis if configured. An unreachable Redis is only a warning; ci-svc keeps retrying and works without it;
4. serves both ports.

Health: the standard gRPC health service on both ports (`grpc.health.v1.Health/Check`, service `""`), `SERVING` while ci-svc serves, `NOT_SERVING` during shutdown. Use it for liveness and readiness probes (`grpc_health_probe -addr=:9090`, or Kubernetes' `grpc:` probe). A Redis outage never makes ci-svc unready.

## Database

- Schema `ci_svc`, created by [`migrations/003_ci_pipelines.sql`](https://github.com/git2-project/git2/tree/dev/main/migrations) in the git2 repo, applied like the other services' migrations. ci-svc never changes the schema.
- ci-svc's runtime role needs `USAGE` on the schema, `SELECT, INSERT, UPDATE, DELETE` on its tables, and `USAGE` on `ci_svc.outbox_id_seq`.
- No session state: it works through transaction poolers.

## Redis

Only ci-svc connects, never runners. Keys and channels all start with `ci:`: the channels `ci:jobs:wake` and `ci:log:<job_id>`, and short-lived counters `ci:rl:register:<ip>`. Nothing in Redis is needed after a restart: **do not back it up**, and flushing it is harmless.

## Kubernetes and Compose notes

- ci-svc: a Deployment with 2 or more replicas, a Service for `:9090` (cluster-internal) and one for `:9091` (reachable by runners). No volume.
- Runner: a static binary with a systemd unit on a VM, or a container with access to a Docker daemon (`DOCKER_HOST`). The runner never bind-mounts host paths into jobs, so a remote daemon or Docker-in-Docker works.
- Images for amd64 and arm64 are not built yet (this repo has no Dockerfile for the programs yet).

## Backup and restore

What the backup must contain for CI:

| What | Where | Why |
|---|---|---|
| The `ci_svc` schema | the database dump | pipelines, jobs, logs, runners (token hashes), encrypted secrets, outbox |
| `GIT2_CI_SECRETS_KEY` | the snapshot's secrets, next to the other instance secrets | without it, every CI secret is unreadable; ci-svc refuses to start with the wrong one |
| The bucket prefix `ci/` | the bucket (from W6) | archived logs, artifacts. `ci/cache/` may be skipped |

What happens after a restore:

- Jobs that were **running** at backup time fail as `RUNNER_LOST` within about a minute, and their pipelines finish. Pending jobs simply run.
- Runners registered before the backup reconnect by themselves. Runners registered after it get `UNAUTHENTICATED` and must be registered again.
- Redis starts empty; nothing is lost.
