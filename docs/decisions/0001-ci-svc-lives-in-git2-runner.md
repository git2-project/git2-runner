# 0001. ci-svc lives in git2-runner

- Status: Accepted
- Date: 2026-10-26

## Context

The kickoff deck, the charter, the project plan and git2's README place `ci-svc` (reads `.git2-ci.yml`, plans pipelines, queues jobs, keeps logs and artifacts) in the git2 repo, owned by Osman. The team map has no ci-svc and makes Bilge and Beste the people committing to git2. Osman owns both ci-svc and the runner, and the protocol between them changes with both.

## Decision

- ci-svc is built in this repo and keeps its name.
- Its schema is `ci_svc`, and its migrations are still written in git2 (`migrations/NNN_ci_<what>.sql`).
- Its gRPC contract is package `ci.v1` in `app/proto/ci/v1/`. As in git2-codeplane's ADR 0004, this repo is the source of truth, and git2's `contracts/ci/README.md` holds a pointer to a pinned tag.
- git2 gets no `ci-svc/` directory.

## Consequences

- The engine and the runner protocol change together, in one PR.
- git2's README table needs a PR (by Bilge or Beste) that points `ci-svc` here.
- dash-api and admin-api reach pipelines, runners and CI secrets over gRPC.
- The all-in-one image has to start ci-svc too (assumption A-08).
- The course reports describe ci-svc as part of git2-runner from the Requirement Specification on.
