# Handoff prompt: continue the runner (git2-runner)

Paste everything below the line into a new Claude Code session opened in a fresh clone of `git2-project/git2-runner`. Written 2026-10-31 by the agent that built v0, for the agent that continues on another machine.

---

You are continuing work on **git2-runner**, the CI lane of git2 (a self-hosted GitHub/GitLab-like platform built as a 9-week university team project). I am **Osman**, the product owner of the project and the owner of this repo (GitHub: `osman-yahya`). Your focus in this session is **the runner**: the `git2-runner` program that runs CI jobs on build hosts. Do not start on ci-svc features unless a runner task needs them.

I am not a gRPC specialist: when you explain a design, say it in plain terms first, then the details. Ask me before decisions that are mine (scope, anything another lane must agree to); decide technical details yourself and tell me what you chose.

## 1. Get oriented (do this first, before any code)

1. Read, in this order: `CLAUDE.md` (hard rules, commands, conventions; they override your defaults), `docs/CLAUDE.md` (how docs and version notes work), `docs/design/plan.md` (architecture), `docs/decisions/` (ADRs 0001–0009, especially 0002, 0005, 0006 and 0008), `docs/api/runners.md` (the runner protocol and how to install a runner), `docs/version_notes/overview.md` and the notes of 29 and 31 Oct.
2. Find the right branch. Run `gh pr view 28 -R git2-project/git2-runner --json state,mergedAt`.
   - If PR #28 is **merged**: `git switch main && git pull`, then branch from `main`.
   - If it is still **open**: it has review fixes pending approval. Branch from `origin/feat/ci-v1-v0` and say so in your PR (it will be rebased onto `main` after #28 merges). Never push to `feat/ci-v1-v0` yourself.
3. Check the board: `gh issue list -R git2-project/git2-runner` and the **Runner** view of org project #1 (https://github.com/orgs/git2-project/projects/1). Runner issues: #23 (hardening), #17 (images, CI workflow), #19 (source checkout, blocked).
4. Prove the environment works **before** changing anything: `cd app && make tests && make lint`, and `make tests-docker` if Docker is available. In the devcontainer everything is provided. Outside it, read "Outside the devcontainer" in `CLAUDE.md` (Go toolchain, throwaway Postgres, zsh). Report the result to me in two lines.

## 2. What exists today (verified on 2026-10-31)

- `app/cmd/git2-runner`: `register` (trades a one-time `g2rt_…` token for a runner token, writes a 0600 JSON config), `run`, `version`. Config path: `-config` or `GIT2_RUNNER_CONFIG`.
- `app/internal/runner`:
  - `runner.go`: `Config`, `DockerLimits` (defaults 2048 MB, 2 CPUs, 1024 pids), `Dial` (bearer token interceptor, TLS for `https://`), the `Executor` interface (`Run(ctx, Spec, io.Writer) Result`), `BuildScript` (echoes each line, `set -e`).
  - `agent.go`: the loop. `RequestJob` long poll (ci-svc holds it up to 25 s) with backoff; one job at a time; heartbeat `UpdateJob(RUNNING)` every 15 s against a 60 s lease; cancel (`cancel_requested`) and "job lost" (`NOT_FOUND`/`FAILED_PRECONDITION`) end the job; timeout from the assignment; final report retried up to 6 times; on SIGTERM the current job finishes first.
  - `logship.go`: buffers output, sends `AppendLog` every ~500 ms by byte offset (idempotent, so retries are safe), drops the oldest output past a buffer limit, flushes everything before the final status.
  - `mask.go`: replaces secret values with `[MASKED]`, also across chunk boundaries and line by line for multi-line secrets; values shorter than 4 bytes are not masked.
  - `docker/`: Docker Engine API (`github.com/moby/moby/client`, never the CLI). Per job: a container, a bridge network and a workspace volume at `/workspace`, all named `git2-job-<job_id>` and labeled `io.git2.job-id`, removed afterwards. Unprivileged, no Docker socket, `no-new-privileges`, init, memory/CPU/pids limits. Image pulled if missing (`IMAGE_PULL_FAILURE` on failure).
- Tests: `mask_test.go`, and `docker/docker_integration_test.go` (`//go:build integration`: success, timeout, missing image). The full system was run end to end: a two-stage pipeline in real containers with a masked secret.
- The runner binary must never contain database, Redis or ci-svc code: `go list -deps ./cmd/git2-runner | grep -E 'pgx|go-redis|cisvc'` must print nothing.
- **Known limitation:** jobs start in an **empty** `/workspace`. There is no source checkout until codeplane can stream a commit archive (assumption A-04, issue #19).

## 3. The work, in priority order

Do them as separate small PRs, one per item. For each: tests first or alongside, `make lint` clean, docs if anyone outside the repo can notice it.

### 3.1 Unit tests for the agent and the log shipper (start here)
`agent.go` and `logship.go` have **no unit tests**. Write them with a fake `civ1.RunnerServiceClient` and a fake `Executor` (no network, no Docker). Make the timings injectable (unexported fields or package vars set by tests) so tests run in milliseconds. Cover at least:
- success, script failure (exit code clamped to -1..255), executor failure reason passed through;
- timeout → `FAILED` + `TIMEOUT`; cancel via heartbeat → job stopped and reported as the server expects; heartbeat `NOT_FOUND` → job stopped and **not** reported;
- heartbeat transport errors keep the job running;
- final report retried on `UNAVAILABLE`, not retried on `NOT_FOUND`/`FAILED_PRECONDITION`/`INVALID_ARGUMENT`;
- `UNAUTHENTICATED` from `RequestJob` makes `Run` return `ErrUnauthenticated`; other errors back off;
- log shipping: offsets are contiguous, a failed `AppendLog` is resent at the confirmed offset, truncation stops sending, everything is flushed before the final `UpdateJob`, and no secret value ever reaches the fake client.
If a test exposes a bug, fix it in the same PR and tell me what it was.

### 3.2 Clean up leftovers after a crash (issue #23, first bullet)
If the runner or the host dies mid-job, its container, network and volume stay behind. At startup, remove the resources **of this runner only**. Several runners may share one Docker daemon, so first add a second label, `io.git2.runner-id=<runner_id>`, to everything the executor creates, then remove only resources with this runner's ID at `run` startup. Log what was removed. Integration test with `//go:build integration`.

### 3.3 Run several jobs at once (issue #23, second bullet)
Add `concurrent` (default 1) to the runner config. N worker loops share one gRPC connection; each runs one job at a time. Keep SIGTERM behaviour (all running jobs finish), and keep log lines tagged by `job_id`. Document it in `docs/api/runners.md`.

### 3.4 Shipping the runner (issue #17, runner part)
- A `Dockerfile` for `git2-runner` (static binary, non-root, amd64 and arm64), version set with `-ldflags "-X github.com/git2-project/git2-runner/app/internal/runner.Version=<v>"`.
- A systemd unit example and install steps in `docs/api/runners.md`.
- A GitHub Actions workflow running `make lint` and `make tests` (with a Postgres service). Coordinate the image registry with Ceyda: write it as an open question, don't pick one.

### 3.5 Later, ask me first
- Source checkout (#19): needs a new `RunnerService` RPC that streams the job's source, and codeplane's archive RPC (A-04). That is a contract change plus an ADR; propose the design to me before writing code.
- Kubernetes executor, private registry credentials, caches (WBS-10.6).

## 4. Rules you must not break

- **PRs only.** Never commit or push to `main`. Branch names `feat/runner-…`, `fix/runner-…`, `test/runner-…`. Conventional commits, small and focused, ending with the co-author line the harness gives you.
- Open PRs into `main` with **`ceydaakin`** as reviewer and me as assignee, and link the issue (`Closes #N`) so the board moves it. Ask me before merging anything; don't push or open PRs unless I said so in this session.
- **Build hosts are untrusted.** The runner only ever holds its own token and, during a job, that job's variables. Never write secrets to disk, a command line, or a log. Never bind-mount host paths into job containers (the daemon may be remote or Docker-in-Docker): use volumes and the API.
- **No migrations here.** The schema lives in the git2 repo (`migrations/003_ci_pipelines.sql`); this repo keeps a byte-identical copy for tests. The runner should never need one.
- **Never edit other repos** (`git2`, `git2-codeplane`, `git2-deploy`). Write what they must change as an action item in a version note, or ask me to open an issue.
- **A doc with every visible change** (`docs/CLAUDE.md`): runner config keys, CLI flags, install steps and behaviour changes go into `docs/api/runners.md`, a version note `docs/version_notes/2026/<month>/<day>_<month>.md`, and a line in `overview.md`. Plain English, examples, no secrets.
- Check library APIs (moby client, grpc-go) with `go doc` or Context7, not memory. The moby client in this repo is the new `github.com/moby/moby/client` API with options/result structs.

## 5. The team (for docs and issue text)

Use names, not `@` mentions, unless I ask.

| Who | Lane | Repo |
|---|---|---|
| Osman (me) | Pipelines / CI, product owner | git2-runner |
| Bilge | Dash (web UI, dash-api) | git2 |
| Beste (Zarif Beste Süner) | Management console, Scrum Master | git2 |
| Apo (Abdullah Akcan) | Git core, codeplane | git2-codeplane |
| Ceyda (Ceyda Akın, `ceydaakin`) | Deploy and backup; my backup reviewer | git2-deploy |

Issues go on org project #1. The Runner view only shows issues **in git2-runner**, so create CI issues there. Title style: `feat(runner): … (WBS-10.2)`, labels `enhancement` or `documentation`, assignee `osman-yahya`.

## 6. Lessons from the last session

- A race test proves nothing until you have seen it fail. Turn the protection off once, confirm the test catches it, then turn it back on (that is how the cancel deadlock fixed on 31 Oct was found).
- Run `make lint` before every commit; golangci-lint also checks import grouping.
- `make proto` after any `.proto` change, and commit `app/gen/` with it.
- Leave no containers, volumes or test databases behind; check with `docker ps -a --filter label=io.git2.job-id`.

When you finish a task, tell me in a few lines what changed, how you verified it, and what is next. Start with step 1 now.
