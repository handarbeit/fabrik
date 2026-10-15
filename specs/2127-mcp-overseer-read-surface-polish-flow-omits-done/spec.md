## Problem

These are smaller defects in the MCP read surface. Each one sends an overseer back to `gh` or misleads it. All were observed in an in-situ test against the live dev daemon on 2026-10-11.

- **F3 — The flow view drops Done silently.** It reported `items_scanned: 88`, but the columns sum to 48 (Specify 11, Implement 1, Validate 2, Backlog 34). The 40 Done items are omitted without saying so. The same omission skews filtered counts, e.g. `milestone=0.1.0`.
- **F6 — `fabrik_status` hides non-Fabrik labels.** For #2112 (labelled `bug`) it showed `stage_labels: []` and `fabrik_labels: []`, so `bug` was nowhere. Yet `fabrik_board filter.label=bug` finds it: the daemon has the label and status does not show it. The same response also has:
  - `url: "unknown"`, although it is derivable from the repo and number;
  - seven `Backlog:*` cycle limits that mean nothing for a parking column.
- **F8 — A filtered attention view with no matches drops the key.** `view=attention filter={column:Specify, has_open_blockers:true}` returned no `attention` field at all rather than `attention: []`. Four Specify items do have open blockers. Nothing says how many items matched the filter but were not attention-worthy, so "nothing" reads as "no blocked items".
- **F12 — `last_invocation` mixes per-call and cumulative figures.** For #2096 it reported `duration_seconds: 13.08` and `turns_used: 1` beside `output: 25691`, `cache_read: 11491047` and `cost_usd: 2.98`. That is consistent with token totals accumulated over a resumed session next to per-call duration and turns. The engine log shows the same mix, so MCP is faithful, but unlabelled it misleads anyone judging waste.
- **F13 — `validate-settled` does not say whether the review found anything.** `reviews=handarbeit-pruefer[bot]: COMMENTED` is identical for a clean pass and for findings, because Pruefer always submits COMMENTED. The overseer had to open the review to make the merge decision.
- **F14 — `validate-settled` hides a flake re-run behind `ci=green`.** On #2112, `validate-settled` said `ci=green`. That green came from the flake re-run (#2072, R1) of a failed "Test and vet" on the same head, and nothing in the event said so.
- **F15 — Pushes do not stand on their own.** An item-scoped push carries no issue title, so a reacting session needs a `fabrik_status` round trip to learn what the item is.

## Requirements

- **R1 (F3).** The flow view either lists Done (count only) or states `excluded: {Done: N}`, and the column counts reconcile with `items_scanned`. Apply the same rule to filtered views.
- **R2 (F6).**
  - `fabrik_status` reports every label on the item; keep the stage/fabrik groupings and add `other_labels`.
  - `url` is derived from the repo and number when not cached.
  - Cycle limits are omitted for unmanaged and holding columns.
- **R3 (F8).** An attention view always includes `attention` (possibly `[]`) and `matched: N`, the number of items that passed the filter.
- **R4 (F12).** `last_invocation` labels each figure as per-invocation or cumulative-for-session, or reports both where the engine has them. Do not change the engine's accounting.
- **R5 (F13).** `validate-settled` (and `fabrik_status`'s review summary) carries, per reviewer, evaluated at the PR **head**:
  - whether the latest review is at the head SHA;
  - the number of inline comments on it at the head;
  - the number of findings outstanding at the current head (unresolved review threads plus actionable review bodies), and a clean-pass flag that is true only when the reviewer's latest review is at the head and no findings are outstanding. A pass on an older head is never reported as a clean pass;
  - whether it has unaddressed findings (using the same predicate as the feedback gate);
  - the PR URL.
- **R6.** Docs: the `docs/USER_GUIDE.md` MCP section, and regenerate `docs/llms-full.txt`.
- **R7 (F14).** When the CI verdict on a `validate-settled` event is green and follows a flake re-run (#2072, R1) on that same head, the event carries a `ci_rerun` attribute naming the failed check that was re-run. The attribute is absent when no re-run occurred on that head, so an overseer can decide whether to look before landing.
- **R8 (F15).** Every item-scoped push carries the issue `title`, so a reacting session does not need a `fabrik_status` round trip to know what the item is.

## Scope

In scope: the rendering and attribute fixes above (F3, F6, F8, F12, F13, F14, F15).

Out of scope: classifier semantics (separate issues).

## Acceptance

- **One test per requirement**, each shown non-vacuous by neutralisation:
  - flow counts reconcile;
  - `bug` appears in `fabrik_status`;
  - an empty filtered attention has `attention: []` and `matched`;
  - `last_invocation` figures are labelled;
  - `validate-settled` distinguishes a clean COMMENTED review from one with inline findings, and does not report a pass on an older head as clean (R5);
  - a green `validate-settled` after a flake re-run carries `ci_rerun` naming the check, and one without a re-run does not (R7);
  - item-scoped pushes carry `title` (R8).
- **No-GitHub guarantees:** the local-API and channel no-GitHub scans stay green.
- `go test -race ./...` is clean.

Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.
