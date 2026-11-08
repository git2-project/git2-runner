# 0010. Events travel by gRPC push to an EventSink

- Status: Accepted
- Date: 2026-11-08
- Agreed with: Apo (codeplane), on git2-runner #33 (WBS-1.3)

## Context

Every service writes its domain events to its own outbox, in the format of codeplane ADR 0007. Nothing delivered them yet. Before codeplane built its relay (git2-codeplane #4), we had to agree on how an event leaves an outbox and reaches its consumers. In particular, how codeplane's `git.v1.RepositoryPushed` reaches ci-svc (A-02, A-13).

These limits were fixed:

- Each service reads only its own Postgres schema. This rule is in git2's `CLAUDE.md` and `README.md`. (Earlier docs here cited codeplane ADR 0006, which only says that git-svc owns `git_svc`.)
- Everything runs behind Supabase's transaction pooler, so there is no `LISTEN/NOTIFY` and no session lock. Relays poll.
- The shared Redis is not persisted (A-15). A Redis stream could lose events it had already marked delivered, which breaks at-least-once delivery.
- Outbox `id` order is not commit order (codeplane ADR 0007), so a consumer cursor over `id` is unsafe.

## Decision

1. **Producers push.** Each producer runs a relay over its own outbox. For each pending row it calls `events.v1.EventSink.Deliver` on every consumer subscribed to that event type. It marks the row published only when all of them answered `OK`. The list of consumers is producer configuration.
2. **One contract.** `events.v1.EventSink` is defined once, in git2-codeplane (`proto/events/v1/`), with a pointer in git2's `contracts/events/`, as for `git.v1`. `DeliverRequest` carries the outbox columns: `event_id`, `event_type`, `aggregate_type`, `aggregate_id`, `occurred_at` and `payload` (canonical protobuf JSON).
3. **What an answer means.** Each call has a deadline of 10 s.

   | Answer | Meaning for the relay |
   |---|---|
   | `OK` | The consumer stored the event, or had seen it before. Mark it delivered |
   | `UNAVAILABLE`, `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED`, `ABORTED`, `INTERNAL` | Retry with backoff. Never drop |
   | `INVALID_ARGUMENT`, `FAILED_PRECONDITION`, `UNIMPLEMENTED` | Permanent. Log at ERROR, mark delivered and move on, so one bad event never blocks the rest |

4. **`Deliver` only records.** A consumer stores the event and answers; the work happens afterwards, so a slow dependency cannot stall delivery. In ci-svc:
   - `Deliver` writes the event to a new table, `ci_svc.inbox`. Its `event_id` is unique, so a redelivery changes nothing.
   - A worker then handles each `git.v1.RepositoryPushed`. **ci-svc decides which refs start CI.** For each ref update with a non-zero new SHA, it reads `.git2-ci.yml` at that commit with codeplane's `CommitService.GetBlob` and creates the pipeline, with the idempotency key `"<event_id>:<ref>"`.
   - Deleted refs, `refs/notes/*` and refs over 255 bytes are skipped as a normal outcome. A missing `.git2-ci.yml` (`NOT_FOUND`) means "no CI for this commit".
   - ci-svc subscribes to `git.v1.RepositoryPushed` only. Any other type gets `UNIMPLEMENTED`.
5. **ci-svc delivers its own outbox the same way.** A relay over `ci_svc.outbox` pushes `ci.v1.PipelineCreated`, `ci.v1.PipelineFinished` and `ci.v1.JobFinished` to their subscribers. The first subscriber is dash-api, for the pipeline status on merge requests. The relay is copied from codeplane's for now; whether it becomes a shared Go module is decided at the W4 review.

## Consequences

- **New table.** git2 gets a migration `004_ci_inbox.sql` for `ci_svc.inbox`, and this repo gets its copy (ADR 0009). It is built with #20 (a push starts a pipeline end to end, W4).
- **New service.** ci-svc's internal port serves `events.v1.EventSink` once codeplane publishes the contract. Pushes no longer go through `TriggerPipeline`. That RPC stays for manual runs (dash-api) and for development.
- **dash-api** serves `EventSink` to receive our events.
- **Deploy** configures each relay with its consumers' addresses, on the instance network only.
- **Restore.** Inbox and outbox rows are in Postgres, so they are in every snapshot. After a restore, rows that were not yet published are delivered again, and consumers drop the duplicates by `event_id`. An inbox row whose pipeline was not created yet is handled again by the worker; the pipeline's idempotency key keeps that safe.
- **Docs.** A-02 and A-13 are agreed, and `docs/api/events.md`, `docs/api/pipelines.md` and `docs/api/overview.md` describe this path instead of "to be agreed".
