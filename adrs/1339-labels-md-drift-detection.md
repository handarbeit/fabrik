# ADR 1339: LABELS.md Drift Detection

**Date**: 2026-08-02
**Status**: Accepted
**Issue**: #1339 — Ship Fabrik label knowledge to the stage skills that reach consumer projects

## Context

`plugin/fabrik-workflows/LABELS.md` ships a ~12 KB, hand-curated condensation of `docs/state-machine.md`
§1.4 ("Label Semantics Reference," a ~30-row table) to consumer projects via the plugin embed. Requirement
6 of #1339 states that any divergence between `LABELS.md` and `docs/state-machine.md` is a bug — a
hand-maintained derivative of a much larger source, with no check, will drift, and the failure mode
(consumer projects reading confidently wrong label documentation) is worse than the pre-#1339 gap (no
documentation at all).

The obvious precedent is `.github/workflows/docs-drift.yml` and `scripts/generate-llms-full.sh`:
regenerate `docs/llms-full.txt` deterministically from the four canonical doc pages, fail the PR if the
committed file differs from the regen output. That pattern requires the derived artifact to be a
byte-for-byte, deterministic function of its sources — `llms-full.txt` is a literal concatenation.

`LABELS.md` is not that. It is condensed roughly 40:1 from `state-machine.md`, curated into prose grouped
by lifecycle role (Pause & Block / Gates / Operator Overrides / Engine-Internal) rather than
`state-machine.md`'s own Controlling/Modifier split. There is no function `f(state-machine.md) →
LABELS.md` to write — regenerating it would mean either turning it into a mechanically-templated document
(defeating the "reference sized for on-demand reading" framing that motivated writing curated prose in
the first place) or comparing `LABELS.md` to itself (a no-op).

## Decision

**Drift detection is a name-presence check, not byte-exact regeneration.** A Go test
(`plugin/labels_drift_test.go`) parses `docs/state-machine.md` §1.4's table, extracts each row's label
identifier, strips `<placeholder>` suffixes to a literal prefix (e.g. `` `fabrik:locked:<user>` `` →
`fabrik:locked:`), and asserts each prefix appears somewhere in the concatenation of `LABELS.md` plus all
12 `SKILL.md` files. This catches the failure mode Requirement 6 is actually worried about — a label
added, renamed, or removed in the canonical doc and never propagated to consumers — without requiring a
regenerator that cannot exist.

This is a **deliberately weaker guarantee** than `docs-drift.yml`'s byte-exact check: it verifies a
label's *name* is mentioned, not that its *documented semantics* match. A label whose behavior changes in
`state-machine.md` without a name change will not be caught. This gap is accepted and stated explicitly
here, in the test's own doc comment, and in `LABELS.md`'s closing "Notes for skill authors" section — so
it is never mistaken for a stronger guarantee than it provides. Requirement 6 asked Research/Plan to
either implement a mechanical guard or record why one isn't feasible; the conclusion here is a middle
ground — a real, if narrower, mechanical guard, with the narrowing made explicit rather than silently
accepted.

**The check reads `docs/state-machine.md` from the source tree, but `LABELS.md`/`SKILL.md` from
`plugin.FabrikPlugin`** (the embed), not the source tree. `docs/` is never embedded — correctly, since
it's fabrik source and never ships — so the canonical side has no choice but to be a source-tree read.
Reading the consumer side from the embed rather than the source tree is a deliberate second win: the
test also fails if the `//go:embed fabrik-workflows/LABELS.md` directive is ever accidentally dropped
from `plugin/embed.go`, catching an embed-wiring regression in the same test that catches content drift.

**The check is a Go test, not a dedicated CI workflow.** It rides the existing `go test ./...` job with
no new required-status-check registration. The alternative — a `docs-drift.yml`-style dedicated workflow
— was rejected on direct precedent: #1234/#1236 (both closed) document that a drift-check workflow
existing in `.github/workflows/` is not sufficient by itself to block a non-compliant merge; it must also
be explicitly added to `required_status_checks.contexts` on branch protection, a step that was missed
once already in this repo. A Go test avoids that entire failure class — it is already a required check by
virtue of being part of the existing test suite.

## Consequences

- A label added, renamed, or removed in `docs/state-machine.md` §1.4 without a corresponding update to
  `LABELS.md` or the 12 skill files fails `go test ./...`, the same gate every other Go change in this
  repo already passes through — no new CI surface, no new branch-protection configuration.
- The guarantee is explicitly weaker than `docs-drift.yml`'s: a label whose *semantics* change without a
  *name* change is not caught. This is stated in three places (this ADR, the test's doc comment,
  `LABELS.md` itself) so a future reader does not mistake partial coverage for full coverage.
- `LABELS.md`'s ~15 KB size budget remains unenforced mechanically (Research's Constraint 2) — this ADR
  covers drift detection only, not size. That risk is accepted as noted in the issue's Risks section.
- If `docs/state-machine.md` §1.4 is ever restructured (e.g. the table format changes, or labels move to
  a different section), `plugin/labels_drift_test.go`'s parser will need a corresponding update — it is
  coupled to the table's current Markdown shape, not just its content.
