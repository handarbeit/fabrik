package githubauth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// DerivedRepo is one repo Reconciler.Derive found accessible, paired with
// enough provenance (R4) to explain why it's in the effective set: which
// account's installation granted it, and that installation's own ID.
type DerivedRepo struct {
	// Repo is "owner/repo" in GitHub's canonical casing (the account login
	// and repo full_name Derive actually observed), not necessarily the
	// casing a watched_repos filter entry used to name it.
	Repo string
	// Owner is Repo's owner half, split out for callers (e.g. Pruefer's
	// poll()) that need it without re-parsing Repo.
	Owner string
	// RepoName is Repo's repo half.
	RepoName       string
	InstallationID int64
}

// DerivedInstallation summarizes one installation's contribution to a
// DerivedRepoSet, for logging and TUI display (R4) — independent of whether
// any of its repos survived an optional watched_repos filter (R3) or the
// max_derived_repos cap (R5): an operator asking "why isn't repo X being
// reviewed" needs to see that its installation was found and enumerated,
// even if X itself was filtered or capped out.
type DerivedInstallation struct {
	Account             string
	InstallationID      int64
	RepositorySelection string
	// RepoCount is how many repos this installation's own
	// FetchInstallationRepositories call returned, before any filter or cap
	// was applied.
	RepoCount int
	// RepoListError is non-empty when this installation's
	// FetchInstallationRepositories call itself failed (e.g. a transient
	// network error) — RepoCount is 0 in that case, but that 0 means
	// "unknown this round," not "genuinely zero accessible repos." Callers
	// (see logDerivedSet) must say so rather than implying the installation
	// was actually confirmed to grant nothing.
	RepoListError string
	// MintError is non-empty when minting a token for this installation
	// itself failed (e.g. a transient GitHub API error) — RepoListError is
	// always empty in that case, since FetchInstallationRepositories was
	// never even attempted with no token to call it with. Distinguishing
	// this from "no installation at all" matters because
	// guideMissingInstallations otherwise cannot tell a genuinely
	// uninstalled owner apart from one whose installation exists but whose
	// mint transiently failed this round — without this field, both looked
	// identical (absent from Installations entirely), so the former's
	// guided-install prompt (and, unless NoBrowser is set, an actual
	// browser-open) fired for the latter too: a misleading "go install the
	// app" message, and an unwanted browser popup, for what's usually a
	// transient error that the next reconciliation retry would clear on
	// its own. See ClientForRepo's mintErrors-aware error message, which
	// already made this same distinction for the "no client" case —
	// Installations must make it too, for the same reason.
	MintError string
	// PermissionShortfalls (#1709, R2) lists every required permission this
	// installation's actually-granted permissions don't meet, as determined
	// by checkGrantedPermissions against the Reconciler's requiredPermissions
	// (Options.RequiredPermissions). Empty when requiredPermissions is
	// nil/empty (no check configured) or every requirement is met. Computed
	// from inst.Permissions (the installations-list response), independent
	// of whether minting a client or listing repos succeeded this round —
	// still populated when MintError or RepoListError is set, since a
	// genuine permission shortfall must not go unreported just because it
	// coincides with an unrelated transient error this round.
	PermissionShortfalls []RequiredPermissionShortfall
	// NotServingWatchedRepos is true when this installation's account is not
	// named as an owner anywhere in watched_repos (R3) — only ever set when
	// watched_repos is non-empty (an operator-imposed narrowing; ADR-1641
	// made an empty watched_repos mean "review everything the installations
	// grant," in which case every installation trivially serves it). No
	// token is minted for such an installation (AC1) — it never held one
	// before this round either, if this is not its first time being seen as
	// unneeded (see derive's unified detachment loop). This is
	// account-granularity matching, not the finer per-repo question
	// verifyRepoAccess already answers post-mint.
	NotServingWatchedRepos bool
	// Unrecognized is true when this installation's account is outside the
	// set of accounts this deployment recognizes as its own (R4/R5) — either
	// explicitly, because ServedAccounts is configured and doesn't name it,
	// or as a softer fallback signal when ServedAccounts is unset but
	// watched_repos is non-empty and doesn't name it either (AC3's "still
	// reportable with no allowlist configured" case). See
	// resolveRecognizedAccounts. Never set when neither ServedAccounts nor
	// watched_repos carries any signal at all (the deliberate
	// "all-installations mode" default, where every installation is
	// trusted).
	Unrecognized bool
	// DeletionAttempted is true when this installation was Unrecognized
	// under an explicit ServedAccounts allowlist (R4) and this round called
	// gh.DeleteAppInstallation for it. Never true when Unrecognized is only
	// the softer watched_repos-fallback signal (R5) — that case is
	// report-only, never destructive (AC3).
	DeletionAttempted bool
	// DeletionError is non-empty when DeletionAttempted is true and the
	// delete call itself failed (a transient GitHub API error, distinct from
	// gh.ErrNotFound — an already-gone installation is treated as converged
	// success, not an error, so it never sets this field). No token is
	// minted for this installation this round regardless of whether the
	// deletion succeeded — a failed deletion is retried on the next
	// re-derivation, not treated as "well, we tried, mint it anyway."
	DeletionError string
}

