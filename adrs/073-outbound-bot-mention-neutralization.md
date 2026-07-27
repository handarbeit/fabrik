# ADR 073: Outbound Bot-Mention Neutralization

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1141 — Fabrik @-mentions bot logins in stage output, provoking auto-replies it then processes (self-sustaining loop, $7.75 on #933)

## Context

Incident #933: Fabrik's comment-processing summary named CodeRabbit with a live `@`-mention — `**@coderabbitai**: No action taken, no reply posted...` — inside a comment Fabrik itself posted. GitHub does not care that the surrounding prose says "no reply posted"; an `@login` substring in a posted comment body is a live mention regardless of intent, and CodeRabbit was subscribed to the thread. The mention notified CodeRabbit, which auto-replied with an acknowledgement, which Fabrik's existing filters (`isBotServiceNotice`, ADR-070) did not recognize as non-actionable (see ADR-074 companion fix), so Fabrik processed it as a new comment and posted another summary — again naming the bot. 38 CodeRabbit replies, 47 Fabrik comments, 39 comment-processing invocations, $7.75, over roughly 25 minutes, stopped only by an operator applying `fabrik:paused`.

This is the "outbound" half of two independent defects that together produced the loop (the "inbound" half — recognizing CodeRabbit's specific acknowledgement shape as non-actionable — is a straightforward additive pattern to `botServiceNoticePatterns`, following ADR-070's own precedent, and does not need a dedicated ADR). The outbound half is the more important of the two: it is the "pump" that would keep re-notifying the bot regardless of which specific acknowledgement phrasing triggered admission, and it is a new formatting-layer behavior with real design decisions, which warrants its own record.

## Decision

**1. Neutralize inside the existing canonical formatters, not a new call site.** A new `neutralizeBotMentions(text string) string` (`engine/mentions.go`) is called from inside `formatOutputComment`, `formatPRSummaryComment`, and `formatReviewFeedbackComment` (`engine/pr.go`), from `updatePRVerification` (`engine/pr.go`, a `## Verification` PR-body rewrite — a second, `UpdateIssueBody`-based exposure outside the `AddComment` funnel that can equally trigger a live mention), and from `buildAwaitingInputComment` (`engine/item.go`). This satisfies the issue's "apply at the single point where output is posted, not per-prompt" requirement using the funnel that already exists, and is transparent to `engine/compliance_test.go`'s `TestAddCommentCompliance` — an AST-walking test that only inspects *which* formatter a caller invokes, not what happens inside it.

**2. Render as an inline code span, not `@`-stripping.** A detected bot mention — e.g. `@coderabbitai` — is rewritten to `` `@coderabbitai` ``. GitHub never linkifies (and therefore never notifies on) an `@name` inside backticks; this is also the convention CodeRabbit's own walkthroughs already use for their own slash-commands. The `@` is preserved so a human reader still sees which account is referenced — only the live-mention behavior is suppressed, not the information.

**3. Detection is code-span-aware and therefore idempotent.** `neutralizeBotMentions` first locates existing inline code spans (`` `[^`\n]*` ``) and only scans text *outside* them for mentions. A mention that is already backtick-wrapped — whether written that way by Claude itself, or by a prior neutralization pass — is left untouched. This means the transform can be safely applied more than once to the same text (e.g. `postOutputToPR` passing the same raw `output` independently into two formatters) without double-wrapping.

**4. Bot-login matching closes the bare-mention gap, not a `+"[bot]"` re-check.** `gh.IsBotLogin("coderabbitai")` was `false` before this change — only the GitHub API login `coderabbitai[bot]` matched the existing `[bot]`-suffix rule, but the text GitHub actually renders (and mentions) is the bare `@coderabbitai`, which is exactly what triggered the incident. The fix adds `"coderabbitai"` as a third bare-literal exact match in `isBotLogin` (`github/types.go`), following the existing precedent of `"dependabot"` and `"gemini-code-assist"` — both bots with the same API-login-vs-mention-surface split. The alternative considered — checking `IsBotLogin(capturedLogin + "[bot]")` per candidate mention — was rejected outright: appending the literal string `"[bot]"` to *any* captured name makes it match `isBotLogin`'s own suffix rule unconditionally, neutralizing every mention in the text, bot or human. That is a correctness bug, not a narrower alternative.

**5. Mention-boundary matching mirrors GitHub's own rules closely enough to avoid the obvious false positive.** `mentionRE` (`engine/mentions.go`) only treats `@` as a mention start when preceded by start-of-text or a non-login character (not alphanumeric/underscore). This incidentally means an email-like `support@dependabot.io` is left alone — the `t` immediately before `@` blocks the match — without needing a dedicated exclusion.

## Rationale

### Why is this scoped to bot logins only, not all mentions?

The loop mechanism is specifically bot auto-reply → Fabrik mention → bot auto-reply — only a bot-login mention is a control-loop input, since only a bot both (a) receives the notification and (b) can auto-reply to re-trigger the cycle. Neutralizing human mentions in generated output would be a separate mention-hygiene change with no bearing on this defect class, and the issue explicitly scopes it out.

### Why cover `updatePRVerification` and `buildAwaitingInputComment`, not just the three comment formatters?

The Definition of Done — "Stage output containing a bot login does not notify that bot" — is not qualified to "in a comment." Both of these embed Claude-derived text into GitHub-persisted, notification-capable content (a PR body section; an issue comment carrying a deliberate *human* `@mention` for push-notification purposes, whose embedded `summary` blockquote is still Claude's freeform text and could itself name a bot). The marginal cost of covering both is one call each.

### Why not extend the same bare-literal fix to Copilot's identical gap (`isBotLogin` matches `copilot-*`-prefixed logins, but Copilot's actual mention surface is bare `@copilot`)?

Nothing in this issue's Definition of Done, Testing, or Scope calls for it, and `"copilot"` is a materially more collision-prone literal to add speculatively than `"coderabbitai"` — a real human GitHub username coinciding with the former is far more plausible. Left as a known, documented follow-up gap rather than spending the false-positive budget here.

### Why not also neutralize `extractUpdatedBody`'s `FABRIK_ISSUE_UPDATE` issue-body-rewrite path?

That path rewrites the issue's own spec body — Specify-stage-shaped content, not the "no action taken" comment-processing acknowledgement pattern that drives this loop. It is the same general class of exposure (generated text landing in a GitHub-persisted, notification-capable body) but categorically lower probability in practice, and the issue's Scope section doesn't call it out. Documented as a residual, not fixed here.

## Consequences

**Positive:**
- A bot-naming stage or comment-review summary can no longer notify that bot, regardless of what the surrounding prose claims about whether a reply was posted — closing the specific outbound pump that sustained the #933 loop.
- The fix lives entirely inside existing formatting functions; no new `AddComment`/`UpdateIssueBody` call sites, no compliance-test changes, no new labels or config surface.
- Idempotent by construction — safe to have been applied more than once to the same underlying text without visual corruption (double-wrapped backticks).

**Negative / Trade-offs:**
- **False negatives remain possible for logins outside `isBotLogin`'s pattern list** (notably Copilot's bare `@copilot` form, deliberately left unfixed here — see Rationale). A new bot vendor with an unmatched bare-mention form would need its own literal added, mirroring this ADR's precedent.
- **A human login that happens to equal a bare bot-pattern literal** (e.g. a hypothetical user named exactly `coderabbitai`) would have their mention silently backtick-wrapped in generated output. Low severity — worst case, a human doesn't get a deserved mention baked from Claude's own prose, which is both rare and incidental (unlike `isBotLogin`'s other consumer, the pause/resume gate, where the same kind of false positive silently breaks a human's ability to unpause — see `IsBotLogin`'s doc comment in `github/types.go`).
- **The mention-boundary regex is a heuristic, not a full reimplementation of GitHub's mention grammar** — it does not, for example, reject a bot-pattern literal immediately followed by additional login-shaped characters that would extend past a 39-character GitHub username limit. Not a concern for any bot login currently matched by `isBotLogin`, all of which are well under that limit.

## Related Work

- Incident #933 — the live loop ($7.75, 39 invocations) this ADR closes the outbound half of.
- ADR-070 / #1088 — the bot service-notice filter this issue's inbound half (the CodeRabbit auto-generated-reply marker) is additive to.
- ADR-069 / #1087 — establishes `github.IsBotLogin` as load-bearing for comment-author filtering; this ADR is a second, independent consumer of the same primitive for mention-text matching rather than author matching.
- #1089 — the general comment-processing circuit breaker (open, not implemented here); complementary defense-in-depth for any loop variant that slips past noise filtering entirely, per the issue's own Risks/Dependencies section.

**References:** [docs/state-machine.md §4.1](../docs/state-machine.md)
