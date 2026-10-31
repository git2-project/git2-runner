# ci-svc: how to call it

ci-svc is the CI service of git2. It turns a push into a pipeline of jobs, queues the jobs for runners on build hosts, and keeps their status, logs and secrets. It speaks gRPC only; REST for browsers lives at git2's gateway and in dash-api and admin-api.

## Two ports

| Port | Default | Services | Who calls it | Auth |
|---|---|---|---|---|
| Internal | `:9090` | `ci.v1.PipelineService`, `ci.v1.RunnerAdminService`, `ci.v1.SecretService` | dash-api, admin-api, and whatever delivers push events (to be agreed, WBS-1.3). Reachable only inside the instance network | None. Like git-svc, ci-svc trusts its callers: **check the user's permissions before you call** |
| Runner | `:9091` | `ci.v1.RunnerService` | `git2-runner` on build hosts | A runner token in `authorization: Bearer <token>`. TLS in production |

Both ports serve the standard gRPC health service (`grpc.health.v1.Health`). With `GIT2_CISVC_DEV=true`, the internal port also serves gRPC reflection, which `grpcurl` needs.

## Conventions

The same as git-svc's `git.v1` (codeplane ADR 0008), so all git2 APIs look alike.

- **Every RPC has its own request and response message.** Read fields from the response (`resp.GetPipeline()`); new fields will be added there without breaking you.
- **IDs are UUID strings** in canonical form (`3f2b8c1e-1d6a-4b5c-9e8f-0a1b2c3d4e5f`). A malformed ID is `INVALID_ARGUMENT`, never `NOT_FOUND`. Pipeline, job and runner IDs are UUIDv7, so they sort by creation time.
- **Repository and user IDs** are the ones git-svc and auth-svc issue. ci-svc stores IDs only, never names: turn them into names yourself for display.
- **Timestamps** are `google.protobuf.Timestamp`. An unset timestamp (`started_at` before a job started) means "not yet".
- **Enums** always have an `_UNSPECIFIED` zero value that ci-svc never returns. Treat an unknown value as "unknown", not as an error: values may be added.
- **Lists** paginate per Google AIP-158: `page_size` 0 means the page's default, values above 100 are treated as 100, negative values are `INVALID_ARGUMENT`. Follow `next_page_token` until it is empty. Tokens are opaque and only valid with the same filter.

## Errors

| Code | Meaning | What to do |
|---|---|---|
| `INVALID_ARGUMENT` | A field breaks a rule. The message names the field (`ref: must be a full branch or tag name …`) | Fix the request. Safe to show the message to an admin |
| `NOT_FOUND` | The pipeline, job, runner or secret does not exist | Show "not found". For a repository the user cannot read, dash-api should answer 404 too |
| `ALREADY_EXISTS` | An idempotency key was reused for different data | A bug in the caller |
| `FAILED_PRECONDITION` | The thing is in a state that forbids the call (a finished job) | Do not retry |
| `RESOURCE_EXHAUSTED` | Rate limit (runner registration) | Wait a minute |
| `UNAVAILABLE` | ci-svc or something it needs (database, git-svc) is down | Retry with backoff |
| `UNAUTHENTICATED` | Runner port only: missing, unknown or deleted runner token | The runner must register again |
| `INTERNAL` | A bug or fault in ci-svc. The message is always just `internal error`; details are in ci-svc's log | Retry later; report it |

## Calling it

From Go, use the committed generated code:

```sh
go get github.com/git2-project/git2-runner/app@<tag or commit>
```

```go
import civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"

conn, err := grpc.NewClient("ci-svc:9090", grpc.WithTransportCredentials(insecure.NewCredentials()))
pipelines := civ1.NewPipelineServiceClient(conn)
resp, err := pipelines.GetPipeline(ctx, &civ1.GetPipelineRequest{Id: pipelineID})
if status.Code(err) == codes.NotFound { /* 404 */ }
```

From a shell, with ci-svc running in dev mode:

```sh
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext -d '{"id":"<pipeline-id>"}' localhost:9090 ci.v1.PipelineService/GetPipeline
```

`grpcurl` is pinned in this repo's `app/tools.mod`: `go tool -modfile=tools.mod grpcurl …` from `app/`.