// DerivedRepoSet is the result of one Reconciler.Derive call: every repo the
// App's installations currently grant access to, after the optional
// watched_repos intersection filter (R3) and the max_derived_repos cap (R5)
// have both been applied. Repos is sorted by lower-cased "owner/repo" so
// repeated Derive calls against an unchanged grant produce byte-identical
// output, and so R5's cap always drops the same repos rather than an
// arbitrary API-order tail.
type DerivedRepoSet struct {
	Repos []DerivedRepo
	// FilteredOut lists every filter entry that named a repo the
	// installation grant does not actually cover — reported, per R3/AC4,
	// rather than silently absent from Repos with no explanation.
	FilteredOut []string
	// Truncated is true when either FetchAppInstallations' or any
	// installation's FetchInstallationRepositories' pagination ceiling was
	// hit — the grant may be larger than what was actually enumerated.
	Truncated bool
	// Capped is true when max_derived_repos (R5) trimmed the union.
	Capped bool
	// CapApplied is the max_derived_repos value in effect when Capped is
	// true; zero otherwise.
	CapApplied int
	// PreCapCount is len(Repos) immediately before the max_derived_repos cap
	// was applied — i.e. after the watched_repos filter (R3), but before R5's
	// cap — valid only when Capped is true. This is deliberately NOT the same
	// as TotalGranted(): TotalGranted sums every installation's raw,
	// pre-filter RepoCount, which is a different (and, whenever a narrowing
	// watched_repos filter is also active, larger) number than what the cap
	// actually sliced from. Reporting TotalGranted() as "capped from N" would
	// misattribute repos the filter already excluded to the cap instead —
	// found by review (handarbeit-pruefer): a 1000-repo grant narrowed by
	// watched_repos to 250 and then capped to 200 would otherwise log
	// "capped from 1000 by max_derived_repos=200", implying the cap dropped
	// 800 repos when it actually dropped only 50 (the filter dropped the
	// other 750) — the opposite of R4's "make the derived set observable"
	// goal. See logDerivedSet/Pruefer's logRederivedRepos, which both report
	// this instead of TotalGranted() in the "capped from" message.
	PreCapCount int
	// Installations summarizes every installation Derive found, regardless
	// of whether any of its repos survived filtering/capping.
	Installations []DerivedInstallation
}

// TotalGranted returns how many repos the installation grant covered before
// any watched_repos filter or max_derived_repos cap was applied — the sum of
// every DerivedInstallation's RepoCount. Used by logging/TUI to distinguish
// "your installations grant N repos" from "M are actually being reviewed
// after your filter/cap," which len(Repos) alone can't show once either has
// trimmed the set.
func (s DerivedRepoSet) TotalGranted() int {
	total := 0
	for _, inst := range s.Installations {
		total += inst.RepoCount
	}
	return total
}

