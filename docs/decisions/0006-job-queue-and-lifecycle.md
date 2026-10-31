# 0006. The job queue and a job's lifecycle

- Status: Accepted
- Date: 2026-10-29

## Context

ADR 0003 put the queue in Postgres and the wake-ups in Redis. This ADR fixes the details that the code and the other lanes depend on: the states of a job and a pipeline, how a runner holds a job, what happens when it disappears, and how logs travel.

We also looked again at a "real" queue (a message broker, or a Redis list or stream). A broker adds a service that Ceyda must deploy and back up, and a second copy of the queue that must be reconciled with Postgres after every crash or restore. Our load (a few runners, a few jobs a minute) is far below what one indexed Postgres table handles. The row-lease pattern below is what GitLab, Woodpecker and many job runners (river, good_job, Oban) use on Postgres.

## Decision

**States.** A job is `created` (its stage is waiting), `pending` (queued), `running` (leased by one runner), then `success`, `failed`, `canceled` or `skipped`. A pipeline is `pending`, `running`, then `success`, `failed` or `canceled`. A final state never changes.

**Stages.** All jobs of a stage are queued together when every job of the stage before succeeded. After a failed or canceled job, later stages are `skipped` and the pipeline ends `failed` (or `canceled`) once the current stage is done. Stages without jobs are passed over.

**Lease.** `RequestJob` takes the oldest pending job whose tags are all among the runner's labels, in one `UPDATE … FOR UPDATE SKIP LOCKED` statement. The lease lasts 60 s. Every `UpdateJob(RUNNING)` heartbeat (every 15 s) and every `AppendLog` extends it. A reaper in every replica, every 15 s, finishes running jobs whose lease ran out: `failed` with reason `runner_lost`, or `canceled` if a cancel was pending. This one mechanism covers a crashed runner, a deleted runner, a lost network and a restore from a backup taken mid-run.

**Long poll.** `RequestJob` waits up to 25 s. It tries the lease at once, again on every `ci:jobs:wake` signal, and every 3 s without one.

**Cancel.** Waiting jobs are canceled at once. Running ones get a flag; the runner sees it in the next heartbeat reply, stops the container and reports, and ci-svc records `canceled` whatever the runner reported.

**Logs.** `AppendLog` is a unary call, not the client stream sketched in the plan: idempotent by byte offset, so a retry after a timeout cannot duplicate output, and simple behind proxies. Each call is one row in `job_log_chunks`. A log is capped at 4 MiB, then a notice is appended and later output is dropped. `StreamJobLog` reads the rows and, with `follow`, waits for a `ci:log:<job_id>` signal or polls every second. The signal carries no data: followers always read Postgres. Archiving finished logs to the bucket comes with artifacts (W6); until then logs stay in Postgres, which the backup covers.

**Locks.** Every transaction locks, in this order:

1. the job rows it starts from, in `(stage_index, id)` order: one job for a lease, a result, a log chunk or the reaper; all of a pipeline's jobs for `CancelPipeline`;
2. the pipeline row;
3. only then, to queue or skip them, jobs of later stages that are still `created`.

A transaction that waits for a later-stage row always holds an earlier-stage one, and `CancelPipeline` takes earlier stages first, so no two transactions can wait for each other. A transaction aborted by a deadlock or serialization failure is still retried up to three times, as a safety net.

*Revised 2026-10-31, after Apo's review of PR #28.* The first version said "job rows before the pipeline row", which was not true: queueing the next stage writes job rows after the pipeline row. `CancelPipeline` locked jobs by ID, and job IDs follow the order of the file, not of the stages. So with a later-stage job written first in `.git2-ci.yml`, canceling while a job reported its result deadlocked. `TestCancelRacesWithResults` reproduced it (51 aborted calls in 10 runs with retries turned off) and passes without retries since the fix.

**Events.** `PipelineCreated`, `JobFinished` (once per job, `skipped` included) and `PipelineFinished` (once per pipeline) go to `ci_svc.outbox` in the transaction that makes the change.

## Consequences

- A queued job reaches an idle runner in well under a second with Redis, and within 3 s without it (target: 5 s).
- A job whose `RequestJob` reply never reaches its runner (the connection broke at that moment) stays `running` until its lease runs out, then fails as `runner_lost`. The pipeline shows it; a retry RPC follows in W4–W5.
- The runner enforces the job timeout. If the runner dies, the lease ends the job instead.
- Logs in Postgres are bounded by the 4 MiB cap per job until the W6 archive moves finished logs to the bucket.
