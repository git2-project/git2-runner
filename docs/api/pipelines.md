# Pipelines, jobs and logs: `ci.v1.PipelineService`

On ci-svc's **internal port** (`:9090`). For dash-api (pipeline views, logs, the "run pipeline" and "cancel" buttons) and for whoever delivers codeplane's push events. ci-svc does not check permissions: **dash-api checks that the user can read the repository** (and, for trigger and cancel, write to it) before calling. Conventions and error codes: [overview.md](overview.md). Proto: [`pipeline.proto`](../../app/proto/ci/v1/pipeline.proto).

| RPC | Input → output | Errors |
|---|---|---|
| [`TriggerPipeline`](#triggerpipeline) | repository, ref, commit, trigger, idempotency key → `pipeline`, `created` | `INVALID_ARGUMENT`, `NOT_FOUND` (no `.git2-ci.yml`), `ALREADY_EXISTS`, `UNAVAILABLE` |
| [`GetPipeline`](#getpipeline-and-listpipelines) | `id` → `pipeline` with jobs | `INVALID_ARGUMENT`, `NOT_FOUND` |
| [`ListPipelines`](#getpipeline-and-listpipelines) | `repository_id`, optional `ref`, page → `pipelines`, `next_page_token` | `INVALID_ARGUMENT` |
| [`CancelPipeline`](#cancelpipeline) | `id` → `pipeline` | `INVALID_ARGUMENT`, `NOT_FOUND` |
| `GetJob` | `id` → `job` | `INVALID_ARGUMENT`, `NOT_FOUND` |
| [`StreamJobLog`](#streamjoblog) | `job_id`, `offset`, `follow` → stream of `offset` + `data` | `INVALID_ARGUMENT`, `NOT_FOUND` |
| [`ValidateConfig`](#validateconfig) | `content` → `valid`, `errors` | `INVALID_ARGUMENT` (over 256 KiB) |

## Pipelines and jobs

A pipeline is one run of [`.git2-ci.yml`](ci-yaml.md) for one commit. Its jobs are grouped into stages; a stage starts when every job of the previous stage succeeded.

```
PipelineStatus:  PENDING ──► RUNNING ──► SUCCESS | FAILED | CANCELED
JobStatus:       CREATED ──► PENDING ──► RUNNING ──► SUCCESS | FAILED | CANCELED
                    └────────────────────────────► SKIPPED | CANCELED
```

| JobStatus | Meaning | Show as |
|---|---|---|
| `CREATED` | Waiting for an earlier stage | "created", grey |
| `PENDING` | Queued, waiting for a runner | "pending" |
| `RUNNING` | A runner is running it | spinner |
| `SUCCESS` | Script exited 0 | green ✓ |
| `FAILED` | See `failure_reason` | red ✗ |
| `CANCELED` | Canceled by a user | grey ⊘ |
| `SKIPPED` | Never ran because an earlier stage failed | grey, struck through |

Dash's draft OpenAPI has five statuses (`pending, running, success, failed, canceled`). For pipelines they map one to one. For jobs, map `CREATED` to `pending` and `SKIPPED` to `canceled`, or add the two values; we suggest adding them.

`JobFailureReason` says why a job failed: `SCRIPT_FAILURE` (non-zero exit; `exit_code` is set), `TIMEOUT`, `RUNNER_LOST` (the runner stopped reporting for 60 s, also after a restore from a backup), `IMAGE_PULL_FAILURE`, `RUNNER_SYSTEM_FAILURE`.

`Pipeline` fields: `id`, `repository_id`, `ref` (full, `refs/heads/main`), `commit_sha`, `status`, `trigger` (`PUSH` or `MANUAL`), `triggered_by_id` (user ID or empty), `error_message` (set when `.git2-ci.yml` is invalid; safe to show), `stages[]` (`name`, `jobs[]`), `created_at`, `started_at`, `finished_at`, `updated_at`.

`Job` fields: `id`, `pipeline_id`, `name`, `stage`, `status`, `failure_reason`, `exit_code` (optional), `runner_id`, `image`, `log_size` (bytes), `created_at`, `started_at`, `finished_at`.

## TriggerPipeline

Starts a pipeline for one commit. **Idempotent on `idempotency_key`**: the same key returns the first pipeline with `created = false`, so at-least-once event delivery never runs a pipeline twice.

| Field | Rule |
|---|---|
| `repository_id` | git-svc repository ID. Required |
| `ref` | `refs/heads/<branch>` or `refs/tags/<tag>`, at most 255 bytes. Required |
| `commit_sha` | 40 or 64 lowercase hex characters. Required |
| `trigger` | `PIPELINE_TRIGGER_PUSH` or `PIPELINE_TRIGGER_MANUAL`. Required |
| `triggered_by_id` | auth-svc user ID, or empty |
| `idempotency_key` | 1–200 printable ASCII characters. Required. For a push: `"<event_id>:<ref>"`. For a button: a UUID made when the user clicks, reused on retry |

**For pushes** (the relay of codeplane's `git.v1.RepositoryPushed`): call it once per entry of `ref_updates` whose `new_sha` is not all zeros (deleted refs start nothing), with `commit_sha = new_sha`. Treat `NOT_FOUND` as "this repository has no CI" (do not retry). Retry `UNAVAILABLE`.

```sh
grpcurl -plaintext -d '{
  "repository_id": "<repository-id>", "ref": "refs/heads/main",
  "commit_sha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
  "trigger": "PIPELINE_TRIGGER_PUSH", "idempotency_key": "<event-id>:refs/heads/main"
}' localhost:9090 ci.v1.PipelineService/TriggerPipeline
```

```json
{
  "pipeline": {
    "id": "01a187f8-8576-7395-915b-2aff2b0f5d02",
    "repositoryId": "<repository-id>", "ref": "refs/heads/main",
    "commitSha": "9fceb02d0ae598e95dc970b74767f19372d61af8",
    "status": "PIPELINE_STATUS_PENDING", "trigger": "PIPELINE_TRIGGER_PUSH",
    "stages": [
      {"name": "build", "jobs": [{"id": "…", "name": "build", "stage": "build", "status": "JOB_STATUS_PENDING", "image": "golang:1.26", …}]},
      {"name": "test",  "jobs": [{"id": "…", "name": "test",  "stage": "test",  "status": "JOB_STATUS_CREATED", …}]}
    ],
    "createdAt": "2026-10-29T14:02:53.700Z", "updatedAt": "2026-10-29T14:02:53.700Z"
  },
  "created": true
}
```

**Today ci-svc cannot read files from git-svc** (codeplane's `CommitService` is planned for W2–W3, assumption A-03). Until then every call answers `UNAVAILABLE`, except in development, where `GIT2_CISVC_DEV_CONFIG_DIR` serves files from a local directory ([config.md](config.md)).

## GetPipeline and ListPipelines

`GetPipeline` returns one pipeline with all its jobs, grouped by stage in run order, jobs sorted by name.

`ListPipelines` returns a repository's pipelines, **newest first**, each with its jobs. `ref` filters by full ref name (`refs/heads/main`); empty means all refs. `page_size` 0 means 20.

```go
resp, err := pipelines.ListPipelines(ctx, &civ1.ListPipelinesRequest{
	RepositoryId: repoID, Ref: "refs/heads/main", PageSize: 20,
})
for _, p := range resp.GetPipelines() { /* p.GetStatus(), p.GetStages() … */ }
next := resp.GetNextPageToken() // "" on the last page
```

For the ✓/✗ next to a commit or merge request, take the newest pipeline of the commit's ref, or listen to [`PipelineFinished`](events.md).

## CancelPipeline

Cancels every unfinished job. Waiting jobs (`CREATED`, `PENDING`) become `CANCELED` at once. `RUNNING` jobs are stopped by their runner within about 15 seconds and then show `CANCELED`; until then the response still shows them `RUNNING`. Canceling a finished pipeline is not an error and changes nothing.

## StreamJobLog

Sends a job's log as raw bytes from `offset` on. Each message has `offset` (position of its first byte) and `data` (never empty).

- `follow = false`: sends what is stored now, then ends.
- `follow = true`: keeps sending new output, usually under a second after the job prints it, and **ends when the job has finished and every byte was sent**. That end is dash-api's `event: end`.
- To resume after a broken connection, call again with `offset` = last message's `offset` + length of its `data`.
- The bytes are what the job printed: usually UTF-8 with ANSI colour codes (render them, or strip `\x1b[…m`). A message can end in the middle of a UTF-8 character, so decode across messages, not per message.
- Secret values are already `[MASKED]`. A log stops at 4 MiB with a notice line.

```go
stream, err := pipelines.StreamJobLog(ctx, &civ1.StreamJobLogRequest{JobId: jobID, Offset: offset, Follow: true})
for {
	msg, err := stream.Recv()
	if errors.Is(err, io.EOF) { break }        // job finished, everything sent
	if err != nil { /* reconnect from offset */ }
	sse.Send(msg.GetData())
	offset = msg.GetOffset() + int64(len(msg.GetData()))
}
```

Logs are kept in the database for now; when artifacts arrive (W6), finished logs move to the instance bucket. The API does not change.

## ValidateConfig

Checks a `.git2-ci.yml` with the same rules as `TriggerPipeline`, without running anything. For an editor or a "lint" button.

```sh
grpcurl -plaintext -d '{"content": "build:\n  image: alpine\n  scrpt: make\n"}' \
  localhost:9090 ci.v1.PipelineService/ValidateConfig
```

```json
{
  "errors": [
    {"path": "build", "line": 1, "column": 1, "message": "script is required"},
    {"path": "build.scrpt", "line": 3, "column": 3, "message": "unknown key; a job has stage, image, script, variables, tags, timeout, artifacts and cache"}
  ]
}
```

`valid` is true (and `errors` empty) for a valid file. Error messages are safe to show.
