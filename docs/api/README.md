# CI lane API reference

The `ci.v1` gRPC contract and its docs land here in week 1 (by 1 Nov 2026). Until then, the draft is in [design/plan.md §3](../design/plan.md#3-proposed-architecture).

Planned pages:

| Page | For | Covers |
|---|---|---|
| `ci-yaml.md` | everyone writing pipelines | The `.git2-ci.yml` format and its JSON schema |
| `pipelines.md` | Dash, codeplane | Triggering, listing, cancelling and retrying pipelines; job logs; artifacts |
| `runners.md` | Console | Registration tokens, runner list, pause, delete |
| `secrets.md` | Console, Dash | CI secrets: set, list, delete (values are write-only) |
| `events.md` | Dash, anyone listening | `ci.v1.PipelineCreated`, `ci.v1.PipelineFinished`, `ci.v1.JobFinished` |
| `config.md` | Deploy | Environment variables, ports, health checks, what the backup must contain |
