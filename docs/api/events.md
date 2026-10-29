# Events

ci-svc publishes domain events through its outbox, in the team's format (the same as git-svc's, codeplane ADR 0007). Proto: [`events.proto`](../../app/proto/ci/v1/events.proto).

## Delivery

- Each event is written to `ci_svc.outbox` **in the same transaction** as the change it describes: an event exists if and only if the change happened.
- A relay (codeplane builds the first one, WBS-1.3) delivers pending rows **at least once**. You may see an event twice: drop duplicates by `event_id`.
- Order is not guaranteed across pipelines. Within one pipeline, `PipelineCreated` is written before any `JobFinished`, and `PipelineFinished` after the last `JobFinished`, but a relay may still deliver them out of order. Use `occurred_at` and the status, not arrival order.

| Column | Content |
|---|---|
| `id` | `BIGSERIAL`, the relay's reading order |
| `event_id` | `UUID`, the event's own ID |
| `aggregate_type` | `pipeline` or `job` |
| `aggregate_id` | the pipeline or job ID |
| `event_type` | full proto message name, e.g. `ci.v1.PipelineFinished` |
| `payload` | the message in canonical protobuf JSON (`jsonb`): lowerCamelCase fields, enums as names |
| `created_at`, `published_at` | when written; `NULL` until the relay delivered it |

Decode with `protojson.Unmarshal` into the type named by `event_type`.

## `ci.v1.PipelineCreated`

When `TriggerPipeline` creates a pipeline (not when it returns an existing one).

```json
{"pipelineId": "<id>", "repositoryId": "<id>", "ref": "refs/heads/main",
 "commitSha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
 "trigger": "PIPELINE_TRIGGER_PUSH", "occurredAt": "2026-10-29T14:02:53.700Z"}
```

## `ci.v1.PipelineFinished`

Once per pipeline, when it reaches `SUCCESS`, `FAILED` or `CANCELED`. This is the kickoff deck's `pipeline.finished`: **Dash uses it for the ✓/✗ on commits and merge requests**, and for "pipeline failed" notifications.

```json
{"pipelineId": "<id>", "repositoryId": "<id>", "ref": "refs/heads/main",
 "commitSha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
 "status": "PIPELINE_STATUS_FAILED", "occurredAt": "2026-10-29T14:03:10.120Z"}
```

A pipeline whose `.git2-ci.yml` is invalid is created and finished at once (`FAILED`), so it produces both events.

## `ci.v1.JobFinished`

Once per job, when it reaches `SUCCESS`, `FAILED`, `CANCELED` or `SKIPPED`.

```json
{"jobId": "<id>", "pipelineId": "<id>", "repositoryId": "<id>", "name": "unit",
 "status": "JOB_STATUS_FAILED", "failureReason": "JOB_FAILURE_REASON_SCRIPT_FAILURE",
 "occurredAt": "2026-10-29T14:03:09.900Z"}
```

`failureReason` is absent unless the job failed (protobuf JSON leaves out default values).
