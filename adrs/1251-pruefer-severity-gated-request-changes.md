# ADR 1251: Pruefer severity-gated REQUEST_CHANGES (amends ADR-1113 §8)

**Status:** Accepted
**Date:** 2026-07-30
**Issue:** [#1251](https://github.com/handarbeit/fabrik/issues/1251)

## Context

ADR-1113 §8 ("Why V1 never approves or requests changes") recorded that `SubmitPRReview` hardcodes `event: "COMMENT"` with no caller-controlled parameter at all, so Pruefer could not escalate to `APPROVE` or `REQUEST_CHANGES` even by a confused or compromised Claude invocation. That was the right call while Pruefer was a second opinion alongside CodeRabbit: a comment-only review was sufficient to satisfy Fabrik's `hasReviews` gate predicate, and there was no unblock mechanism for a blocking review anyway.

Retiring CodeRabbit makes Pruefer the sole automated reviewer. Projects opting into `review_authority: authoritative` (ADR-1250, a per-stage YAML field) need a reviewer whose verdict can actually bind Fabrik's progression — not just be present. A comment-only review satisfies `reviewGateOutstanding`'s "has responded" check regardless of content, so it cannot, by itself, express "changes are required." This issue adds that expressive capability — the raise half only. Auto-`APPROVE` remains out of scope permanently: approval is an accountability decision that must not rest on a bot rubber-stamping itself.

## Decision

**Amends ADR-1113 §8.** The prior decision ("there is no code path that can produce `APPROVE` or `REQUEST_CHANGES`") is superseded for `REQUEST_CHANGES` only: Pruefer now submits `event: "REQUEST_CHANGES"` when severity-gating is enabled and a finding's severity meets a configured threshold. `APPROVE` remains categorically unreachable — this is the one part of §8 that does not change, and is now enforced more strongly than before (see below).

### Severity is new to the finding schema

`ReviewFinding` (`pruefer/findings.go`) gains a `Severity` field, JSON-tagged `"severity"`, populated from Claude's fenced JSON findings block exactly like `Path`/`Line`/`Body` already are. `buildReviewPrompt` (`pruefer/claude.go`) asks for one of four ordinal tiers per finding: `low`, `medium`, `high`, `critical` — deliberately minimal, just enough for a threshold comparison, not a multi-dimensional risk score (that remains #1051/#1177's separate, unrelated concept: which PRs/repos need what tier of *human* review, decided independently of any one finding's severity).

### The event is computed Go-side from parsed severity, never from prose

`decideEvent(findings []ReviewFinding, threshold Severity) gh.ReviewEvent` (`pruefer/review.go`) is a pure function reading only each finding's already-JSON-unmarshaled `Severity` field. It never inspects `result.Text`, the prose summary, or any finding's own `Body` — so a PR that tries to inject text like "APPROVE" or "REQUEST_CHANGES" into what Claude reads or writes has zero effect on the submitted event. This preserves ADR-1113 §5's "never escalate via prompt" invariant exactly, just extended to a now-nonzero decision space.

`decideEvent` runs on the full pre-partition findings slice, not just the subset that anchored to a diff line — a severity-worthy finding that couldn't be anchored (and was demoted into the body instead) must still count toward the threshold; diff-anchoring success is orthogonal to severity.

### No APPROVE, structurally — a stronger guarantee than before

ADR-1113 §5 made "never approve" a code-level invariant by giving `SubmitPRReview` no event parameter at all — nothing to set, so nothing could be misused. Adding a parameter could have weakened that to convention-level (a `string` with a runtime `if event != "COMMENT" && event != "REQUEST_CHANGES"` guard, say). Instead, `github.ReviewEvent` (`github/prs.go`) is a struct with a single **unexported** field: no package outside `github` can construct one carrying an arbitrary string — the only way to obtain a value is to copy `ReviewEventComment` or `ReviewEventRequestChanges`, or receive the zero value. `SubmitPRReview` itself additionally normalizes defensively: only a `ReviewEvent` whose internal string is exactly `"REQUEST_CHANGES"` escapes the `"COMMENT"` default — so even a hypothetical future in-package mistake (a stray `ReviewEvent{raw: "APPROVE"}` literal, say) still can't reach the wire as anything but `COMMENT`. This is stronger than ADR-1113's original guarantee: it now survives a future `pruefer`-side or `github`-side code change, not just today's prompt-injection concern.

### Config: single field, default off, fail loud on typo

`Config.RequestChangesThreshold Severity` (`pruefer/config.go`) follows the flag > env > YAML > default precedence chain every other Pruefer setting uses. Its zero value (`""`) means the feature is off — every review submits `COMMENT`, byte-for-byte the pre-#1251 behavior — matching the "unset = off" convention every other Pruefer config field already follows. This is global to the daemon instance, like every other Pruefer setting; Pruefer has no per-repo config override mechanism, and building one is out of scope here (flagged as future work only if a real per-repo need emerges).

`LoadConfig` rejects any non-empty value that isn't one of the four recognized tiers. This is deliberately asymmetric with per-finding severity handling: a mistyped **config** value is a one-time operator error worth catching at startup — an unvalidated typo would otherwise rank as 0 via `severityRank`'s fail-closed default, making `severityRank(finding) >= 0` true for *every* finding and turning every review into `REQUEST_CHANGES`, which is worse than doing nothing. A **finding's** severity, by contrast, comes from an untrusted LLM output and must fail closed toward `COMMENT` on anything unrecognized or missing — never toward the more disruptive outcome.

### Unblocking relies on GitHub's stale-review dismissal, not self-approval

When Fabrik or a human pushes a fix, GitHub's "dismiss stale reviews on push" branch-protection setting auto-dismisses the stale `REQUEST_CHANGES` review; Pruefer's next poll re-reviews the new head SHA independently (`decideEvent` carries no state across calls — a clean re-review structurally cannot inherit a prior block). This is documented in `cmd/pruefer/README.md` as a setup prerequisite. Resolving inline review threads is not a substitute: thread resolution and review-state dismissal are different GitHub mechanisms, and Fabrik's own auto-merge gate already reacts to unresolved threads (#1207) independently of this review-state gate.

### Self-dismiss: documented, not implemented

The issue allowed Pruefer to optionally dismiss its own prior `REQUEST_CHANGES` review (`PUT /pulls/{n}/reviews/{id}/dismissals`) as a fallback for repos without stale-review dismissal enabled. This is **not implemented** in this issue: dismissing a review on a *protected* branch requires the GitHub App's installation to be explicitly listed in that branch protection rule's dismissal-allowed actors — a separate, per-repo, manually-configured GitHub setting beyond the `pull_requests: write` App permission the issue anticipated. Given this materially larger operational ask for an explicitly-optional feature, implementing it now would be over-scoping; the requirement is satisfied by documenting the real prerequisite in `cmd/pruefer/README.md` without building it.

## Consequences

**Positive:**
- Pruefer as sole reviewer can now produce a binding verdict for `review_authority: authoritative`, human-merge paths, and repos with branch protection — closing the gap #1207's thread-based auto-merge gate didn't cover.
- The "no APPROVE, ever" guarantee is now type-level (unconstructable outside `github`), not merely "no parameter exists" — a strictly stronger property than ADR-1113 §5 originally established, and one that survives future code changes on both sides of the package boundary.
- Zero engine-side changes needed: ADR-1250's `reviewGateAuthorityVerdict` already consumes a `CHANGES_REQUESTED` review identically whether it came from a human or from Pruefer.

**Negative / Trade-offs:**
- A repo without "dismiss stale reviews on push" enabled has no automatic unblock path today; an operator must either enable that setting or manually dismiss Pruefer's review. Self-dismiss remains a documented, not-yet-built option for that case.
- Severity is Claude's own self-assessment per finding, with no independent verification; an under-reported severity fails toward `COMMENT` (safe), but an over-reported one could trigger `REQUEST_CHANGES` on a finding an operator wouldn't consider critical. Mitigated by the threshold being operator-configured and off by default.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` §5 (unaffected: Claude still never calls `gh` or submits the review itself) and §8 (amended by this ADR).
- `adrs/1250-review-authority-orthogonal-to-autonomy.md` — the consumer of this issue's `REQUEST_CHANGES` output; requires no changes of its own.
- `adrs/1189-pruefer-inline-review-comments.md` — prior precedent for extending `SubmitPRReview`'s signature (added `comments []ReviewComment`) without loosening ADR-1113's invariants; this issue's `event` parameter follows the same shape.
- `#1207` — Fabrik's thread-based auto-merge gate, which this issue's review-state gate complements rather than replaces (thread resolution ≠ review dismissal).

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
