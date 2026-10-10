package engine

import (
	"sort"
	"sync"

	"github.com/handarbeit/fabrik/internal/pathglob"
)

// Overlap-aware fresh batch composition (#2047, ADR-2047, docs/state-machine.md §6.32).
//
// admitByOverlap is the third fresh-formation filter in prepareTrainWorker, after the
// live-landed guard (#1871) and the CI admission gate (#1821). It keeps two members
// that change the same (non-ignored) path out of one batch, so a trial rarely needs
// conflict resolution or bisection. The deferred member simply stays Queued — no
// reroute, no comment, no counter, no pause — and is reconsidered by the next fresh
// formation. Like the CI gate it is reached only on fresh formation (both restart
// routes return from reconstructTrainState first); bisection, landOneAtATime and
// landGreenBatch never call it.
//
// Polarity is fail-OPEN, matching ADR-1821: a member whose changed files cannot be read
// (or cannot be known complete) is admitted exactly as before this filter existed. The
// post-landing invalidation scan (merge_train_invalidate.go) is the opposite — fail-safe
// — because rerouting a member is the costly direction there.

// overlapStarvationThreshold is how many consecutive fresh formations may defer one
// member for overlap before it is admitted first regardless of overlap (SC-003). A code
// constant, not a setting.
const overlapStarvationThreshold = 3

// prFilesTruncationLimit is the size at which GitHub's /pulls/{n}/files listing is
// silently capped. A list this long cannot be known to be complete, so it is treated
// as unreadable (fail-open) rather than as a trustworthy file set.
const prFilesTruncationLimit = 3000

// overlapCacheMax bounds the file-list cache. It is a safety net only: one entry is kept
// per PR, so growth is bounded by the number of distinct PRs the engine ever batches.
const overlapCacheMax = 2048

type overlapFileKey struct {
	repoKey string
	pr      int
}

type overlapFileEntry struct {
	headSHA string
	files   []string // raw, unfiltered; nil when incomplete
	// incomplete marks a list that could not be known complete (>= prFilesTruncationLimit).
	// It is cached so the oversized read is not repeated every formation.
	incomplete bool
}

// overlapState is the shared in-memory state of the overlap filter and of the
// post-landing scan's attribution. The zero value is ready to use. It is shared by every
// per-partition worker and the poll goroutine, so every access holds mu.
type overlapState struct {
	mu    sync.Mutex
	files map[overlapFileKey]overlapFileEntry // one entry per PR; a new head SHA overwrites
	// skips counts consecutive fresh formations that deferred a member, per train
	// partition (trainKey) then issue number. Reset when the member is admitted or is
	// absent from a formation's candidate list.
	skips    map[string]map[int]int
	disabled bool // test seam: SetMergeTrainOverlapDisabledForTest
}

// SetMergeTrainOverlapDisabledForTest makes admitByOverlap admit every member unchecked,
// neutralising the overlap filter (the FR-015 neutralisation seam).
func (e *Engine) SetMergeTrainOverlapDisabledForTest(disabled bool) {
	e.overlap.mu.Lock()
	defer e.overlap.mu.Unlock()
	e.overlap.disabled = disabled
}

// overlapIgnored reports whether path matches a configured overlap_ignore glob.
func (e *Engine) overlapIgnored(path string) bool {
	return pathglob.MatchAny(path, e.cfg.MergeTrainOverlapIgnore)
}

// prFiles returns the PR's changed files at member m's head SHA, reading them at most
// once per (repo, PR, head SHA). ok is false when the list is unreadable or cannot be
// known complete; a read error is never cached (so it is retried next formation) and
// never recorded as an empty list.
func (e *Engine) prFiles(owner, repo string, m trainMember) (files []string, ok bool) {
	key := overlapFileKey{repoKey: owner + "/" + repo, pr: m.prNum}

	e.overlap.mu.Lock()
	if ent, hit := e.overlap.files[key]; hit && ent.headSHA == m.headSHA {
		e.overlap.mu.Unlock()
		return ent.files, !ent.incomplete
	}
	e.overlap.mu.Unlock()

	// The live client, never the board cache: a fresh formation is about to commit to a
	// batch composition, and this is a REST read bounded by max_batch_size.
	got, err := e.client.FetchPRFiles(owner, repo, m.prNum)
	if err != nil {
		e.logf(m.item.Number, "merge-train", "overlap check: could not read changed files of PR #%d: %v — admitting #%d unchecked\n", m.prNum, err, m.item.Number)
		return nil, false
	}
	if len(got) == 0 {
		// nil,nil is the client's 404 shape; a PR that changes nothing is equally
		// uninformative. Either way there is nothing to compare.
		return nil, false
	}

	ent := overlapFileEntry{headSHA: m.headSHA, files: got}
	if len(got) >= prFilesTruncationLimit {
		ent = overlapFileEntry{headSHA: m.headSHA, incomplete: true}
	}
	e.overlap.mu.Lock()
	if e.overlap.files == nil || len(e.overlap.files) >= overlapCacheMax {
		e.overlap.files = make(map[overlapFileKey]overlapFileEntry)
	}
	e.overlap.files[key] = ent
	e.overlap.mu.Unlock()

	if ent.incomplete {
		e.logf(m.item.Number, "merge-train", "overlap check: PR #%d lists %d changed files (GitHub truncates at %d) — cannot be known complete, admitting #%d unchecked\n", m.prNum, len(got), prFilesTruncationLimit, m.item.Number)
		return nil, false
	}
	return got, true
}

