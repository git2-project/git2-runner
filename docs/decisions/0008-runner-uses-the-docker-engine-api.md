# 0008. The runner drives Docker through the Engine API

- Status: Accepted
- Date: 2026-10-29

## Context

The runner starts one container per job. It can shell out to the `docker` CLI or call the Docker Engine API directly. A job's environment includes secrets. With the CLI, they would go on the command line (visible in `ps`), in an `--env-file` (on disk, and unable to hold multi-line values such as SSH keys), or in the CLI's own environment (where a job variable named `DOCKER_HOST` would redirect the runner to another daemon).

## Decision

- The `docker` executor uses the Engine API through `github.com/moby/moby/client`, the client for Docker 29. The daemon comes from `DOCKER_HOST` or the local socket; the client negotiates the API version.
- Per job: a container `git2-job-<job_id>`, a bridge network and a workspace volume of the same name, mounted at `/workspace`. All three are removed when the job ends, whatever the outcome.
- The script runs as `/bin/sh -c` with `set -e`, and each command is echoed before it runs. Images need `/bin/sh`.
- Isolation (R-05): not privileged, no Docker socket inside, `no-new-privileges`, an init process, and memory, CPU and process limits from the runner's config (default 2 GiB, 2 CPUs, 1024 processes). Nothing on the runner host is bind-mounted, which also makes a remote daemon (Docker-in-Docker, a VM) work.
- Image pull failures are reported as `IMAGE_PULL_FAILURE`, daemon failures as `RUNNER_SYSTEM_FAILURE`.
- One job at a time per runner process in v0. Run more processes for more parallelism.
- The runner token lives in the runner's config file, written with permissions 0600. `git2-runner register` refuses to overwrite it unless `-force` is given.

## Consequences

- The runner binary links the Docker client but no database or Redis code (checked with `go list -deps`).
- Leftovers after a runner crash (`io.git2.job-id` labels) are not cleaned automatically yet.
- A `kubernetes` executor (a pod per job) can implement the same `Executor` interface later.
