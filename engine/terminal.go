package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

func (e *Engine) runProbeAndDeepFetch(cacheImpl *boardcache.CacheImpl) {
	probeItems, _, err := e.client.ProbeProjectBoard(e.cfg.Owner, e.cfg.Repo, e.cfg.ProjectNum, e.cfg.OwnerType)
	if err != nil {
		_, graphqlStats := e.client.RateLimitStats()
		if graphqlStats.Limit > 0 && (graphqlStats.Remaining == 0 || float64(graphqlStats.Remaining)/float64(graphqlStats.Limit) < rateLimitBackoffThreshold) {
			retryStr := "retrying soon"
			if !graphqlStats.Reset.IsZero() && graphqlStats.Reset.After(time.Now()) {
				retryStr = fmt.Sprintf("retrying after %s (local)", graphqlStats.Reset.Local().Format("15:04"))
			}
			e.logf(0, "warn", "rate limited — polling suspended, %s: %v\n", retryStr, err)
			e.emitStructural(tui.RateLimitAlertEvent{Exhausted: true, Reset: graphqlStats.Reset})
		} else {
			e.logf(0, "cache", "probe refresh failed (using prior cache state): %v\n", err)
		}
		return
	}

	// Guard: 0 probe items with a populated cache indicates a transient indexer
	// hiccup rather than a genuine board wipe. Skip this cycle; retry on the next
	// poll. (probeProjectBoardOnce already retries 3x, but a degraded response can
	// still slip through.)
	if len(probeItems) == 0 && cacheImpl.IsBootstrapped() {
		e.logf(0, "cache", "probe returned 0 items while cache has data — skipping (transient indexer hiccup)\n")
		return
	}

	newKeys := make(map[string]bool, len(probeItems))
	var deepFetched int

	for _, pi := range probeItems {
		repo := pi.Repo
		if repo == "" {
			repo = e.defaultRepo()
		}
		newKeys[fmt.Sprintf("%s#%d", repo, pi.Number)] = true

		// Stage-membership guard: whether the item's column has a matching Fabrik stage.
		// The guard must come after newKeys[key]=true so unconfigured items are not
		// falsely evicted from the store by the post-loop tombstoning pass (lines below).
		configuredStage := stages.FindStage(e.cfg.Stages, pi.Status) != nil

		snap, snapErr := e.store.Get(repo, pi.Number)
		if snapErr != nil {
			// New item on board — skip entirely if not in a configured stage.
			if !configuredStage {
				continue
			}
			// Seed minimal state into the store.
			minimal := gh.ProjectItem{
				ID:             pi.ContentID,
				ItemID:         pi.ItemID,
				Number:         pi.Number,
				IsPR:           pi.IsPR,
				IsClosed:       pi.IsClosed,
				Status:         pi.Status,
				Repo:           repo,
				UpdatedAt:      pi.EffectiveUpdatedAt,
				LinkedPRNumber: pi.LinkedPRNumber,
			}
			e.store.Apply(itemstate.IssueOpened{Item: minimal})
			// Probe-only terminal short-circuit: closed items in a cleanup stage whose
			// worktree is absent have no remaining Fabrik work; skip the deep-fetch.
			// isProbeOnlyTerminal logs the worktree-presence decision internally (FR-6).
			if e.isProbeOnlyTerminal(minimal) {
				e.store.Apply(itemstate.TerminalFlagSet{Repo: repo, Number: pi.Number, Terminal: true})
				e.logf(pi.Number, "cache", "probe: new item seeded terminal — skipping deep-fetch\n")
				continue
			}
			e.logf(pi.Number, "cache", "probe: new item discovered — deep-fetching\n")
			if fetchErr := e.readClient.FetchItemDetails(&minimal); fetchErr != nil {
				e.logf(pi.Number, "warn", "probe: deep-fetch for new item failed: %v\n", fetchErr)
				e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: pi.Number, At: time.Now()})
			} else {
				deepFetched++
			}
			continue
		}

		// Detect linkage drift: probe found a different linked PR than the cache holds.
		s := snap.State()
		cachedPRNum := 0
		if s.LinkedPR != nil {
			cachedPRNum = s.LinkedPR.Number
		}
		if pi.LinkedPRNumber != cachedPRNum {
			if s.LastDeepFetchAt.IsZero() {
				// Never deep-fetched: no prior deep cache to invalidate.
				// Treat the probe's value as authoritative — write it into LinkedPR.Number
				// and update the prToKey reverse index without firing DeepFetchInvalidated.
				// This suppresses spurious invalidations that occur when the bootstrapped
				// LinkedPR.Number differs from the probe's value (e.g., cold-start with
				// the old FetchProjectBoard path that did not populate LinkedPR.Number).
				// Apply even when pi.LinkedPRNumber==0 to clear a stale prToKey entry
				// if the PR was delinked between bootstrap and the first probe cycle.
				e.store.Apply(itemstate.PRDetailsUpdated{
					Repo:     repo,
					Number:   pi.Number,
					PRNumber: pi.LinkedPRNumber,
				})
			} else {
				// Warm cache (has been deep-fetched): real linkage drift — invalidate.
				e.logf(pi.Number, "cache", "probe: linkage drift (was PR #%d, now PR #%d) — invalidating deep cache\n",
					cachedPRNum, pi.LinkedPRNumber)
				e.store.Apply(itemstate.DeepFetchInvalidated{Repo: repo, Number: pi.Number})
			}
		}

		// Terminal skip: if this item was previously identified as terminal and the
		// probe still shows it in the same cleanup stage, skip deep-fetch entirely —
		// external activity on a closed Done item has no bearing on Fabrik's work.
		// Must run BEFORE ProbeBoardItemUpdated so we read the cached Terminal flag;
		// applyProbeItem would clear it on status change before we could react to pi.Status.
		// pi.Status == s.Status guards against items that moved between two cleanup stages:
		// in that case we must apply the probe update so the store reflects the new status.
		if s.Terminal {
			if pst := stages.FindStage(e.cfg.Stages, pi.Status); pst != nil && pst.CleanupWorktree && pi.Status == s.Status {
				continue // still terminal in the same cleanup stage — no deep-fetch needed
			}
			// Status changed (left the cleanup stage or moved to a different one) — clear
			// the flag and fall through to normal probe processing.
			e.store.Apply(itemstate.TerminalFlagSet{Repo: repo, Number: pi.Number, Terminal: false})
			e.logf(pi.Number, "poll", "terminal flag cleared (status drifted to %q)\n", pi.Status)
		}

		// Apply probe state (updates IsClosed, State, IsPR, Status, UpdatedAt;
		// intentionally skips Labels to preserve webhook-driven label state).
		e.store.Apply(itemstate.ProbeBoardItemUpdated{Repo: repo, Number: pi.Number, Item: pi})

		// Unconditionally write the probe's head SHA whenever present — the probe
		// response is always authoritative, including for cache-fresh items that
		// skip the deep-fetch below. This is the primary poll-mode path for keeping
		// HeadSHA populated without relying solely on the REST FetchLinkedPR fallback.
		if pi.LinkedPRHeadSHA != "" && pi.LinkedPRNumber != 0 {
			e.store.Apply(itemstate.PRHeadSHAUpdated{
				Repo:        repo,
				Number:      pi.Number,
				LinkedPRNum: pi.LinkedPRNumber,
				SHA:         pi.LinkedPRHeadSHA,
			})
		}

		// Existing item in unconfigured column: probe state updated above (keeps
		// Status current for TUI and itemMayNeedWork), but deep-fetch is wasted work.
		if !configuredStage {
			continue
		}

		// Deep-fetch when cache is stale relative to effectiveUpdatedAt.
		if cacheImpl.IsItemCacheFresh(repo, pi.Number, pi.EffectiveUpdatedAt) {
			continue
		}
		e.logf(pi.Number, "cache", "probe: stale — deep-fetching\n")
		minimal := gh.ProjectItem{
			ID:        pi.ContentID,
			ItemID:    pi.ItemID,
			Number:    pi.Number,
			IsPR:      pi.IsPR,
			IsClosed:  pi.IsClosed,
			Status:    pi.Status,
			Repo:      repo,
			UpdatedAt: pi.EffectiveUpdatedAt,
		}
		if fetchErr := e.readClient.FetchItemDetails(&minimal); fetchErr != nil {
			e.logf(pi.Number, "warn", "probe: deep-fetch for stale item failed: %v\n", fetchErr)
			e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: pi.Number, At: time.Now()})
			continue
		}
		deepFetched++
		// After a successful deep-fetch, check if this item is now terminal.
		if isTerminalPredicate(minimal.Labels, minimal.Status, e.cfg.Stages) {
			if !s.Terminal {
				e.logf(pi.Number, "poll", "terminal flag set\n")
			}
			e.store.Apply(itemstate.TerminalFlagSet{Repo: repo, Number: pi.Number, Terminal: true})
		}
	}

	// Remove items no longer on the board.
	for _, snap := range e.store.All() {
		key := fmt.Sprintf("%s#%d", snap.Repo(), snap.Number())
		if !newKeys[key] {
			e.logf(snap.Number(), "cache", "probe: item gone from board — removing from store\n")
			e.store.Remove(snap.Repo(), snap.Number())
		}
	}

	if deepFetched > 0 {
		e.logf(0, "cache", "probe: deep-fetched %d item(s)\n", deepFetched)
	}
}

