# What the CI lane assumes about the other repos

The other repos are still young, so we build against these assumptions until their owners confirm or correct them. **If you own one, please answer on its git2 issue** (or tell Osman). When an assumption is confirmed or changed, we update its status here, and the change gets a version note if it affects anyone.

| ID | Topic | Owner | Needed by | Status |
|---|---|---|---|---|
| [A-01](#a-01--ci-svc-lives-in-git2-runner) | ci-svc lives in git2-runner | Osman, Bilge and Beste informed | W1 | **confirmed** 2026-10-26 ([ADR 0001](../decisions/0001-ci-svc-lives-in-git2-runner.md)) |
| [A-02](#a-02--how-a-push-reaches-us) | How a push reaches us | Apo | W4 | open |
| [A-03](#a-03--reading-git2-ciyml-at-a-commit) | Reading `.git2-ci.yml` at a commit | Apo | W4 | open |
| [A-04](#a-04--how-a-job-gets-the-source-code) | How a job gets the source code | Apo | W4 | open |
| [A-05](#a-05--the-ci-secrets-key-is-in-every-backup) | The CI secrets key is in every backup | Ceyda | W5 | open |
| [A-06](#a-06--one-bucket-and-our-prefix) | One bucket, and our prefix | Ceyda | W6 | open |
| [A-07](#a-07--live-logs-in-the-browser) | Live logs in the browser | Bilge | W6 | open |
| [A-08](#a-08--all-in-one-image) | All-in-one image | Ceyda, git2 owners | W3 | open |
| [A-09](#a-09--how-runners-reach-ci-svc) | How runners reach ci-svc | Ceyda | W3 | open |
| [A-10](#a-10--who-checks-permissions) | Who checks permissions | everyone | W2 | open |
| [A-11](#a-11--the-web-apps-reach-us-through-their-apis) | The web apps reach us through their APIs | Bilge, Beste | W5 | open |
| [A-12](#a-12--ids-not-names) | IDs, not names | Apo | W1 | **confirmed** 2026-10-30: codeplane ADR 0002 |
| [A-13](#a-13--one-outbox-format) | One outbox format | Apo, everyone | W4 | format **confirmed** 2026-10-29 (codeplane ADR 0007); delivery path open (WBS-1.3) |
| [A-14](#a-14--one-go-version-one-dsn-variable) | One Go version, one DSN variable | everyone | W2 | **confirmed** 2026-10-29: codeplane ADR 0010 moved to Go 1.26 and `POSTGRES_DSN` |
| [A-15](#a-15--the-shared-redis) | The shared Redis | Ceyda, everyone | W2 | open |

Weeks: W1 starts 26 Oct 2026, W2 2 Nov, W3 9 Nov, W4 16 Nov, W5 23 Nov, W6 30 Nov.

---

### A-01 · ci-svc lives in git2-runner

**Confirmed on 2026-10-26** ([ADR 0001](../decisions/0001-ci-svc-lives-in-git2-runner.md)). The pipeline engine the charter calls `ci-svc` (reads `.git2-ci.yml`, plans pipelines, queues jobs, keeps logs and artifacts) is built in this repo, under the same name. git2 gets no `ci-svc/` directory, and its README's table changes in a later PR.
**Why:** the charter gives ci-svc to Osman, and the team map makes Bilge and Beste the people committing to git2.

### A-02 · How a push reaches us

**We assume:** whatever delivers codeplane's `git.v1.RepositoryPushed` event (codeplane itself, or a shared outbox relay) calls our `TriggerPipeline` once per created or updated branch or tag in the event, with the event ID plus the ref as the idempotency key. Deleted refs trigger nothing. Delivery is at least once, and we drop duplicates.
**If wrong:** ci-svc reads codeplane's events itself, which needs read access to its outbox.
**Status (2026-10-30):** to be agreed with Apo in the WBS-1.3 design (W2). Who calls `TriggerPipeline` is not decided.

### A-03 · Reading `.git2-ci.yml` at a commit

**We assume:** codeplane's gRPC API can return one file's content at a given commit. Its `repository.proto` plans this as `CommitService` for W2–W3.
**If wrong:** pushes cannot start pipelines, and the M3 demo (11 Dec) is at risk.
**Today (2026-10-29):** ci-svc reads files through a `Source` interface. In production it answers `UNAVAILABLE` until this RPC exists; in development `GIT2_CISVC_DEV_CONFIG_DIR` serves files from disk.

### A-04 · How a job gets the source code

**We assume (v0):** codeplane can stream a commit as a tar archive over gRPC (`git archive`). ci-svc fetches it and passes it to the runner, so runners never talk to codeplane.
**Later:** the runner clones over HTTPS with a short-lived, read-only job token for that one repository. That needs the gateway and auth-svc to accept such tokens.
**If wrong:** we need the job token path in v0, which involves three lanes.

### A-05 · The CI secrets key is in every backup

**We assume:** ci-svc's secrets key (`GIT2_CI_SECRETS_KEY`, 32 bytes) is kept with the instance's other secrets (the secrets part of `git2.yaml`, or wherever those live), so it is in every snapshot and a restore puts it back before ci-svc starts. The database only ever holds ciphertext.
**If wrong:** a restore brings back CI secrets nobody can decrypt (risk R-09).

### A-06 · One bucket, and our prefix

**We assume:** the instance has one bucket, configured by one URL in `gocloud.dev/blob` form (`s3://…`, `gs://…`, `azblob://…`, `file:///…`). We own the `ci/` prefix: `ci/logs/`, `ci/artifacts/` and `ci/cache/`. Backups and bucket migrations may skip `ci/cache/`; jobs rebuild it. The database stores object keys, never bucket URLs.
**If wrong:** if artifacts must be in the snapshot itself, or live in a separate bucket, we add a second bucket setting.

### A-07 · Live logs in the browser

**We assume:** browsers get live job logs from dash-api, which reads our `StreamJobLog` gRPC stream and passes it on as SSE or WebSocket. Browsers never call ci-svc.
**If wrong:** we would serve gRPC-Web or SSE ourselves, through the gateway.

### A-08 · All-in-one image

**We assume:** the all-in-one image runs ci-svc as its own process next to `git2`. git2's `cmd/git2` does not import our code.
**If wrong:** we add a small public package so `cmd/git2` can mount ci-svc in-process.

### A-09 · How runners reach ci-svc

**We assume:** runners connect to ci-svc's runner port directly over TLS (its own port in Compose, its own Service in Helm), not through the gateway. Only that port is exposed to build hosts.
**If wrong:** the gateway must pass HTTP/2 gRPC through to that port.

### A-10 · Who checks permissions

**We assume:** ci-svc's internal port (pipelines, runner admin, secrets) is reachable only inside the instance network. dash-api and admin-api check the user's permissions before they call it, as they do for git-svc. Service-to-service authentication (mTLS) comes later, for all services at once.
**If wrong:** we check permissions ourselves, which means calling auth-svc.

### A-11 · The web apps reach us through their APIs

**We assume:** Dash reaches us through dash-api and the console through admin-api, the Go services in git2 that call our gRPC API. The React apps never call ci-svc directly.
**If wrong:** the gateway would need REST or gRPC-Web routes to ci-svc.

### A-12 · IDs, not names

**We assume:** we store only repository IDs and owner IDs (the UUIDs issued by codeplane and auth-svc), never names or slugs. Callers turn IDs into names for display. This is the same rule as codeplane's ADR 0002.
**If wrong:** we would have to follow renames.
**Confirmed on 2026-10-30** by Apo (review of PR #28): codeplane's ADR 0002 and its 26 Oct version note.

### A-13 · One outbox format

**We assume:** every service writes its events to `<schema>.outbox` in the same transaction as the change: an ID (UUID), the type (the full proto message name, for example `ci.v1.PipelineFinished`), the payload (canonical protobuf JSON) and `created_at`. codeplane is the first service to build this; we copy its format.
**Format confirmed** (codeplane ADR 0007). **Delivery still open:** a relay that reads every service's outbox would break the one-schema-per-service rule (codeplane ADR 0006), so how events leave each outbox is to be agreed in the WBS-1.3 design (W2). Delivery stays at least once.
**If wrong:** we follow whatever format codeplane ships.

### A-14 · One Go version, one DSN variable

**Confirmed on 2026-10-29:** codeplane's ADR 0010 moved it to Go 1.26 and `POSTGRES_DSN`, as in git2. All three repos now use Go 1.26 (devcontainers pin 1.26.10), `POSTGRES_DSN`, `POSTGRES_MAX_CONNS` and `TEST_POSTGRES_DSN`.
**If wrong:** nothing breaks; we rename.

### A-15 · The shared Redis

**Decided (Osman, 2026-10-26):** the instance runs one Redis, shared by every git2 service.

**We assume:**
- It runs in the Compose file and in the Helm chart (Ceyda).
- It is not part of the backup, and needs no persistence, because no service keeps durable data only in Redis.
- ci-svc reaches it with the same settings as git2's services: `REDIS_ADDR` and `REDIS_PASSWORD`.
- Every service starts its keys and channels with its own name. Ours start with `ci:`, as listed in [plan.md §3.6](plan.md#36-what-goes-in-redis). auth-svc today uses `sess:` and `revoked:`, which don't clash with ours, but a team-wide rule avoids collisions later.

**If wrong:** if each service gets its own Redis, we only change our address. If Redis must be backed up, nothing changes for us either, since we keep nothing there that a restore needs.
