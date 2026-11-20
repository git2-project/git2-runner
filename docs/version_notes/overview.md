# Version notes overview

One line per CI-lane change that touches another lane, **newest first**.
Open the linked note for the details, and use this page to check what your
lane has and hasn't done yet.

| Tag | Meaning |
|---|---|
| 🔴 Breaking | Something your lane already built stops working until you change it. |
| 🟠 Action | Nothing breaks, but your lane must build or change something. |
| 🟢 New | New RPC, field, event or option. Use it when you're ready. |
| ⚪ FYI | Inside the CI lane only. Nothing to do. |

**Affects** names the lanes with something to read or do: Dash (Bilge),
Console (Beste), Codeplane (Apo), Deploy (Ceyda).

Notes live in `<year>/<month>/<day>_<month>.md`. When you add one, put it in
the right month folder and add a line at the top of that month's table below.

---

## 2026 · November

| Date | What changed | Affects | Tag |
|---|---|---|---|
| [18 Nov](2026/11/18_11.md) | A push starts its pipelines: ci-svc serves `events.v1.EventSink`, stores each `RepositoryPushed` in `ci_svc.inbox`, and starts one pipeline per pushed branch or tag (key `"<event_id>:<ref>"`, retried until it works). Tested end to end with git-svc `v0.3.0` and a runner. Deploy sets `GIT2_GITSVC_EVENT_SINKS=ci-svc:9090=git.v1.RepositoryPushed` on git-svc. Also: a restarting runner removes its crashed jobs' containers, networks and volumes (label `io.git2.runner-id`); A-04 answered by codeplane's `GetArchive`; our outbox relay is #48. | Deploy, Dash | 🟠 Action |
| [15 Nov](2026/11/15_11.md) | `RetryJob`: run one finished job again as a new attempt in the same pipeline; `Job` gets `attempt` and `retry_of_id`. `PipelineFinished` can now arrive again for a retried pipeline (keep the latest `occurredAt`). Migration 006 adds job attempts and the `ci_svc.inbox` table. Images for ci-svc and git2-runner build for amd64 and arm64 (not published yet). Dash adds a Retry button and attempt history; Deploy applies 006 and adds ci-svc to Compose. Ends with the week 3 status. | Dash, Deploy | 🟠 Action |
| [9 Nov](2026/11/09_11.md) | ci-svc reads `.git2-ci.yml` from git-svc (`GetBlob`): new variable `GIT_SVC_GRPC_ADDR` (Compose: `git-svc:9090`). A `.git2-ci.yml` that is a directory, symlink, LFS pointer, binary or over 256 KiB now gives a `FAILED` pipeline with that reason. Deploy sets the variable when ci-svc joins Compose. | Deploy, Dash | 🟠 Action |
| [8 Nov](2026/11/08_11.md) | Agreed with codeplane (ADR 0010): each producer's relay pushes its outbox to `events.v1.EventSink.Deliver`. ci-svc records `RepositoryPushed` and starts the pipelines itself, so nobody calls `TriggerPipeline` for pushes. Codeplane publishes `events.v1` and builds the relay; Dash plans an `EventSink` for `PipelineFinished`; Deploy wires relay subscribers. Also: retrying a whole pipeline is a `TriggerPipeline` call with a new key; `RetryJob` comes in W4. | Codeplane, Dash, Deploy | 🟠 Action |

## 2026 · October

| Date | What changed | Affects | Tag |
|---|---|---|---|
| [31 Oct](2026/10/31_10.md) | Review fixes before PR #28 merges: `idempotency_key` takes 1–512 bytes of UTF-8 so `"<event_id>:<ref>"` fits every ref; how pushes reach `TriggerPipeline` and how events leave the outbox are now "to be agreed" in WBS-1.3; a lock-order fix in ci-svc. Codeplane agrees the delivery path with Osman in W2. Also three runner fixes found by new unit tests (no action). | Codeplane | 🟠 Action |
| [29 Oct](2026/10/29_10.md) | First `ci.v1` contract and working code: `PipelineService` (trigger, list, cancel, live logs, validate), `RunnerAdminService`, `SecretService`, three outbox events, `.git2-ci.yml` v0 with JSON schema. `ci_svc` migration on git2 branch `feat/ci-svc-schema`. git2 merges the migration; Dash and console plan views; codeplane builds file and archive RPCs; deploy adds ci-svc, its two ports and the secrets key to the backup. | All | 🟠 Action |
| [26 Oct](2026/10/26_10.md) | Decided: ci-svc lives in git2-runner, and one Redis is shared by all services. The CI lane keeps its state in Postgres and uses Redis only for signals, with keys starting `ci:`. Deploy adds the shared Redis; git2's README should point `ci-svc` here; everyone prefixes their Redis keys. | All | 🟠 Action |
| [25 Oct](2026/10/25_10.md) | git2-runner repo opened. ci-svc (pipelines, job queue, logs, artifacts, CI secrets) is planned to live here next to the runner. Nothing to build yet, but please check the assumptions about your repo. | All | ⚪ FYI |
