# 0002. ci-svc and the runner are two programs

- Status: Accepted
- Date: 2026-10-26

## Context

Build hosts run other people's code (risk R-05). Runners must work on a dedicated server, a VM or Kubernetes, and need only network access to the instance. CI secrets are stored in the database.

## Decision

- **ci-svc** (`app/cmd/ci-svc`) is the control plane.
  - It is the only program in the lane with the database DSN, the Redis address, the secrets key and bucket credentials.
  - It keeps no state of its own and runs as one or more replicas.
  - It serves two ports: an internal one for git2's services, and a runner port with TLS.
- **The runner** (`app/cmd/git2-runner`) runs on build hosts.
  - It dials out to the runner port and authenticates with its runner token.
  - It receives only what one job needs, while that job runs.
  - It runs jobs through an executor interface: `docker` first, `kubernetes` later.

## Consequences

- A compromised build host can leak only the secrets of the jobs it ran, never the whole database.
- The database and Redis never have to be reachable from build hosts.
- Database code is not linked into the runner binary.
- Logs and artifacts travel through ci-svc. Signed bucket URLs can take that load off later.
