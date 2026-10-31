# 0009. Schema copy, tooling and tests, aligned with codeplane

- Status: Accepted
- Date: 2026-10-29

## Context

Migrations live only in the git2 repo, but our Postgres tests need the schema. git2-codeplane solved the same problem (its ADRs 0006, 0010 and 0011), and the lanes agreed that all git2 services look alike.

## Decision

- **Schema.** The source of truth is `migrations/003_ci_pipelines.sql` in the git2 repo (schema `ci_svc`). This repo keeps a byte-identical copy in `migrations/` at the repo root. Only the tests and the devcontainer's test database read it. When the git2 file changes (for example it is renumbered before merge), the copy changes with it. ci-svc never changes the schema; at startup it checks that its tables exist.
- **Tests.** Postgres tests live in `*_integration_test.go` and skip unless `TEST_POSTGRES_DSN` is set. They apply the copy (idempotent), run once in pgx's default mode and once in exec mode (what runs behind Supabase's transaction pooler), and delete their own rows. Tests that need Docker carry `//go:build integration` (`make tests-docker`).
- **Devcontainer.** Adds a throwaway `postgres:17-alpine` that loads `migrations/` on first start, and pins Go to 1.26.10.
- **Tooling.** `app/tools.mod` pins buf, the protoc plugins, golangci-lint and grpcurl at the same versions as codeplane. `make lint`, `make proto`, `make tests` work as in codeplane. Generated code in `app/gen/go/` is committed.

## Consequences

- A schema change needs a git2 PR and the copy here in the same week.
- Until the git2 PR is merged and applied, the shared dev DB has no `ci_svc` tables; use the devcontainer's test database meanwhile.
- No CI workflow runs the tests yet; the PR checklist asks for `make tests` in the devcontainer.