// derivedRepoFromFullName splits a GitHub "owner/repo" full_name into a
// DerivedRepo, attributed to installationID. Returns ok=false for a
// malformed full_name (missing/extra slash) — defensive only; GitHub's own
// API contract always returns a well-formed full_name.
func derivedRepoFromFullName(fullName string, installationID int64) (DerivedRepo, bool) {
	owner, repo, ok := splitOwnerRepo(fullName)
	if !ok {
		return DerivedRepo{}, false
	}
	return DerivedRepo{Repo: fullName, Owner: owner, RepoName: repo, InstallationID: installationID}, true
}

// derivedSetForPinned builds a DerivedRepoSet directly from filter (the
// operator's watched_repos) for a pinned-installation Reconciler
// (opts.AppInstallationID != 0) — see ADR-1233 Decision 4. This compat mode
// is deliberately exempt from R1's installation-derived inversion: the
// operator has explicitly asserted the pinned installation covers every
// watched repo, so there is nothing to discover, filter, or cap. Calling
// Derive again later (e.g. a re-derivation trigger, R2) against a pinned
// Reconciler is therefore a safe, cheap no-op that reproduces the same set,
// rather than requiring every re-derivation call site to special-case
// pinned mode itself.
func derivedSetForPinned(filter []string, pinnedInstallationID int64) DerivedRepoSet {
	var repos []DerivedRepo
	for _, spec := range filter {
		if dr, ok := derivedRepoFromFullName(spec, pinnedInstallationID); ok {
			repos = append(repos, dr)
		}
	}
	sortDerivedRepos(repos)
	return DerivedRepoSet{Repos: repos}
}

// neededOwnersFromFilter returns the set of lower-cased owners named
// anywhere in filter (watched_repos) — R3's account-granularity mint gate.
// Returns nil when filter is empty, which callers must treat as "no
// filter — every account is needed" (ADR-1641's "all-installations mode"),
// never as "the empty set — nothing is needed." A malformed filter entry
// (not "owner/repo") contributes nothing; it's already reported elsewhere
// (distinctOwnersLogging, DerivedRepoSet.FilteredOut) and must not silently
// widen or narrow this set.
func neededOwnersFromFilter(filter []string) map[string]bool {
	if len(filter) == 0 {
		return nil
	}
	needed := make(map[string]bool, len(filter))
	for _, spec := range filter {
		if owner, _, ok := splitOwnerRepo(spec); ok {
			needed[strings.ToLower(owner)] = true
		}
	}
	return needed
}

// resolveRecognizedAccounts computes R4/R5's "is this account one we
// recognize as ours" set from served (Options.ServedAccounts) and watched
// (Options.WatchedRepos), each case-insensitively. Three regimes:
//
//   - served is non-empty: recognized is exactly served's account set, and
//     explicit is true — an account outside it is a confirmed R4 deletion
//     candidate, never merely a soft report.
//   - served is empty but watched is non-empty: recognized is watched's
//     distinct owner set (R5's fallback signal — this is what makes AC3's
//     "still reportable with no allowlist configured" possible without a
//     third config key), explicit is false — an account outside it is
//     logged, never deleted.
//   - both are empty: hasSignal is false — every account is trusted (the
//     deliberate "all-installations mode" default) and recognized/explicit
//     are meaningless; callers must check hasSignal first.
func resolveRecognizedAccounts(served, watched []string) (recognized map[string]bool, hasSignal, explicit bool) {
	if len(served) > 0 {
		set := make(map[string]bool, len(served))
		for _, a := range served {
			set[strings.ToLower(a)] = true
		}
		return set, true, true
	}
	if len(watched) > 0 {
		return neededOwnersFromFilter(watched), true, false
	}
	return nil, false, false
}

func sortDerivedRepos(repos []DerivedRepo) {
	sort.Slice(repos, func(i, j int) bool {
		return strings.ToLower(repos[i].Repo) < strings.ToLower(repos[j].Repo)
	})
}