// cachedPRFiles returns a PR's cached file list for its recorded head SHA without any
// read; used for attribution only. ok is false when nothing usable is cached.
func (e *Engine) cachedPRFiles(owner, repo string, prNum int) (files []string, ok bool) {
	e.overlap.mu.Lock()
	defer e.overlap.mu.Unlock()
	ent, hit := e.overlap.files[overlapFileKey{repoKey: owner + "/" + repo, pr: prNum}]
	if !hit || ent.incomplete {
		return nil, false
	}
	return ent.files, true
}

// admitByOverlap filters a freshly assembled batch down to members whose changed files
// are pairwise disjoint (FR-002), preserving today's deterministic order among the
// survivors. members arrives already ordered (#1833) and already capped.
//
// A member skipped overlapStarvationThreshold consecutive formations in a row is
// considered first regardless of overlap, so overlapping later candidates defer against
// it rather than the other way round (FR-003). The first considered member is always
// admitted, so overlap can never empty a batch.
func (e *Engine) admitByOverlap(trainKey, owner, repo string, members []trainMember) []trainMember {
	e.overlap.mu.Lock()
	disabled := e.overlap.disabled
	e.overlap.mu.Unlock()
	if disabled || len(members) == 0 {
		return members
	}

	// Consideration order: starved members first, otherwise today's order.
	e.overlap.mu.Lock()
	partSkips := e.overlap.skips[trainKey]
	e.overlap.mu.Unlock()
	order := make([]int, 0, len(members))
	for i, m := range members {
		if partSkips[m.item.Number] >= overlapStarvationThreshold {
			order = append(order, i)
		}
	}
	starved := len(order)
	for i, m := range members {
		if partSkips[m.item.Number] < overlapStarvationThreshold {
			order = append(order, i)
		}
	}

	keep := make([]bool, len(members))
	claimed := map[string]int{} // path -> number of the admitted member that changes it
	for idx, i := range order {
		m := members[i]
		files, ok := e.prFiles(owner, repo, m)
		if !ok {
			keep[i] = true // fail-open; its files stay out of the union
			continue
		}

		var set []string
		for _, f := range files {
			if !e.overlapIgnored(f) {
				set = append(set, f)
			}
		}
		sort.Strings(set) // deterministic "an overlapping path" in the log line

		blocker, shared := 0, ""
		for _, f := range set {
			if n, hit := claimed[f]; hit {
				blocker, shared = n, f
				break
			}
		}
		if blocker != 0 {
			e.logf(m.item.Number, "merge-train", "deferred #%d: overlaps #%d on %s\n", m.item.Number, blocker, shared)
			continue
		}

		keep[i] = true
		for _, f := range set {
			claimed[f] = m.item.Number
		}
		if idx < starved {
			e.logf(m.item.Number, "merge-train", "admitting #%d first: deferred for overlap in %d consecutive formation(s) (starvation guard)\n", m.item.Number, overlapStarvationThreshold)
		}
	}

	out := make([]trainMember, 0, len(members))
	e.overlap.mu.Lock()
	next := make(map[int]int, len(members))
	for i, m := range members {
		if keep[i] {
			out = append(out, m)
			continue // admitted: count resets (absent from next)
		}
		next[m.item.Number] = partSkips[m.item.Number] + 1
	}
	// Members absent from this formation's candidate list have left the batch: their
	// counts are dropped too, by replacing the partition's map wholesale.
	if e.overlap.skips == nil {
		e.overlap.skips = make(map[string]map[int]int)
	}
	if len(next) == 0 {
		delete(e.overlap.skips, trainKey)
	} else {
		e.overlap.skips[trainKey] = next
	}
	e.overlap.mu.Unlock()
	return out
}
