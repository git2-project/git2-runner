# `.git2-ci.yml` v0

A repository's pipeline is described by `.git2-ci.yml` at the root of the repository. When a branch or tag is pushed, ci-svc reads the file at the pushed commit and runs it. A commit without the file starts no pipeline.

v0 is a small subset of GitLab CI, so people who know GitLab can read it. Everything not listed here is rejected with a clear error, so nothing is silently ignored.

**JSON schema:** [`app/internal/ciyaml/git2-ci.schema.json`](../../app/internal/ciyaml/git2-ci.schema.json) (draft 2020-12). Editors can use it for completion and checks; the YAML extension of VS Code takes `# yaml-language-server: $schema=<url of the file>` as the first line. The schema cannot check everything: ci-svc also checks that a job's stage is in `stages`, that durations are within their limits, and that the file has at least one job. To check a file exactly as ci-svc does, call [`ValidateConfig`](pipelines.md#validateconfig).

## Example

```yaml
stages: [build, test, deploy]

variables:
  CGO_ENABLED: "0"

.go: &go             # a template: keys starting with "." are not jobs
  image: golang:1.26
  tags: [linux]

build:
  <<: *go
  stage: build
  script:
    - go build -o bin/ ./...
  artifacts:
    paths: [bin/]
    expire_in: 7d

test:
  <<: *go
  stage: test
  timeout: 10m
  variables:
    GOFLAGS: -count=1
  script:
    - go vet ./...
    - go test ./...

lint:
  stage: test
  image: golangci/golangci-lint:v2.5
  script: golangci-lint run

deploy:
  stage: deploy
  image: alpine:3.20
  script:
    - ./deploy.sh "$DEPLOY_TOKEN"   # a CI secret, masked in the log
```

`build` runs first. `test` and `lint` run in parallel once `build` succeeded. `deploy` runs once both succeeded. If any job fails, later stages are skipped.

## Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `stages` | list of names | `[build, test, deploy]` | The stages, in the order they run. 1 to 50 unique names |
| `variables` | map | none | Environment variables for every job |
| `.<anything>` | anything | | A template for YAML anchors (`&name`, `*name`, `<<:`). Not a job |
| any other key | job | | A job; the key is its name |

**Names** of jobs and stages: 1 to 63 characters from `A-Z a-z 0-9 _ . -`, starting with a letter or digit. A file has 1 to 200 jobs and is at most 256 KiB.

These GitLab keys are rejected as "not supported in v0": `default`, `include`, `workflow`, `image`, `services`, `before_script`, `after_script`, `cache` (at the top level), `pages`.

## Job keys

| Key | Required | Type | Default | Meaning |
|---|---|---|---|---|
| `image` | yes | string | | Container image, e.g. `golang:1.26`. No spaces, at most 255 characters. Must contain `/bin/sh` |
| `script` | yes | string or list of strings | | Shell commands, run in order by `/bin/sh` in the workspace. Each command is printed before it runs; the job fails at the first command that exits non-zero |
| `stage` | no | name | `test` | Must be one of `stages` |
| `variables` | no | map | | Environment variables for this job; they override the file's `variables` |
| `tags` | no | list | none | The job runs only on a runner that has every one of these labels. Up to 20 unique tags, each 1 to 63 characters from `A-Z a-z 0-9 _ . -`. Without tags, any runner can take the job |
| `timeout` | no | duration | `1h` | The job is stopped and fails with reason `TIMEOUT` after this long. `10s` to `1d` |
| `artifacts.paths` | with `artifacts` | list of paths | | Files to keep after the job. **Accepted, not stored yet (W6–W7)** |
| `artifacts.expire_in` | no | duration | `30d` | How long to keep them. `1h` to `365d` |
| `cache.paths` | with `cache` | list of paths | | Files to keep between runs. **Accepted, not used yet (WBS-10.6)** |
| `cache.key` | no | string | | Separates caches. Same rule as tags |

**Durations:** whole days, hours, minutes and seconds in that order, no spaces: `90s`, `10m`, `1h30m`, `7d`.

**Paths:** relative to the workspace, at most 255 characters, not absolute and not leaving the workspace (`../x` is rejected). Glob patterns are allowed. Up to 50 per list.

These GitLab job keys are rejected as "not supported in v0": `rules`, `only`, `except`, `needs`, `services`, `before_script`, `after_script`, `when`, `allow_failure`, `parallel`, `extends` (use `<<:` instead), `environment`, `retry`, `dependencies`, `coverage`, `interruptible`, `trigger`, `inherit`, `release`, `resource_group`, `secrets`, `id_tokens`, `hooks`.

## Variables

Names: 1 to 100 characters from `A-Z a-z 0-9 _`, not starting with a digit. Names starting with `CI_` or `GIT2_` are reserved. Values are strings; numbers and booleans are turned into their text (`3`, `true`). At most 32 KiB each.

A job's environment is built in this order, a later one winning on equal names:

1. **Predefined** by git2:

   | Variable | Example |
   |---|---|
   | `CI`, `GIT2_CI` | `true` |
   | `CI_PIPELINE_ID`, `CI_JOB_ID` | UUIDs |
   | `CI_JOB_NAME`, `CI_JOB_STAGE` | `test`, `test` |
   | `CI_REPOSITORY_ID` | the git-svc repository ID |
   | `CI_COMMIT_SHA` | `9fceb02d0ae598e95dc970b74767f19372d61af8` |
   | `CI_COMMIT_REF` | `refs/heads/main` |
   | `CI_COMMIT_REF_NAME` | `main` |
   | `CI_COMMIT_BRANCH` or `CI_COMMIT_TAG` | `main` (only the one that applies) |

2. The file's `variables`.
3. The job's `variables`.
4. **CI secrets** of the instance and of the repository ([secrets.md](secrets.md)). Their values are replaced by `[MASKED]` in the log.

## The workspace

v0 jobs start in an **empty** `/workspace` volume: the repository's source is not checked out yet. That arrives in W4–W5, when codeplane can hand ci-svc an archive of the commit (assumption A-04). Until then, jobs can only run commands that do not need the source.

Each job runs in its own container, network and volume, all removed when it ends. Jobs are never privileged and never see the Docker socket.

## Errors

An invalid file still creates a pipeline when pushed: it has no jobs, status `FAILED`, and `error_message` lists the problems with their line numbers, for example:

```
.git2-ci.yml is invalid: line 4: build.scrpt: unknown key; a job has stage, image, script, variables, tags, timeout, artifacts and cache; line 7: test.stage: stage "check" is not in stages
```

Every problem in the file is reported at once (up to 10 in `error_message`; `ValidateConfig` returns all of them, each with path, line and column).