// Derive re-derives the effective repo set from the App's current
// installations — the R1 inversion this issue exists to make: installations
// are the desired state, and filter (the operator's optional watched_repos,
// R3) is an intersection applied on top, never a widening. Safe to call
// repeatedly (Reconcile's first call, and any later re-derivation trigger,
// R2): it always re-fetches every installation's accessible-repo list live
// from GitHub rather than trusting any cache, which is also what makes
// AC2/AC3's "future repos become pollable with no restart" requirement true
// for free — a newly created repo under an "all" or "selected" installation
// simply appears in the next call's FetchInstallationRepositories result.
//
// An owner already holding a client (from a prior Derive call) keeps that
// same *Auth/*gh.Client — Derive never re-mints a token for an installation
// it already knows about, only for one it's seeing for the first time (or
// re-seeing after a prior mint failure) — so a repo gained or lost under an
// already-known installation never disturbs that installation's running
// refresh loop. An owner whose installation has disappeared since the last
// call is detached (mirroring RemoveOwners) and returned as a DetachedAuth
// for the caller to drain-then-stop once it's confirmed safe (exactly the
// contract RemoveOwners' own doc comment already establishes) — Derive
// itself never stops a refresh loop, since a review dispatched before this
// call may still be holding a *gh.Client backed by it.
//
// maxRepos <= 0 means no cap (R5's max_derived_repos "unset/off" case).
//
// In pinned-installation mode (r.pinnedInstallationID != 0), this is a
// no-op wrapper around derivedSetForPinned — see that function's doc
// comment for why R1 doesn't apply there.
func (r *Reconciler) Derive(ctx context.Context, filter []string, maxRepos int, logf func(format string, args ...any)) (DerivedRepoSet, []DetachedAuth, error) {
	return r.derive(ctx, filter, maxRepos, logf, true)
}

