# ADR 1951: Last-Known-Good Installation Repo Listings, and Rate Limits as Their Own Error

**Date**: 2026-09-29
**Status**: Accepted
**Issue**: #1951 — a rate-limited installation listing drops every repo, and is misreported as an auth failure

## Context

ADR 1641 made Pruefer derive its reviewed repo set live from the App's installations, with
"never cached, always live" as its premise. On 2026-09-29 a `GET /installation/repositories` call
for the `verveguy` installation hit GitHub's rate limit. `derive` recorded that failure as
`0 repo(s) accessible` and excluded every repo under the owner; the daemon went from 15 watched
repos to 4 until the next re-derivation, with only a warning line to show for it.

The same incident printed advice to switch a fine-grained PAT to a classic one. `appRequest`
mapped every 401/403 to `ErrAppUnauthorized` plus `authErrorHint`, but GitHub also uses 403 (and
429) for rate limiting, and the App client uses no PAT at all.

## Decision

### A failed listing is "unknown this round", never an empty grant

`Reconciler` keeps, in memory and per installation ID, the raw repo listing from that
installation's last **successful** `FetchInstallationRepositories` call (`lastGood`). On a failed
(or deferred) listing, `derive` substitutes it and marks the installation `Stale`; only a
successful listing may overwrite it, so only a successful listing can shrink the set.

- **Retained pre-filter, pre-cap.** The `watched_repos` filter, `FilteredOut`, the
  `max_derived_repos` cap and the sort then run unchanged over merged data, so a SIGHUP filter
  change still applies to stale data.
- **Cold start is an error, not zero.** With nothing to fall back on, the installation is logged
  `ERROR` and derives no repos; `RepoCount` 0 means unknown. A `watched_repos` entry under that
  owner is not reported as "not covered by any installation's grant", which would be false.
- **Partial pagination is a failed listing.** `fetchAppPaginated` already discards everything on
  a page error, so no partial page set can become the grant. A listing that merely hits the
  `appFetchMaxPages` ceiling is a *successful* truncated one and overwrites (the existing
  `Truncated` warning covers it).
- **Pruned aggressively.** Entries for installations not wanted this round (left the list,
  R4-deleted, unrecognized, narrowed out by R3) are dropped at the end of every `derive`, so a
  revoked or unwatched installation cannot keep its repos alive.
- **Not persisted.** A restart starts cold. The diagnostics `InstallationRepoCache` in
  `app-state.json` is a separate, diagnostics-only store and is not reused.

This qualifies ADR 1641's premise rather than reversing it: the retained listing is a fallback used
only on failure; every successful listing is still live and authoritative.

### Rate limits are a distinct, typed error

`github/ratelimit.go` adds `ErrRateLimited` and `*RateLimitError` (`errors.Is(err, ErrRateLimited)`;
`errors.As` yields `ResetAt`). One shared classifier is called from `appRequest`, `doWithAccept`
and `graphqlRequest`:

- **403** is rate-limited only on a positive signal: `X-RateLimit-Remaining: 0`, a `Retry-After`
  header, or one of the narrow body phrases `api rate limit exceeded`, `secondary rate limit`,
  `abuse detection`. A bare "rate limit" is deliberately not matched — a permissions 403 whose
  body quotes rate-limit docs must stay an authorization failure. A bare 403 is unchanged.
- **429** is always rate-limited (the HTTP meaning, and how the engine already treats it). This is
  slightly broader than the issue's "carrying" wording, which would leave a bare 429 unclassified.
- `ResetAt` comes from `Retry-After` (delta-seconds, capped before multiplication so it cannot
  overflow, or HTTP-date) and then `X-RateLimit-Reset`; malformed, zero or negative means unknown.
  The error carries the raw value; consumers clamp it.
- It never wraps `ErrAppUnauthorized` and never carries the PAT hint. The App client never emits
  the hint at all; the PAT client emits it only for a genuine 401 or non-rate-limited 403.

The error text keeps the `GitHub API returned %d: <body>` framing (`GitHub App API …` on the App
client) with a `(rate limited; resets at …)` suffix. `engine.isTransientAPIError` (#1313) and the
sim's `ghfault` classify by substring against that framing; `github` cannot import `engine`, so the
narrow phrase list is duplicated, and `engine/ratelimit_classification_compat_test.go` pins the
coupling. Changing the engine's own classification is out of scope.

One side effect is intended: `Reconcile`'s "was the App deleted?" check keys on
`ErrAppUnauthorized`, so a rate-limited 403 now takes the transient-retry branch instead of being
read as a possible deletion.

### Reset-aware scheduling, enforced inside `derive`

On a `RateLimitError` with a future `ResetAt`, `derive` records a per-installation hold until
`min(ResetAt, now+1h)` and skips that installation's listing call until then. A past or unknown
reset sets no hold, so the normal cadence applies. The hold is a floor, never a schedule: after it
expires the ordinary re-derivation cadence resumes.

Enforcing it in the listing step, rather than by changing the daemon's global ticker, is what lets
other installations keep their cadence. It also applies to every trigger (timer, webhook, SIGHUP),
which all converge on `Derive`: an early trigger would fail inside the window again, and the
retained set means nothing is lost meanwhile. The one-hour clamp is the primary rate-limit window
and follows the ADR 1815 precedent for untrusted reset times. `Reconciler` takes a `now` seam
(`nowFn`) so this is testable without sleeping.

## Consequences

- A transient listing failure no longer rewrites what Pruefer reviews. It surfaces as a `STALE`
  warning (log and TUI) naming the reason and, for a rate limit, the reset time.
- **Accepted trade-off:** a repo whose access is genuinely revoked keeps being reviewed until a
  listing succeeds. A failed read is not evidence of revocation; the `STALE` line makes it visible.
- A cold start under a persistently exhausted budget derives nothing for that installation and
  retries at the normal cadence or the reset; the `ERROR` line is the operator's only signal.
- Reducing Pruefer's per-poll API cost — the actual cause of the exhaustion — is tracked separately.

See also: ADR 1641 (derivation), ADR 1709 (grant verification, unaffected: shortfalls are still
reported for a stale installation), ADR 1815 (reset clamping precedent).
