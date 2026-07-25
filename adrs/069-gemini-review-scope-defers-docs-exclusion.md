# ADR 069: Gemini Review Scoping Defers docs/** Exclusion

**Date**: 2026-07-25
**Status**: Accepted
**Issue**: #1067 — scope Gemini Code Assist to substantive PRs via `.gemini/config.yaml`
**Related**: #1071 (open, undecided) — de-fragilize the review gate's single-source dependency on formal-review bots

## Context

#1067 asked for `.gemini/config.yaml` to skip Gemini Code Assist's automatic
review on draft PRs, docs-only PRs, and lockfile/manifest-only PRs — reducing
consumption of Gemini's per-installation daily quota on PRs where its
*diverse second opinion* adds least value.

Research for #1067 found that Fabrik's own `wait_for_reviews: true` gate
(`checkReviewGate`, `engine/reviews.go`) clears only when at least one
non-dismissed formal `pull_request_review` exists, from *any* source. In this
repo that source is Gemini and Gemini alone: there is no `CODEOWNERS`, no
auto-reviewer-request mechanism, Copilot signups are closed, and Claude's own
review integrations don't submit formal `pull_request_review` objects (per
#1071's findings). At the time of Research, issues #1058 and #1065 were
*already paused* on exactly this quota-exhaustion failure mode — proof the
SPOF is live, not hypothetical.

Excluding `docs/**` from Gemini's `ignore_patterns`, as originally specified,
would mean every docs-only Fabrik PR (produced routinely by the
`audit-documentation` skill) has zero possible source of `hasReviews`. Such a
PR would time out at the Review and/or Validate gate and pause for manual
intervention every single time, with no natural clearing path, until #1071
lands a gate fix. That is a direct regression traded for the quota
improvement this issue set out to make.

The lockfile/manifest class (`go.mod`, `go.sum`) does not carry the same
risk: this repo has no Dependabot or Renovate configured, so there is
currently no automated producer of lockfile-only PRs. The exposure is limited
to a human manually bumping a dependency alone — rarer and lower-stakes than
the docs-only case, which runs on a schedule via an existing skill.

## Decision

Ship `.gemini/config.yaml` with `include_drafts: false` and
`ignore_patterns: [go.mod, go.sum]` only. Do **not** add `docs/**` or
`**/*.md` to `ignore_patterns` yet.

`include_drafts: false` carries no comparable gate risk: Fabrik's PRs are
created as drafts and marked ready before Review runs, and Gemini's
automatic review already fires on the ready-for-review transition (observed
on ~9 of the last 12 merged PRs at Research time) — so excluding drafts
doesn't remove Gemini's only opportunity to review a Fabrik-authored PR the
way excluding `docs/**` would.

This is a deliberate, scoped reduction from #1067's literal spec — decided
rather than blocked, consistent with how #1067's own Specify stage handled
its own schema-gap discovery (documented and resolved, not left open).

## Revisit condition

Once #1071 lands a fix that gives this repo a review-gate clearing path
independent of Gemini (e.g. a CODEOWNERS-based reviewer, an alternate formal
review source, or a gate redesign that no longer requires a formal
`pull_request_review`), revisit this scope and add `docs/**` / `**/*.md` to
`.gemini/config.yaml`'s `ignore_patterns` to fully satisfy #1067's original
request.

**Do not add `docs/**` to `ignore_patterns` before #1071 resolves the gate
dependency** — doing so silently reintroduces the exact deadlock this ADR
exists to avoid, for every docs-only Fabrik PR the `audit-documentation`
skill produces.

## Consequences

- Docs-only PRs continue to consume Gemini review quota until #1071 lands.
  This is the accepted tradeoff, not a defect.
- `go.mod`/`go.sum`-only PRs no longer trigger automatic Gemini review, and
  draft PRs are skipped until marked ready — the portion of #1067's request
  that carries no gate risk ships immediately.
- No author-based or title-based filtering exists in Gemini's current
  config schema regardless of this decision (a separate, unrelated schema
  limitation documented in #1067 itself).
