# CI lane API reference

The reference for everything other lanes can see of the CI lane. Where these pages and the code disagree, these pages and the `.proto` files are right and the code has a bug.

| Page | For | Covers |
|---|---|---|
| [overview.md](overview.md) | everyone, read first | Ports, conventions, IDs, errors, pagination, how to call ci-svc |
| [ci-yaml.md](ci-yaml.md) | everyone writing pipelines, Dash (editor) | The `.git2-ci.yml` format and its JSON schema |
| [pipelines.md](pipelines.md) | Dash, codeplane | Triggering, listing and cancelling pipelines; jobs; live logs; validating a file |
| [runners.md](runners.md) | Console, Deploy | Registration tokens, runner list, pause, delete; installing and running `git2-runner` |
| [secrets.md](secrets.md) | Console, Dash | CI secrets: set, list, delete (values are write-only) |
| [events.md](events.md) | Dash, anyone listening | `ci.v1.PipelineCreated`, `ci.v1.PipelineFinished`, `ci.v1.JobFinished` |
| [config.md](config.md) | Deploy | Environment variables, ports, health checks, what the backup must contain |

The contract itself: [`app/proto/ci/v1/`](../../app/proto/ci/v1/). Every service, RPC, message and field there has a comment. Go code generated from it is committed in [`app/gen/go/ci/v1/`](../../app/gen/go/ci/v1/).

**Not available yet:** retrying a job (W4–W5), artifacts and caches (W6–W7), owner-scoped secrets, and reading `.git2-ci.yml` from git-svc (waits for codeplane's `CommitService`, assumption A-03).