// isTerminalPredicate reports whether an item with the given labels and board
// status qualifies as terminal: it must be in a cleanup (Done) stage, carry the
// stage:Name:complete label, and have no transient lifecycle or lock labels.
// Terminal items have no remaining Fabrik work and may safely skip deep-fetch.
func isTerminalPredicate(labels []string, status string, stagesCfg []*stages.Stage) bool {
	st := stages.FindStage(stagesCfg, status)
	if st == nil || !st.CleanupWorktree {
		return false
	}
	completeLabel := "stage:" + st.Name + ":complete"
	hasComplete := false
	for _, l := range labels {
		if l == completeLabel {
			hasComplete = true
			break
		}
	}
	if !hasComplete {
		return false
	}
	for _, l := range labels {
		for _, tl := range transientLifecycleLabels {
			if l == tl {
				return false
			}
		}
		if strings.HasPrefix(l, "fabrik:locked:") {
			return false
		}
	}
	return true
}

// isProbeOnlyTerminal reports whether an item is terminal based on probe data
// (IsClosed + CleanupWorktree stage) and confirms the on-disk worktree is absent.
// Use this predicate in the new-item branch of runProbeAndDeepFetch and in
// seedTerminalFromProbeItems, where labels have not yet been fetched.
//
// The worktree check prevents stranding cleanup work: if the worktree still
// exists on disk (cleanup_worktree stage not yet run), the item must proceed
// through processItem so the Done stage cleanup can execute.
func (e *Engine) isProbeOnlyTerminal(item gh.ProjectItem) bool {
	if !item.IsClosed {
		return false
	}
	st := stages.FindStage(e.cfg.Stages, item.Status)
	if st == nil || !st.CleanupWorktree {
		return false
	}
	if e.worktreeExistsForItem(item) {
		e.logf(item.Number, "cache", "probe: worktree present — not treating as terminal yet\n")
		return false
	}
	e.logf(item.Number, "cache", "probe: no worktree on disk — treating as terminal\n")
	return true
}