// derive is Derive's implementation, additionally parameterized by
// startLoopsForNewOwners: true for every public Derive call (including
// every re-derivation trigger — installation webhook, the periodic ticker,
// a SIGHUP watched_repos edit), since nothing else will ever start a
// refresh loop for an owner one of those calls newly discovers. false only
// for Reconcile's own first call (see Reconcile's non-pinned discovery
// path): that caller's own contract (execute.go) is to call
// Reconciler.RunRefreshLoops itself immediately after Reconcile returns,
// covering every owner Reconcile just discovered — starting a loop here too
// would start a *second*, independent refresh-loop goroutine for the exact
// same *Auth. Auth.startRefreshLoop's cancel func is a single field, so the
// second call's cancel silently overwrites the first — Auth.Stop (and thus
// Pruefer's drainThenStopAuth/RemoveOwners cleanup, ADR-1640) could then
// only ever cancel the second loop, permanently leaking the first for the
// life of the process, alongside doubling every installation's token-mint
// API traffic. See TestReconcile_InitialDiscovery_DoesNotDoubleStartRefreshLoops.
func (r *Reconciler) derive(ctx context.Context, filter []string, maxRepos int, logf func(format string, args ...any), startLoopsForNewOwners bool) (DerivedRepoSet, []DetachedAuth, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	r.mu.Lock()
	pinnedID := r.pinnedInstallationID
	appID := r.appID
	privateKey := r.privateKey
	baseURL := r.baseURL
	botLogin := r.botLogin
	requiredPermissions := r.requiredPermissions
	servedAccounts := r.servedAccounts
	existingClients := make(map[string]*gh.Client, len(r.clients))
	for k, v := range r.clients {
		existingClients[k] = v
	}
	r.mu.Unlock()

	if pinnedID != 0 {
		set := derivedSetForPinned(filter, pinnedID)
		r.mu.Lock()
		r.lastDerived = set
		r.mu.Unlock()
		// #1709 R3: re-run on every re-derivation trigger, not just
		// Reconcile's own initial call — this is how a manual App-permission
		// raise in production gets verified without a restart, since a
		// pinned Reconciler never reaches the non-pinned loop below.
		verifyPinnedGrants(baseURL, appID, privateKey, pinnedID, requiredPermissions, logf)
		return set, nil, nil
	}

	jwt, err := gh.BuildAppJWT(appID, privateKey)
	if err != nil {
		return DerivedRepoSet{}, nil, fmt.Errorf("building app JWT: %w", err)
	}
	installations, instTruncated, err := gh.FetchAppInstallations(baseURL, jwt)
	if err != nil {
		return DerivedRepoSet{}, nil, fmt.Errorf("discovering app installations: %w", err)
	}

	byAccount := make(map[string]gh.AppInstallation, len(installations))
	for _, inst := range installations {
		byAccount[strings.ToLower(inst.Account)] = inst
	}

	// R3/R4/R5's account-level gates, resolved once per derive call: which
	// accounts are needed (named in watched_repos, when non-empty) and which
	// are recognized (served_accounts, falling back to watched_repos for a
	// softer report-only signal). See both helpers' doc comments.
	neededOwners := neededOwnersFromFilter(filter)
	recognized, hasAccountSignal, explicitAllowlist := resolveRecognizedAccounts(servedAccounts, filter)

	// A truncated installation list means an installation past the
	// pagination ceiling was never evaluated against either gate this round
	// — every installation actually *returned* is still enforced below
	// (the safer failure mode: a known-bad installation must never get an
	// indefinite pass merely because pagination truncated elsewhere in the
	// list), but the sweep itself may be incomplete.
	if instTruncated && hasAccountSignal {
		logf("! app installation enumeration hit a pagination ceiling while served_accounts/watched_repos enforcement is active — every installation returned above is still checked, but one beyond the ceiling was not evaluated this round and won't be recognized/removed until a future round enumerates it")
	}

	var allRepos []DerivedRepo
	var instSummaries []DerivedInstallation
	truncated := instTruncated
	newMintErrors := make(map[string]error)
	// wantOwners records every account this round actually wants a client
	// for — i.e. it passed both the R4 recognition gate and the R3
	// need gate. Used below to unify detachment: an owner still installed
	// (present in byAccount) but not in wantOwners loses its client exactly
	// like one whose installation disappeared entirely.
	wantOwners := make(map[string]bool, len(installations))

	for _, inst := range installations {
		key := strings.ToLower(inst.Account)
		unrecognized := hasAccountSignal && !recognized[key]

		if unrecognized && explicitAllowlist {
			logf("! installation %d (account %q) is not in the configured served_accounts allowlist — removing it (R4)", inst.ID, inst.Account)
			delErr := gh.DeleteAppInstallation(baseURL, jwt, inst.ID)
			summary := DerivedInstallation{
				Account: inst.Account, InstallationID: inst.ID,
				RepositorySelection: inst.RepositorySelection,
				Unrecognized:        true,
				DeletionAttempted:   true,
				// inst.Permissions comes from the already-fetched
				// installations list — still worth recording even for an
				// installation being removed, in case the deletion itself
				// fails and it lingers.
				PermissionShortfalls: checkGrantedPermissions(inst.Permissions, requiredPermissions),
			}
			switch {
			case delErr != nil && errors.Is(delErr, gh.ErrNotFound):
				logf("✓ installation %d (account %q) was already removed", inst.ID, inst.Account)
			case delErr != nil:
				logf("! removing installation %d (account %q) failed: %v — will retry on the next re-derivation; no token will be minted for it meanwhile", inst.ID, inst.Account, delErr)
				summary.DeletionError = delErr.Error()
			default:
				logf("✓ removed installation %d (account %q) — it was not in the configured served_accounts allowlist", inst.ID, inst.Account)
			}
			instSummaries = append(instSummaries, summary)
			continue
		}
		if unrecognized {
			logf("! installation %d (account %q, repository_selection=%s) is not named in watched_repos and no served_accounts allowlist is configured — reporting only, not removing; configure served_accounts to enable automatic removal (see cmd/pruefer/README.md)", inst.ID, inst.Account, inst.RepositorySelection)
		}

		if neededOwners != nil && !neededOwners[key] {
			// R3: this installation's account isn't named anywhere in
			// watched_repos — no token is minted for it (AC1). Note this is
			// the only branch reached for the softer watched_repos-fallback
			// "unrecognized" signal above (R5 with no served_accounts
			// configured): that fallback's recognized set is built from this
			// exact same filter, so an account unrecognized that way is, by
			// construction, always also not-needed here.
			instSummaries = append(instSummaries, DerivedInstallation{
				Account: inst.Account, InstallationID: inst.ID,
				RepositorySelection:    inst.RepositorySelection,
				NotServingWatchedRepos: true,
				Unrecognized:           unrecognized,
				PermissionShortfalls:   checkGrantedPermissions(inst.Permissions, requiredPermissions),
			})
			continue
		}

		wantOwners[key] = true

		client, ok := existingClients[key]
		if !ok {
			a, err := mintAuth(appID, inst.ID, botLogin, privateKey, baseURL)
			if err != nil {
				logf("! minting token for installation %d (account %q) failed: %v", inst.ID, inst.Account, err)
				newMintErrors[key] = err
				// Still recorded in instSummaries — this installation was
				// found, it just couldn't be minted this round. Omitting it
				// here would make guideMissingInstallations (which decides
				// "installed" purely from Installations membership) unable
				// to tell this apart from a genuinely uninstalled owner. See
				// MintError's own doc comment.
				instSummaries = append(instSummaries, DerivedInstallation{
					Account: inst.Account, InstallationID: inst.ID,
					RepositorySelection: inst.RepositorySelection,
					MintError:           err.Error(),
					// inst.Permissions comes from the already-fetched
					// installations list, independent of whether minting a
					// token succeeded this round — so a shortfall is still
					// computable (and worth surfacing) even when the
					// installation's client couldn't be minted.
					PermissionShortfalls: checkGrantedPermissions(inst.Permissions, requiredPermissions),
				})
				continue
			}
			client = r.registerOwnerAuth(inst.Account, a, true)
			if startLoopsForNewOwners {
				installLogf := func(format string, args ...any) {
					logf("installation %d: "+format, append([]any{inst.ID}, args...)...)
				}
				a.startRefreshLoop(ctx, installLogf, nil)
			}
		}

		repos, repoTruncated, err := gh.FetchInstallationRepositories(baseURL, client.Token())
		repoListErr := ""
		if err != nil {
			logf("! listing accessible repositories for installation %d (account %q) failed: %v", inst.ID, inst.Account, err)
			repoListErr = err.Error()
		}
		if repoTruncated {
			truncated = true
		}
		for _, full := range repos {
			if dr, ok := derivedRepoFromFullName(full, inst.ID); ok {
				allRepos = append(allRepos, dr)
			}
		}
		instSummaries = append(instSummaries, DerivedInstallation{
			Account: inst.Account, InstallationID: inst.ID,
			RepositorySelection: inst.RepositorySelection, RepoCount: len(repos),
			RepoListError:        repoListErr,
			PermissionShortfalls: checkGrantedPermissions(inst.Permissions, requiredPermissions),
		})
	}

	// Owners that had a client before this call lose it now if either: (a)
	// their installation no longer exists at all (the original mirror image
	// of the mint-new branch above), or (b) their installation still exists
	// but this round decided it's no longer wanted — removed via R4
	// (deleted or awaiting deletion), unrecognized-and-report-only via R5's
	// fallback, or no longer needed via R3's watched_repos narrowing.
	// Unified into one loop (rather than a separate pass per reason) since
	// the effect — drop the client, detach the Auth for the caller to
	// drain-then-stop — is identical regardless of which gate fired.
	// Detached, not stopped: see RemoveOwners' doc comment.
	var goneOwners []string
	for owner := range existingClients {
		if _, ok := byAccount[owner]; !ok {
			goneOwners = append(goneOwners, owner)
			continue
		}
		if !wantOwners[owner] {
			goneOwners = append(goneOwners, owner)
		}
	}
	detached := r.RemoveOwners(goneOwners)

	set := DerivedRepoSet{Truncated: truncated, Installations: instSummaries}

	if len(filter) > 0 {
		filterSet := make(map[string]bool, len(filter))
		for _, f := range filter {
			filterSet[strings.ToLower(f)] = true
		}
		granted := make(map[string]bool, len(allRepos))
		for _, dr := range allRepos {
			granted[strings.ToLower(dr.Repo)] = true
		}
		var kept []DerivedRepo
		for _, dr := range allRepos {
			if filterSet[strings.ToLower(dr.Repo)] {
				kept = append(kept, dr)
			}
		}
		for _, f := range filter {
			if !granted[strings.ToLower(f)] {
				set.FilteredOut = append(set.FilteredOut, f)
			}
		}
		allRepos = kept
	}

	sortDerivedRepos(allRepos)

	if maxRepos > 0 && len(allRepos) > maxRepos {
		set.Capped = true
		set.CapApplied = maxRepos
		set.PreCapCount = len(allRepos)
		logf("! derived repo set (%d repos) exceeds max_derived_repos=%d — capping to the first %d (sorted owner/repo); raise max_derived_repos, or narrow watched_repos, to review the rest", len(allRepos), maxRepos, maxRepos)
		allRepos = allRepos[:maxRepos]
	}

	set.Repos = allRepos

	r.mu.Lock()
	for k, v := range newMintErrors {
		r.mintErrors[k] = v
	}
	r.lastDerived = set
	r.mu.Unlock()

	return set, detached, nil
}

