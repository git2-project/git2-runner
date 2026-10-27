# docs/ — notes for Claude

Everything in `docs/` is written for **people outside this repo**: the other lane owners and the coding agents they hand our docs to. They have not seen our code, and a doc here is the spec they build from, so it has to work without the code.

| Lane | Owner | Usually needs from us |
|---|---|---|
| Dash | Bilge | pipelines, jobs, live logs, artifacts, status on merge requests |
| Management console | Beste | runners (register, pause, delete), CI secrets, instance CI settings |
| Codeplane | Apo | how a push triggers a pipeline, what we read from repositories |
| Deploy, backup | Ceyda | config, ports, images, Helm values, what is in the backup and how restore behaves |

## What goes where

| Path | What | Source of truth? |
|---|---|---|
| `api/` | Reference for our gRPC API, events, `.git2-ci.yml` and config. One file per area (`pipelines.md`, `runners.md`, `secrets.md`, `events.md`, `ci-yaml.md`, `config.md`) | Yes, together with the `.proto` files |
| `version_notes/<year>/<month>/<day>_<month>.md` | What changed on one day and what each lane must do about it | No. It summarizes a change and links to `api/` |
| `version_notes/overview.md` | One line per note, newest first | Index |
| `design/` | Plan, assumptions, proposals | For us. Others may read it but never build from it |
| `decisions/` | ADRs | For us |

## When a change needs docs

A change needs docs when anyone outside this repo can notice it: a proto change (service, RPC, message, field, error code), an event, a config variable or default, a limit, behaviour another service relies on, or a deploy requirement.

In the same PR:

1. Update `api/` so it describes the new state. History belongs in the version note.
2. Add today's version note, or extend it if it already exists.
3. Add a line at the top of the month's table in `version_notes/overview.md`, creating the month's section if needed.
4. If a lane must build something (a UI screen, a migration, a deploy change), write the action items in the note so the issue can link to them. Osman opens the git2 issue; don't open it unless asked.

## Version notes

Path: `version_notes/<year>/<month>/<day>_<month>.md`, for example `version_notes/2026/10/25_10.md`. One note per day. A second change on the same day goes into the same note.

```markdown
# Runner version notes — 2026-10-25

**Audience:** <lanes this is for> and their coding agents.
**Scope:** <one or two sentences: what changed>

**Canonical reference:** [docs/api/<file>.md → <section>](../../../api/<file>.md#<anchor>).
The API docs are the source of truth; this note only summarizes the change.

**Breaking?** <No / Yes: what stops working, and for whom>

---

## 🟢 NEW — <thing>          (or 🟡 CHANGED, 🔴 REMOVED, ℹ️ FYI)

<What it is, then an example: a grpcurl call, a Go snippet or a YAML snippet.
A table of error codes and when each happens. Before/after tables for changes.>

## Action items

### Dash (Bilge)
- [ ] <concrete, checkable step>

### Console (Beste)
- [ ] …
```

Only list the lanes that have something to do.

## Overview tags

| Tag | Meaning |
|---|---|
| 🔴 Breaking | Something a lane already built stops working until they change it. |
| 🟠 Action | Nothing breaks, but a lane must build or change something. |
| 🟢 New | New RPC, field, event or option. Use it when you're ready. |
| ⚪ FYI | Inside the runner only. Nothing to do. |

## Style

- Plain English and short sentences. Say what the reader must do, not how we built it.
- Be concrete: real RPC, field and enum names, an example call, and when each error code happens.
- Name the lane each action item is for.
- Never paste secrets, DSNs, tokens or internal hostnames, even in examples. Use `<placeholder>`.
- Links are relative, so they work on GitHub and in a local checkout.
