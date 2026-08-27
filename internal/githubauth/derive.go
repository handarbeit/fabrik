package githubauth

import (
	"context"
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
	if logf == nil {
		logf = func(string, ...any) {}
	}

	r.mu.Lock()
	pinnedID := r.pinnedInstallationID
	appID := r.appID
	privateKey := r.privateKey
	baseURL := r.baseURL
	botLogin := r.botLogin
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

	var allRepos []DerivedRepo
	var instSummaries []DerivedInstallation
	truncated := instTruncated
	newMintErrors := make(map[string]error)

	for _, inst := range installations {
		key := strings.ToLower(inst.Account)
		client, ok := existingClients[key]
		if !ok {
			a, err := mintAuth(appID, inst.ID, botLogin, privateKey, baseURL)
			if err != nil {
				logf("! minting token for installation %d (account %q) failed: %v", inst.ID, inst.Account, err)
				newMintErrors[key] = err
				continue
			}
			installLogf := func(format string, args ...any) {
				logf("installation %d: "+format, append([]any{inst.ID}, args...)...)
			}
			client = r.CommitOwnerAuth(ctx, inst.Account, a, true, installLogf)
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
			RepoListError: repoListErr,
		})
	}

	// Owners that had a client before this call but no longer have a
	// matching installation lose it now — the mirror image of the mint-new
	// branch above. Detached, not stopped: see RemoveOwners' doc comment.
	var goneOwners []string
	for owner := range existingClients {
		if _, ok := byAccount[owner]; !ok {
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
		if inst.RepoListError != "" {
			logf("✓ installation %d (%s, repository_selection=%s): repo-access verification was skipped this round (listing failed — see error above); the installation is still authorized", inst.InstallationID, inst.Account, inst.RepositorySelection)
			continue
		}
		logf("✓ installation %d (%s, repository_selection=%s): %d repo(s) accessible", inst.InstallationID, inst.Account, inst.RepositorySelection, inst.RepoCount)
	}
	if len(set.Installations) == 0 {
		return
	}
	suffix := ""
	if set.Capped {
		suffix = fmt.Sprintf(" (capped from %d by max_derived_repos=%d)", set.TotalGranted(), set.CapApplied)
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
func guideMissingInstallations(opts Options, slug string, set DerivedRepoSet, logf func(format string, args ...any)) {
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
			if err := openBrowser(installURL); err != nil {
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
// Pruefer's SIGHUP watched_repos reload) use this to re-intersect a changed
// filter against the already-known installation grant without triggering a
// fresh, live re-derivation — see this issue's "SIGHUP no longer mints"
// consequence.
func (r *Reconciler) LastDerived() DerivedRepoSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDerived
}
