## What and why

<!-- One or two sentences. Link the issue or the assumption (docs/design/assumptions.md) if there is one. -->

## Checklist

- [ ] Tests added or updated, and `cd app && go test ./...` passes
- [ ] Can anyone outside this repo notice this change (proto, event, config, error, limit, behaviour)?
  If yes: `docs/api/` is updated, a version note is added, and its line is at the top of `docs/version_notes/overview.md`
- [ ] Needs a table or column? Migration PR on git2: <!-- link -->
- [ ] Needs work in another lane? Git2 issue linking the version note: <!-- link -->
- [ ] No secrets, tokens, DSNs or `.env` in the diff