// seedTerminalFromProbeItems applies TerminalFlagSet for probe items that
// qualify as terminal: closed, in a cleanup stage, and worktree absent on disk.
// Must be called after BootstrapFromProbe so items are in the store.
func (e *Engine) seedTerminalFromProbeItems(items []gh.BoardProbeItem) {
	var seeded int
	for _, pi := range items {
		stub := gh.ProjectItem{
			Number:   pi.Number,
			IsClosed: pi.IsClosed,
			Status:   pi.Status,
			Repo:     pi.Repo,
		}
		if e.isProbeOnlyTerminal(stub) {
			e.store.Apply(itemstate.TerminalFlagSet{
				Repo:     pi.Repo,
				Number:   pi.Number,
				Terminal: true,
			})
			seeded++
		}
	}
	if seeded > 0 {
		e.logf(0, "cache", "probe bootstrap: seeded %d terminal items (worktree absent, closed cleanup stage)\n", seeded)
	}
}

// runStartupTerminalScan marks terminal items in the Store after the first
// successful poll using the full label-aware predicate (isTerminalPredicate).
// Must run after runStartupTransientLabelScan so stale transient labels have
// been removed before the predicate is evaluated.
//
// Label availability note: when the cache was bootstrapped via BootstrapFromProbe
// (the default cold-start path), labels are absent from the Store. In that case
// this scan is a no-op — isTerminalPredicate returns false for every item.
// Cold-start terminal seeding is instead handled by seedTerminalFromProbeItems
// (called after BootstrapFromProbe) using IsClosed+CleanupWorktree+worktree-absent.
// When bootstrapped via the full FetchProjectBoard path, labels are present
// and this scan applies the full predicate correctly.
func (e *Engine) runStartupTerminalScan() {
	snaps := e.store.All()
	var marked int
	for _, snap := range snaps {
		if snap.IsTerminal() {
			continue // already set — no-op path
		}
		if isTerminalPredicate(snap.Labels(), snap.Status(), e.cfg.Stages) {
			e.store.Apply(itemstate.TerminalFlagSet{
				Repo:     snap.Repo(),
				Number:   snap.Number(),
				Terminal: true,
			})
			marked++
		}
	}
	if marked > 0 {
		e.logf(0, "startup", "terminal scan: marked %d item(s) as terminal\n", marked)
	}
}
