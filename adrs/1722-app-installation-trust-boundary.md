# ADR 1722: App Installation Trust Boundary — Private by Default, Per-Account Apps, Remove Unauthorized Installations

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1722 — App installation trust boundary; measured incident: an unrecognized
organization (`kolfadser1`) installed the public `handarbeit-pruefer` App and was minted a
token every 10 minutes for three weeks, unnoticed

## Context

`handarbeit-pruefer` is a **public** GitHub App, spanning `handarbeit`, `verveguy`,
`liminisapp`, and `shadoworg`. It was made public by hand, at App-creation time — not by any
code path (`buildManifest` has always requested `"public": false`) — because the
architecture had no supported answer for an operator serving several accounts: a private
App can only be installed on the account that owns it, and a `Reconciler` carries exactly
one App identity (`Options.AppID`/`Options.AppPrivateKeyPath`). Faced with that gap, the
operator reached for the one thing that actually works — making the App public — which also
makes it installable by anyone who finds its URL.

On 2026-08-24, an unrecognized organization, `kolfadser1`, did exactly that: installed the
public App, granting one machine-named repository
(`kolfadser1/cluster_40pvihou_1787546401`). Pruefer's own installation-derived discovery
(ADR-1641) then enumerated that installation every 10 minutes for three weeks — 831 log
lines across three log files — minting a fresh installation token each time. No review ever
ran there and no Claude budget was spent, only because `kolfadser1` happened not to be named
in `watched_repos` — luck of configuration, not a guarantee. The installation went unnoticed
the entire time because it was logged at the same tag/level as every other, routine
enumeration line.

