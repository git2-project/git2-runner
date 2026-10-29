# git2-runner

> Runs [git2](https://github.com/git2-project/git2) CI jobs in containers on your build hosts.

**Repos:** [git2](https://github.com/git2-project/git2) · [git2-codeplane](https://github.com/git2-project/git2-codeplane) · [git2-runner](https://github.com/git2-project/git2-runner) · [git2-deploy](https://github.com/git2-project/git2-deploy)

## How it fits

This repo holds both halves of git2's CI. `ci-svc` reads `.git2-ci.yml`, plans the pipeline and queues jobs. A runner (`git2-runner`):

1. Registers with ci-svc (runners are managed from the **Admin** plane).
2. Picks up jobs from the queue.
3. Runs each job's steps inside a container on the build host.
4. Streams logs back and uploads artifacts and caches to the instance bucket.
5. Reports the result, which ends up as a `pipeline.finished` event.

Run as many runners as you have build hosts; they only need network access to ci-svc. The design and its decisions are in [docs/design/plan.md](docs/design/plan.md) and [docs/decisions/](docs/decisions/).

## What lives here

| Path | What it is |
| :--- | :--- |
| `app/` | The Go module (`github.com/git2-project/git2-runner/app`): `ci-svc`, `git2-runner`, the `ci.v1` contract in `app/proto/ci/v1/` |
| `migrations/` | A copy of our schema migration; the original lives in the git2 repo |
| `docs/` | Docs for the other git2 lanes: [API reference](docs/api/), [version notes](docs/version_notes/overview.md), [plan and assumptions](docs/design/) |
| `.devcontainer/` | Go 1.26, psql, a test PostgreSQL, a Redis standing in for the shared one, and a Docker-in-Docker daemon where job containers run |

## Status

Early development, on the 9-week plan in the [org README](https://github.com/git2-project). Since 29 Oct 2026, ci-svc and the runner run a pipeline end to end in development: register a runner, store a secret, trigger, watch the jobs run stage by stage in containers, stream the log. Not yet: reading `.git2-ci.yml` from git-svc (pushes), source checkout, retry, artifacts. What's new is in [docs/version_notes/overview.md](docs/version_notes/overview.md).

## Development

Open the repo in the devcontainer (VS Code → *Reopen in Container*). Job containers run on its own Docker daemon (`DOCKER_HOST=tcp://docker:2375`), never on your host's Docker. The database is the team's shared Supabase project (dev only); its schema comes from the git2 repo's migrations.

```bash
cp env.example .env    # POSTGRES_DSN, and GIT2_CI_SECRETS_KEY from `openssl rand -base64 32`
cd app
make tests             # unit and Postgres tests
make tests-docker      # plus the tests that start job containers
make run               # ci-svc on :9090 (internal) and :9091 (runners)
```

All commands are in [CLAUDE.md](CLAUDE.md#development).

## Contributing

- Work stays inside this repo's scope. Changes to shared contracts go through a PR approved by every affected owner.
- Nobody pushes to `main`: every change arrives as a pull request.
- Every PR ships with tests and needs one human review before merge.
- A change that other lanes can notice ships with a doc: [docs/api/](docs/api/) is updated and a [version note](docs/version_notes/overview.md) is added.
- Database tables are created only by migrations in the [git2](https://github.com/git2-project/git2) repo.
