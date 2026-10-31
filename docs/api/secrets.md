# CI secrets: `ci.v1.SecretService`

Values that jobs get as environment variables, such as deploy keys and registry tokens. On ci-svc's **internal port**. Proto: [`secret.proto`](../../app/proto/ci/v1/secret.proto).

- **Console (Beste)**, through admin-api: instance secrets, for instance admins.
- **Dash (Bilge)**, through dash-api: repository secrets, for users who may change the repository's settings.

ci-svc does not check permissions; the caller does, before calling.

## How secrets behave

- **Write-only.** `SetSecret` takes a value. No RPC ever returns one: `ListSecrets` returns names and dates only. The UI shows a list of names with "update" and "delete", never a value.
- **Scopes:** `SECRET_SCOPE_INSTANCE` (every job of the instance; `scope_id` empty) and `SECRET_SCOPE_REPOSITORY` (every job of one repository; `scope_id` = repository ID). When both have the same name, the repository's value wins. Owner-level (user or organization) secrets come later.
- **Names** are environment variable names: 1–100 characters from `A-Z 0-9 _`, not starting with a digit, and not starting with `CI_` or `GIT2_`, which git2 reserves. Upper case only.
- **Values:** 1 byte to 32 KiB of UTF-8, without NUL. Multi-line values (SSH keys) work.
- **In the log** a secret's value is replaced by `[MASKED]`, and so is each line of a multi-line value. Values shorter than 4 bytes are not masked. Masking catches accidents; a job that wants to leak a secret can (by encoding it), so only give secrets to repositories you trust.
- **Changes** apply to jobs that start afterwards. A running job keeps the value it got.
- **At rest** values are encrypted (AES-256-GCM) with a key that is not in the database. See [config.md](config.md#backup-and-restore) for what the backup must contain.

## RPCs

| RPC | Input → output | Errors |
|---|---|---|
| `SetSecret` | `scope`, `scope_id`, `name`, `value` → `secret` (metadata), `created` (false if a value was replaced) | `INVALID_ARGUMENT` |
| `ListSecrets` | `scope`, `scope_id`, `page_size` (0 = 50), `page_token` → `secrets` sorted by name, `next_page_token` | `INVALID_ARGUMENT` |
| `DeleteSecret` | `scope`, `scope_id`, `name` → empty | `INVALID_ARGUMENT`, `NOT_FOUND` |

`Secret`: `scope`, `scope_id` (empty for the instance), `name`, `created_at`, `updated_at`.

`INVALID_ARGUMENT` messages name the field and the rule (`name: names starting with "CI_" or "GIT2_" are reserved for git2`) and are safe to show.

```sh
grpcurl -plaintext -d '{
  "scope": "SECRET_SCOPE_REPOSITORY", "scope_id": "<repository-id>",
  "name": "DEPLOY_TOKEN", "value": "<value>"
}' localhost:9090 ci.v1.SecretService/SetSecret
```

```json
{
  "secret": {"scope": "SECRET_SCOPE_REPOSITORY", "scopeId": "<repository-id>", "name": "DEPLOY_TOKEN",
             "createdAt": "2026-10-29T14:02:53Z", "updatedAt": "2026-10-29T14:02:53Z"},
  "created": true
}
```

```go
_, err := secrets.SetSecret(ctx, &civ1.SetSecretRequest{
	Scope: civ1.SecretScope_SECRET_SCOPE_INSTANCE, Name: "REGISTRY_TOKEN", Value: value,
})
```

When a repository is deleted, its secrets stay until a cleanup lands (it will listen to `git.v1.RepositoryDeleted`).
