# 0003. Postgres holds the state; the shared Redis carries signals

- Status: Accepted
- Date: 2026-10-26

## Context

- The kickoff deck says "no Redis". The charter and the project plan use Redis for the job queue and for live log pub/sub. git2 core already keeps sessions in Redis.
- On 2026-10-26 Osman decided: **one Redis, shared by all git2 services.**
- Redis is not part of the backup, and risk R-03 forbids durable data that lives only in Redis.
- Targets: a job starts within 5 s, and a log line reaches the browser within 2 s.
- The Supabase transaction pooler does not support `LISTEN/NOTIFY`.

We considered making a Redis list the queue, with Postgres as the record. We rejected it: there would be two copies of the queue, and after every Redis loss or restore they would have to be reconciled.

## Decision

- **Postgres holds all state**: pipelines, the job queue (`jobs` rows leased with `FOR UPDATE SKIP LOCKED`), log chunks, secrets and runners.
- **Redis carries signals only**, and only ci-svc connects to it:
  - `ci:jobs:wake` (pub/sub) wakes long-polling runners;
  - `ci:log:<job_id>` (pub/sub) carries new log chunks to followers connected to other replicas;
  - `ci:rl:<action>:<key>` holds rate-limit counters.
- Every key and channel starts with `ci:`. ci-svc writes to Postgres first and publishes afterwards.
- **Fallbacks when Redis is down or drops a message.** A waiting long poll tries to lease a job every 3 s, and log followers read Postgres every second. A Redis fault does not make ci-svc unready.

## Consequences

- A restore starts with an empty Redis and loses nothing. The W8 fault test (kill Redis mid-pipeline) passes by design.
- The fan-out and the fallback live together in one package (`internal/notify`).
- The project plan's WBS-10.3 says "Redis job queue". The next report should describe it as "the queue is in Postgres, and Redis wakes the runners".
