# 0005. Runners long-poll for jobs

- Status: Accepted
- Date: 2026-10-26

## Context

A runner must hear about a new job within 5 s, through proxies and load balancers, whichever ci-svc replica it reaches. The options:

- **A bidirectional stream.** The connection is pinned to one replica, needs keepalives and reconnect logic, and has to be drained on every deploy.
- **A long poll.** A unary RPC that the server holds open until a job arrives or a timeout passes.

## Decision

- `RequestJob` is a long poll. ci-svc holds it for up to 30 s and answers early when ADR 0003's wake-up arrives. The runner calls again right away.
- Cancellation is delivered in the reply to `UpdateJob`, which the runner sends as its heartbeat every few seconds while a job runs.

## Consequences

- Any replica can answer any call, and ci-svc can be restarted or scaled without draining streams.
- Each waiting runner keeps one idle request open.
- A cancel reaches the runner within one heartbeat interval.
