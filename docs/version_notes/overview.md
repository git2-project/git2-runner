# Version notes overview

One line per CI-lane change that touches another lane, **newest first**.
Open the linked note for the details, and use this page to check what your
lane has and hasn't done yet.

| Tag | Meaning |
|---|---|
| 🔴 Breaking | Something your lane already built stops working until you change it. |
| 🟠 Action | Nothing breaks, but your lane must build or change something. |
| 🟢 New | New RPC, field, event or option. Use it when you're ready. |
| ⚪ FYI | Inside the CI lane only. Nothing to do. |

**Affects** names the lanes with something to read or do: Dash (Bilge),
Console (Beste), Codeplane (Apo), Deploy (Ceyda).

Notes live in `<year>/<month>/<day>_<month>.md`. When you add one, put it in
the right month folder and add a line at the top of that month's table below.

---

## 2026 · October

| Date | What changed | Affects | Tag |
|---|---|---|---|
| [26 Oct](2026/10/26_10.md) | Decided: ci-svc lives in git2-runner, and one Redis is shared by all services. The CI lane keeps its state in Postgres and uses Redis only for signals, with keys starting `ci:`. Deploy adds the shared Redis; git2's README should point `ci-svc` here; everyone prefixes their Redis keys. | All | 🟠 Action |
| [25 Oct](2026/10/25_10.md) | git2-runner repo opened. ci-svc (pipelines, job queue, logs, artifacts, CI secrets) is planned to live here next to the runner. Nothing to build yet, but please check the assumptions about your repo. | All | ⚪ FYI |
