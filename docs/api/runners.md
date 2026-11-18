# Runners

A runner (`git2-runner`) is the program that runs jobs on a build host. It connects out to ci-svc's runner port, so it needs only network access to the instance. Each job runs in its own Docker container.

- **Console (Beste):** [`RunnerAdminService`](#runneradminservice-for-the-console), to show runners and add, pause and delete them.
- **Deploy (Ceyda):** [installing a runner](#installing-a-runner).
- **Nobody outside this repo** calls [`RunnerService`](#runnerservice-the-runner-protocol); it is documented for completeness.

## RunnerAdminService (for the console)

On ci-svc's **internal port**. admin-api calls it and **checks that the user is an instance admin** first. Proto: [`runner_admin.proto`](../../app/proto/ci/v1/runner_admin.proto).

| RPC | Input → output | Errors |
|---|---|---|
| `CreateRegistrationToken` | optional `description` (≤ 200 chars), optional `expires_in` (default 1 h, 1 min to 7 days) → `token`, `expires_at` | `INVALID_ARGUMENT` |
| `ListRunners` | `page_size` (0 = 50), `page_token` → `runners` (oldest first), `next_page_token` | `INVALID_ARGUMENT` |
| `GetRunner` | `id` → `runner` | `INVALID_ARGUMENT`, `NOT_FOUND` |
| `UpdateRunner` | `id`, optional `paused`, optional `description` → `runner` | `INVALID_ARGUMENT`, `NOT_FOUND` |
| `DeleteRunner` | `id` → empty | `INVALID_ARGUMENT`, `NOT_FOUND` |

**`Runner`**: `id`, `name` (host name, from the runner), `description` (admin's note), `labels`, `executor` (`docker`), `version`, `platform` (`linux/amd64`), `status`, `paused`, `last_seen_at`, `created_at`, `updated_at`.

| `status` | Meaning |
|---|---|
| `RUNNER_STATUS_ONLINE` | Asked for work within the last 90 s |
| `RUNNER_STATUS_OFFLINE` | Not heard from for 90 s, or never |
| `RUNNER_STATUS_PAUSED` | An admin paused it: it finishes its current job, then takes no new ones |

**Adding a runner**, the console flow:

1. The admin clicks "New runner". admin-api calls `CreateRegistrationToken`.
2. Show the `token` **once**, with the command to run on the build host (below). ci-svc stores only a hash; the token cannot be shown again. It works once and expires at `expires_at`.
3. The runner registers and appears in `ListRunners`.

**Deleting** a runner stops its token at once. A job it was running fails as `RUNNER_LOST` within about a minute.

**After a restore** from a backup, runners registered before the backup keep working. A runner registered after the last backup must be registered again; say so on the Runners page if a runner reports `UNAUTHENTICATED`.

```sh
grpcurl -plaintext -d '{"description": "build-01", "expires_in": "3600s"}' \
  localhost:9090 ci.v1.RunnerAdminService/CreateRegistrationToken
# {"token": "g2rt_…", "expiresAt": "2026-10-29T15:02:53Z"}
```

Tokens start with `g2rt_` (registration) or `g2r_` (runner), so a leaked one is easy to recognise.

## Installing a runner

On the build host, with Docker Engine running:

```sh
git2-runner register -url https://<instance>:9091 -token <registration-token> -labels linux,docker
git2-runner run
```

- `register` writes `/etc/git2-runner/config.json` (or `-config <path>`, or `GIT2_RUNNER_CONFIG`) with permissions 0600. It holds the runner token. `-name` defaults to the host name. The token can also come from `GIT2_RUNNER_REGISTRATION_TOKEN`, to keep it out of the shell history.
- `run` asks for jobs and runs them one at a time with the Docker daemon from `DOCKER_HOST`, or the local one. SIGTERM lets the current job finish, then exits.
- `-url` is `https://…` for TLS, or `http://…` without (development only).
- **Labels** decide which jobs a runner takes: a job runs only on a runner that has every one of the job's `tags`. A job without tags runs anywhere.
- The config file's `docker` section sets limits per job container: `memory_mb` (default 2048), `cpus` (2), `pids_limit` (1024).
- The build host needs nothing but outbound access to the runner port. It never gets the database, Redis or bucket credentials. Run runners on a different VM from the git2 instance in production: they run other people's code.

### As a systemd service

[`deploy/systemd/git2-runner.service`](../../deploy/systemd/git2-runner.service) is an example unit. On the build host:

```sh
sudo useradd --system --create-home --home-dir /var/lib/git2-runner git2-runner
sudo install -d -o git2-runner -g git2-runner -m 0700 /etc/git2-runner
sudo install -m 0755 git2-runner /usr/local/bin/git2-runner
sudo -u git2-runner GIT2_RUNNER_REGISTRATION_TOKEN=<registration-token>   git2-runner register -url https://<instance>:9091 -labels linux,docker
sudo cp git2-runner.service /etc/systemd/system/
sudo systemctl enable --now git2-runner
```

The unit runs as `git2-runner` with the `docker` group, which is root-equivalent on that host. That is one more reason to keep runners off the git2 instance.

### In a container

The `git2-runner` image ([config.md → Images](config.md#images)) runs as user 65532 and keeps its config in the `/etc/git2-runner` volume. It needs a Docker daemon for the jobs:

```sh
docker volume create git2-runner
# Register once
docker run --rm -v git2-runner:/etc/git2-runner <registry>/git2-runner:<tag>   register -url https://<instance>:9091 -token <registration-token> -labels linux,docker
# Run, with the host's Docker socket and its group
docker run -d --name git2-runner --restart unless-stopped   -v git2-runner:/etc/git2-runner -v /var/run/docker.sock:/var/run/docker.sock   --group-add "$(stat -c %g /var/run/docker.sock)" <registry>/git2-runner:<tag>
```

Instead of the socket you can set `DOCKER_HOST` to a Docker-in-Docker daemon. The runner never bind-mounts host paths into jobs, so both work.

### After a crash

Each job runs in a container, a bridge network and a volume, all named `git2-job-<job_id>`. They are removed when the job ends. If the runner or its host dies mid-job, they stay on the Docker daemon, and the container may still be running.

- **On ci-svc:** the job's lease runs out, and the job fails with `JOB_FAILURE_REASON_RUNNER_LOST`. Retry it with `RetryJob`.
- **On the build host:** everything a job creates carries the labels `io.git2.job-id` and `io.git2.runner-id`. When the same runner starts again (`git2-runner run` with the same config), it first removes every container, network and volume with its own runner ID, and logs their names. Another runner's jobs on the same daemon are never touched.
- **Never run two runners with the same config on one daemon.** Each would remove the other's running jobs at startup. Register a second runner instead.
- Jobs started by a runner older than 18 Nov carry only `io.git2.job-id`. Remove those by hand: `docker ps -aq --filter label=io.git2.job-id`, and the same for `docker network ls` and `docker volume ls`.

## RunnerService (the runner protocol)

On ci-svc's **runner port**. Every call except `RegisterRunner` carries `authorization: Bearer <runner token>`. Proto: [`runner.proto`](../../app/proto/ci/v1/runner.proto).

| RPC | What it does |
|---|---|
| `RegisterRunner` | One-time registration token + name, labels, executor, version, platform → `runner_id`, `runner_token`. `UNAUTHENTICATED` for a bad, used or expired token; `RESOURCE_EXHAUSTED` after 10 attempts a minute from one address |
| `RequestJob` | Long poll, up to about 25 s: returns a `JobAssignment` (image, script, variables with secrets marked `masked`, timeout) or nothing |
| `UpdateJob` | `RUNNING` is the heartbeat (every 15 s; extends the 60 s lease); `SUCCESS` or `FAILED` (with `failure_reason`, and `exit_code` for `SCRIPT_FAILURE`) finishes the job. The reply's `cancel_requested` tells the runner to stop |
| `AppendLog` | Appends masked log bytes at a byte offset; idempotent, so retries are safe. Returns the stored size and whether the 4 MiB limit was hit |