// logDerivedSet logs a DerivedRepoSet's contents for R4's "make the derived
// set observable" requirement — the failure mode this exists to prevent is a
// repo silently joining or leaving the review set with no record. Called
// once per Reconcile/Derive round (initial and every re-derivation trigger).
func logDerivedSet(set DerivedRepoSet, logf func(format string, args ...any)) {
	for _, inst := range set.Installations {
		// The first three branches below fire on the "UNRECOGNIZED-INSTALLATION"
		// marker — a fixed, grep-able tag reserved exclusively for this class
		// of event (R5/AC4), so it reads distinctly from the routine
		// enumeration lines every other branch below produces, no matter how
		// many hundreds of enumeration cycles pass between one appearance and
		// the next. Permission shortfalls are deliberately not logged for
		// these three (or for a just-deleted installation there is nothing
		// further to act on) — an account this deployment doesn't recognize
		// isn't a scope worth reporting granted-permission detail for.
		switch {
		case inst.DeletionAttempted && inst.DeletionError != "":
			logf("! UNRECOGNIZED-INSTALLATION: installation %d (%s) is outside the configured served_accounts allowlist — removing it failed this round (%s); will retry on the next re-derivation, no token minted meanwhile", inst.InstallationID, inst.Account, inst.DeletionError)
			continue
		case inst.DeletionAttempted:
			logf("! UNRECOGNIZED-INSTALLATION: installation %d (%s) was outside the configured served_accounts allowlist — removed", inst.InstallationID, inst.Account)
			continue
		case inst.Unrecognized:
			logf("! UNRECOGNIZED-INSTALLATION: installation %d (%s, repository_selection=%s) is not named in watched_repos and no served_accounts allowlist is configured — no token minted for it; configure served_accounts to enable automatic removal", inst.InstallationID, inst.Account, inst.RepositorySelection)
			continue
		case inst.NotServingWatchedRepos:
			logf("installation %d (%s, repository_selection=%s): not named in watched_repos — no token minted for it (R3)", inst.InstallationID, inst.Account, inst.RepositorySelection)
		case inst.MintError != "":
			logf("! installation %d (%s, repository_selection=%s): minting a token failed this round (%s) — the installation exists but is not yet usable; retry reconciliation", inst.InstallationID, inst.Account, inst.RepositorySelection, inst.MintError)
		case inst.RepoListError != "":
			logf("✓ installation %d (%s, repository_selection=%s): repo-access verification was skipped this round (listing failed — see error above); the installation is still authorized", inst.InstallationID, inst.Account, inst.RepositorySelection)
		default:
			logf("✓ installation %d (%s, repository_selection=%s): %d repo(s) accessible", inst.InstallationID, inst.Account, inst.RepositorySelection, inst.RepoCount)
		}
		// Permission shortfalls are logged regardless of which of the
		// remaining branches above fires: PermissionShortfalls is derived
		// from inst.Permissions (the installations-list response), never
		// from whether minting or repo-listing succeeded this round — a
		// genuine shortfall must not go unlogged just because it coincides
		// with an unrelated transient error (review finding on #1709's own
		// PR).
		logPermissionShortfalls(inst.InstallationID, inst.Account, inst.PermissionShortfalls, logf)
	}
	if len(set.Installations) == 0 {
		return
	}
	suffix := ""
	if set.Capped {
		suffix = fmt.Sprintf(" (capped from %d by max_derived_repos=%d)", set.PreCapCount, set.CapApplied)
	}
	if set.Truncated {
		suffix += " — WARNING: pagination ceiling was hit while enumerating installations/repos; the actual grant may be larger than shown"
	}
	logf("derived %d repo(s) to review across %d installation(s)%s", len(set.Repos), len(set.Installations), suffix)
	for _, f := range set.FilteredOut {
		logf("! watched_repos entry %q is not covered by any installation's grant — excluded", f)
	}
}