This directly falsifies ADR-1641 Decision 8's central premise: *"once every operator runs a
dedicated App... 'installed on it' and 'mine to review' become the same statement... no
owner-allowlist... was introduced [because it's unneeded]."* That premise assumed every
operator's App is **private**. The live App is public, so `GET /app/installations` returning
"every installation of this App" and "every installation I actually meant to authorize" are
not, in fact, the same set. This ADR amends that decision — it does not reverse ADR-1641's
core inversion (installations, not `watched_repos`, are still the primary discovery input)
— it adds back a second, independent, defense-in-depth layer for the case that assumption
doesn't hold.

Separately, **Fabrik is about to ship the same App-manifest pattern via #770's engine
identity** (#1712/#1713/#1715), to users who are not the maintainer — this needed an answer
before that lands, not after.

## Decisions

### 1. R3: account-granularity mint refusal against `watched_repos`, not repo-granularity

`Reconciler.derive()`'s per-installation loop now refuses to mint (or keep minted) a token
for any installation whose account isn't named as an owner anywhere in a **non-empty**
`watched_repos` — `neededOwnersFromFilter(filter)` returns `nil` for an empty filter
(ADR-1641's "all-installations mode" is unchanged: with nothing to compare against, every
installation trivially "serves" it) and a lower-cased owner set otherwise.

Considered and rejected: a repo-granularity check via `GET /repos/{owner}/{repo}/installation`
(JWT-authenticated, so it never needs to mint a token to answer the question) for
`"selected"`-mode installations, floated during Research as a way to answer "does this
specific installation actually grant this specific watched repo" without a chicken-and-egg
mint-then-check. Rejected because the finer-grained question is already answered, post-mint,
by the existing `verifyRepoAccess`/`FilteredOut` mechanism (ADR-1233 Decision 6) — it doesn't
need to gate minting itself. The incident is fully caught by account-level matching alone,
with none of the new endpoint, extra per-repo API calls, or restructuring cost the
repo-granularity mechanism would add.

### 2. R4/R5: `served_accounts` is a new, independent signal — never derived from `watched_repos`

`Options.ServedAccounts []string` (mirroring `Options.RequiredPermissions`'
caller-supplied-`Options`-field precedent, ADR-1709) is the **sole** basis for R4's
destructive action: deletion never consults `watched_repos`. Coupling deletion to a filter
that's explicitly optional/narrowing-by-design (ADR-1641) would make an empty
`watched_repos` (all-installations mode) either delete every installation or none,
unpredictably — and would force an operator who merely wants review scoped down to also risk
installations they still want authorized. `served_accounts` exists precisely so the
irreversible action has its own explicit, low-ambiguity gate:

- **Configured** (`resolveRecognizedAccounts` returns `explicit = true`): an installation
  whose account isn't in the allowlist is unrecognized, and `derive()` calls
  `gh.DeleteAppInstallation` for it — logged loudly either way (success, already-gone via
  `errors.Is(err, gh.ErrNotFound)`, or failure) — and never minted this round regardless of
  whether the deletion itself succeeded.
- **Not configured** (the default): nothing is ever deleted (AC3). R5's "still reportable"
  requirement is met by a **fallback signal**: an installation is also `Unrecognized` when
  `watched_repos` is non-empty and doesn't name its account either — reusing an
  already-typed-in list rather than forcing every operator to maintain a second one just to
  get alerting. When *both* `served_accounts` and `watched_repos` are empty, nothing is ever
  flagged — the deliberate "all-installations mode" trust is unchanged.

Because the fallback's recognized set and R3's needed-owners set are built from the exact
same `watched_repos` list, a fallback-unrecognized installation is, by construction, always
also not-serving-watched-repos — it is never minted, matching R3's guarantee independently of
whether `served_accounts` happens to be configured.

### 3. No implicit self-exemption for the App's own owning account

`FetchAppInstallations` returns the App's own owning account's installation indistinguishably
from any other. Rather than add fragile "is this the owner?" heuristics, an operator's
`served_accounts` list must include every account they want served, **including their own**
— consistent with R4's "explicit configuration" framing.

### 4. No cross-restart persistence for "newly appearing" — the loud alert simply repeats

The `UNRECOGNIZED-INSTALLATION` log marker (reserved exclusively for this class of event —
Research's finding that Pruefer's logging has no level/severity concept at all ruled out
"log it louder at the existing tag") fires on **every** re-derivation cycle an installation
remains unrecognized, not once per newly-discovered ID. This satisfies AC4 ("distinct from
routine enumeration") without a persisted seen-installation-ID set in `app-state.json` —
repeating a loud, correctly-tagged alert is strictly safer than a persistence bug silently
under-alerting after a restart, and the incident's own 831-line timeline shows that
*frequency* was never the problem; *indistinguishability* from routine lines was. The TUI's
`ptui.UnrecognizedInstallationsEvent` mirrors this: a level (the current snapshot, `Count`
and `Accounts`), not a delta, so its footer banner clears the moment a later cycle no longer
finds one — parity with `SignatureDriftEvent`/`FooterComponent`'s existing escalation
pattern (ADR-1563).

### 5. A truncated installation list does not suspend R3/R4 enforcement

`FetchAppInstallations`/`FetchInstallationRepositories` are already truncation-aware
(ADR-1641 Decision 3). Every installation actually *returned* by a truncated round is
unambiguous on its own — only installations beyond the pagination ceiling are unevaluated.
So R3/R4 still apply to every returned entry, with a loud warning that the sweep may be
incomplete. This is a deliberate deviation from a more conservative "skip enforcement
entirely on any truncation" alternative: that alternative risks a known-bad installation
(like `kolfadser1`, once identified) getting an indefinite pass merely because pagination
truncated *elsewhere* in an unrelated part of the list — the less safe failure mode.

### 6. Detachment is unified, not a new mechanism

`derive()`'s existing detachment loop (an owner whose installation disappeared entirely) is
extended to also detach an owner whose installation still exists but is no longer wanted
this round — removed via R4, unrecognized-and-report-only via R5's fallback, or no longer
needed via R3. All three collapse onto one `wantOwners`-driven condition, reusing the
existing `RemoveOwners`/`DetachedAuth` drain-then-stop contract unchanged: an in-flight
review holding a `*gh.Client` backed by a just-detached `Auth` is still allowed to finish,
exactly as an ordinary `watched_repos` edit already guaranteed.

### 7. R3/R4/R5 do not extend to pinned-installation mode

`AppInstallationID != 0` never calls `FetchAppInstallations` at all (`derivedSetForPinned`'s
existing exemption from R1's discovery/filtering, ADR-1233 Decision 4) — there is no
discovery step to apply an allowlist or unrecognized-check to, and thus nothing this ADR's
mechanisms can attach to. An operator wanting these protections uses non-pinned discovery,
the supported, documented path already.

### 8. R1/R2/R6: documentation-only, no new code path

`buildManifest` already hardcodes `"public": false` unconditionally (no config path can
override it) — R1's code requirement predates this issue. The remaining work is
documentation: `cmd/pruefer/README.md` now states plainly that a private App can only be
installed on its account, replaces the three passages that previously implied one App could
safely span multiple accounts, adds an explicit "one App, one daemon, per account" model
(R2) with its own subsection, and states outright — as a permanent prohibition, not a
re-litigable preference — that Fabrik must never ship a single shared App whose private key
users configure (R6): one key would control every installation of every user, and its
onboarding convenience is exactly why it needs to be written down as forbidden rather than
left to be rediscovered as a shortcut.

### 9. R7: the migration runbook is documentation, not executed code

Migrating the live `handarbeit-pruefer` App off its public, multi-account shape is a
human-operational sequence, not a Pruefer feature: GitHub refuses to flip a public App to
private while it's installed on any non-owner account, so the runbook
(`cmd/pruefer/README.md`'s "The per-account model" section) sequences around that hard
external constraint — register a private App per additional account, stand up a daemon per
App, move each account across, and only then retire (or re-privatize) the original public
App. No code in this change performs any part of that migration.

## Consequences

**Positive:**
- AC1 (no token for an unwatched installation) closes the incident's actual mint-and-hold
  behavior, independent of whether `served_accounts` is ever configured.
- AC2/AC3 give an operator an explicit choice between active remediation (delete) and
  passive visibility (report), rather than forcing one or the other.
- AC4 makes a repeat of "logged 831 times, noticed never" structurally harder — a fixed,
  grep-able marker plus a TUI banner, independent of log volume.
- R2/R6/R7 give operators serving multiple accounts (a case the architecture previously had
  no good answer for) a documented, supported path that doesn't require making an App
  public.

**Trade-offs / accepted gaps:**
- `served_accounts` and `watched_repos` remain two lists an operator serving a subset of
  their own installations must keep mentally reconciled (Decision 2) — accepted because
  conflating them would make an already-narrow `watched_repos` a security boundary by
  accident.
- An operator who sets `watched_repos` non-empty but wants some *other* installation left
  entirely alone (not narrowed, not flagged) has no way to express that short of adding it to
  `watched_repos` or leaving `watched_repos` empty entirely — an existing ADR-1641 gap this
  issue's alerting makes more visible but does not itself widen or close.
- No time-delayed/confirmation window guards R4's deletion beyond the allowlist-configured
  gate itself — accepted as sufficient given deletion is self-healing (the installer can
  simply reinstall) and the allowlist is explicit, operator-authored config, not inferred.

## Prior Art / Amendments

- **Amends ADR-1641 Decision 8** ("Containment (AC7) is structural, not an added check... No
  owner-allowlist, denylist, or 'trusted accounts' list was introduced"). That reasoning
  holds precisely when every operator's App is private — this ADR's R3/R4/R5 are the
  defense-in-depth layer for when it isn't, or for the window before an operator has migrated
  off a public App via R7. ADR-1641's core inversion (installations are the desired state,
  `watched_repos` an optional intersection filter) is **not** reversed — R3 only *narrows*
  which installations that inversion actually mints for.
- **Relates to ADR-1233** Decision 1, whose original security model — "only ever mint a
  token for an owner in `watched_repos`" — is exactly what R3 reinstates in scoped form. This
  is the second amendment to that original reasoning (ADR-1641 was the first, more permissive
  one); together the two ADRs and this one describe a "tightened, then loosened, then
  re-tightened along an orthogonal axis" history worth reading in sequence for a new
  contributor.
- **Follows ADR-1709's** `Options.RequiredPermissions` pattern for `Options.ServedAccounts`:
  a caller-supplied field threaded through `Reconciler`, evaluated per-installation inside
  `derive`'s existing loop, soft by default with the one destructive action explicitly gated.
- **Follows ADR-1563's** `SignatureDriftEvent`/`FooterComponent` escalation shape for
  `UnrecognizedInstallationsEvent` — a level-triggered TUI banner, not an edge-triggered one.
- **#1715** (the #770 engine setup flow, out of scope for this issue's code) must adopt R1/R2
  once it lands, per the issue's own scope note — `Options.ServedAccounts` is already
  caller-agnostic (mirroring `RequiredPermissions`), so a future engine caller gets the R3/R4
  mechanism for free by supplying its own value; only the documentation adoption remains its
  own follow-up.
