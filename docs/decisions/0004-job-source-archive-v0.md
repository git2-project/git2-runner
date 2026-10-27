# 0004. In v0, a job gets its source as an archive through ci-svc

- Status: Accepted
- Date: 2026-10-26

## Context

A job needs the repository at the pushed commit. There are two ways to get it there:

1. The runner clones over HTTPS with a short-lived job token. That needs support from the gateway, auth-svc and codeplane.
2. ci-svc fetches the commit as a tar archive (`git archive`) from codeplane over gRPC and streams it to the runner.

## Decision

v0 uses option 2. The runner unpacks the archive into the job's volume. Option 1 follows once job tokens exist.

## Consequences

- Runners never talk to codeplane. We need one RPC from it (assumption A-04).
- v0 workspaces have no `.git` directory, so jobs that need history (`git describe`, `git log`) don't work yet. `docs/api/ci-yaml.md` must say so.
- Archives stream through ci-svc and are size-capped. That is fine at the demo's scale.
