# git2-runner

> Runs [git2](https://github.com/git2-project/git2) CI jobs in containers on your build hosts.

**Repos:** [git2](https://github.com/git2-project/git2) · [git2-codeplane](https://github.com/git2-project/git2-codeplane) · [git2-runner](https://github.com/git2-project/git2-runner) · [git2-deploy](https://github.com/git2-project/git2-deploy)

## How it fits

`ci-svc` in [git2](https://github.com/git2-project/git2) reads `.git2-ci.yml`, plans the pipeline and queues jobs. A runner:

1. Registers with the instance (runners are managed from the **Admin** plane).
2. Picks up jobs from the queue.
3. Runs each job's steps inside a container on the build host.
4. Streams logs back and uploads artifacts and caches to the instance bucket.
5. Reports the result, which ends up as a `pipeline.finished` event.

Run as many runners as you have build hosts; they only need network access to the git2 instance.

## Status

Early development, on the 9-week plan in the [org README](https://github.com/git2-project). Nothing here is ready to run yet; build and test commands will land in this file and in `CLAUDE.md` as the code does.

## Contributing

- Work stays inside this repo's scope. Changes to shared contracts go through a PR approved by every affected owner.
- Every PR ships with tests and needs one human review before merge.
