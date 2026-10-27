# git2-runner docs

Docs for the git2 lanes that build on the CI lane: Dash, the management console, codeplane and deploy. Each doc is meant to be enough on its own; you shouldn't need to read our code.

| Start here | |
|---|---|
| [version_notes/overview.md](version_notes/overview.md) | What changed, newest first, and what your lane has to do about it |
| [api/](api/) | Reference for our gRPC API, events, `.git2-ci.yml` and config. Where the docs and the code disagree, this is the source of truth |
| [design/plan.md](design/plan.md) | How the CI lane is built and what is still open (draft) |
| [design/assumptions.md](design/assumptions.md) | What we assume about your repo. Please confirm or correct your rows |

When an update needs work in your lane, Osman opens an issue on the git2 repo that links the version note.