// guideMissingInstallations logs (and, at most once per Reconcile call,
// attempts to open a browser to) the App's guided-install URL for every
// distinct owner named in opts.WatchedRepos that set's installations don't
// cover — the same operator guidance the pre-#1641 discovery loop gave for a
// watched-but-uninstalled owner, preserved even though discovery itself is
// no longer driven by watched_repos. When opts.WatchedRepos is empty
// entirely (AC1's primary case — installations are the sole input) and no
// installation exists at all, a single "install the app somewhere" hint is
// logged instead, since there is no specific owner name to guide toward.
//
// The actual browser-open call goes through opts.OpenBrowser when non-nil
// (#1763) — it takes precedence over the package-level openBrowser var so an
// external caller's test can stub it without touching opts.NoBrowser, whose
// zero value is unsafe for this purpose (false = "attempt to open a
// browser").
func guideMissingInstallations(opts Options, slug string, set DerivedRepoSet, logf func(format string, args ...any)) {
	opener := openBrowser
	if opts.OpenBrowser != nil {
		opener = opts.OpenBrowser
	}
	installURL := fmt.Sprintf("https://github.com/apps/%s/installations/new", slug)

	if len(opts.WatchedRepos) == 0 {
		if len(set.Installations) == 0 {
			logf("no installations found for this App yet — install it on a repo, org, or account to begin reviewing: %s", installURL)
		}
		return
	}

	ownersInstalled := make(map[string]bool, len(set.Installations))
	for _, inst := range set.Installations {
		ownersInstalled[strings.ToLower(inst.Account)] = true
	}

	// openedInstallBrowser bounds guided-installation browser-opening to at
	// most once per call: watched_repos can plausibly span several owners
	// with no installation at all, and popping open a separate browser
	// tab/window per missing owner is surprising, unlike the single-flow
	// manifest bootstrap. Every missing owner still gets its install URL
	// logged; only the first one also gets an actual browser-open attempt.
	openedInstallBrowser := false
	for _, owner := range distinctOwnersLogging(opts.WatchedRepos, logf) {
		if ownersInstalled[strings.ToLower(owner)] {
			continue
		}
		notFoundDesc := "has no installation"
		if set.Truncated {
			notFoundDesc = "not found among the installations/repos that could be enumerated — the App may have more than could be listed (see the truncation warning above), so this could be a pagination gap rather than an actual missing installation; re-run reconciliation to confirm before assuming it needs installing"
		}
		if !opts.NoBrowser && !openedInstallBrowser {
			logf("! %s %s → opening %s …", owner, notFoundDesc, installURL)
			if err := opener(installURL); err != nil {
				logf("could not open browser automatically (%v) — visit the URL above manually", err)
			}
			openedInstallBrowser = true
		} else {
			logf("! %s %s → %s", owner, notFoundDesc, installURL)
		}
	}
}

// LastDerived returns the DerivedRepoSet from the most recent Derive call —
// including the one Reconcile itself performs at construction. Callers (e.g.
// Pruefer's execute.go, seeding the daemon's initial Clients/derived state
// right after Reconcile returns) use this to read that already-computed
// result without triggering a second, redundant live derivation. A later
// change to watched_repos (SIGHUP) does NOT read this — it triggers a fresh
// Derive call instead (see Pruefer's rederiveRepos/triggerRederivation),
// since every re-derivation trigger re-fetches installations/repos live
// rather than trusting a cached snapshot; this method is a one-time
// startup convenience, not a caching layer for the reload path.
func (r *Reconciler) LastDerived() DerivedRepoSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDerived
}
