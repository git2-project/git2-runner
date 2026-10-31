# 0007. CI secrets live in ci-svc

- Status: Accepted
- Date: 2026-10-29

## Context

Jobs need secrets (deploy keys, registry tokens). Someone must store them, and someone must hand them to a job. The options were a general secret store in git2 (auth-svc or a new service) that ci-svc calls, or ci-svc storing them itself.

The only reader of a CI secret is ci-svc, at the moment it leases a job to a runner. A separate store would add a network hop and a second service that must be up for any job to start, and it would still need the same encryption, key handling and backup rules.

## Decision

- **ci-svc owns CI secrets**: the `ci.v1.SecretService` API on the internal port and the `ci_svc.secrets` table. admin-api calls it for instance secrets, dash-api for repository secrets. Both check the user's permissions first (A-10).
- **Scopes in v0:** instance and repository. A job gets both; the repository's value wins on equal names. Owner (user or organization) scope comes later; it needs ci-svc to know a repository's owner, which needs git-svc.
- **Write-only.** `SetSecret` takes a value; nothing returns one. `ListSecrets` returns names and dates.
- **At rest.** AES-256-GCM with a random nonce per value. The additional data is scope, scope ID and name, so a ciphertext copied into another row does not decrypt. The 32-byte key is `GIT2_CI_SECRETS_KEY`, from configuration, never from the database. Each row stores the ID of the key that encrypted it.
- **Restore check.** At startup, ci-svc refuses to start if any stored secret was encrypted with another key, and says so. A restore that lost the key fails loudly instead of failing every job.
- **To the runner.** Decrypted values are added to the job when it is leased, marked `masked`, and sent only to that runner over the runner port (TLS in production). The runner passes them to the container through the Docker API (no file, no command line) and replaces them by `[MASKED]` in the log before sending it. Values shorter than 4 bytes are not masked; each line of a multi-line value is masked too.
- **Names** are upper-case environment variable names. `CI_` and `GIT2_` are reserved for the variables git2 sets.
- **Runner tokens** follow the same idea: random 256-bit tokens with a readable prefix (`g2rt_` registration, `g2r_` runner), stored only as SHA-256. `RegisterRunner` is rate-limited per client address through Redis (`ci:rl:register:<ip>`, 10 a minute, open when Redis is down).

## Consequences

- The secrets key must be in every backup, next to the database dump (A-05). Without it, every CI secret is lost.
- A job can still print a secret in a transformed form (base64, reversed). Masking catches mistakes, not malice; the defence against malicious jobs is that a build host only ever sees the secrets of the jobs it runs (ADR 0002).
- Key rotation is possible later (decrypt with the old key ID, re-encrypt), but not built.
