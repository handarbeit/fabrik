package pruefer

import (
	"strings"
	"sync"
)

// prStamp is the set of inputs a memoised evaluation was made under, other
// than the PR's own head SHA. A memo entry is only honoured when every field
// still matches (#1952, ADR-1952):
//
//   - UpdatedAt: the PR's updated_at from the ListOpenPRs read that started
//     the evaluation. GitHub bumps it on pushes, comments (so a pending
//     "/pruefer review" is covered), reviews, label changes and draft/ready
//     transitions.
//   - OpGen: the daemon's operator-config generation, bumped by every
//     ApplyReload. Covers excluded_authors/labels/paths, max_diff_bytes,
//     cadence and repo_cadence — none of which move the PR.
//   - RepoCfg: a fingerprint of the repo-resident .pruefer/config.yaml at the
//     PR's base ref. That file changes when the base branch changes, not when
//     the PR does.
//
// The zero stamp (and any stamp with an empty field) means "unknown" and is
// never memoised nor matched — an unknown input always falls through to a
// real evaluation.
type prStamp struct {
	UpdatedAt string
	OpGen     uint64
	RepoCfg   string
}

// usable reports whether every component is known.
func (s prStamp) usable() bool {
	return s.UpdatedAt != "" && s.RepoCfg != ""
}

// prMemoKey identifies one PR; owner/repo are lowercased like reviewKey so
// lookups are case-insensitive.
type prMemoKey struct {
	owner, repo string
	number      int
}

type prMemoEntry struct {
	headSHA string
	stamp   prStamp
}

// prMemo remembers, per PR, the head SHA and prStamp at which the last
// evaluation reached a *conclusive* outcome (reviewed, or skipped for a
// reason). poll() consults it before dispatch: a PR whose head and stamp are
// unchanged needs no further calls this poll.
//
// It is in-memory and process-lifetime only — a cold start re-evaluates every
// PR once, exactly as before this existed — consistent with ADR-1113's "review
// state is derived from GitHub, never stored locally" and ADR-1631's
// precedent. Unlike ReviewTracker it is bounded: Prune drops PRs absent from a
// successful listing and RetainRepos drops repos that left the derived set.
//
// Nil-safe: a nil *prMemo never skips and records nothing, so a hand-built
// test Daemon{} literal behaves exactly as before.
type prMemo struct {
	mu      sync.Mutex
	entries map[prMemoKey]prMemoEntry
}

func newPRMemo() *prMemo {
	return &prMemo{entries: make(map[prMemoKey]prMemoEntry)}
}

func newPRMemoKey(owner, repo string, number int) prMemoKey {
	return prMemoKey{owner: strings.ToLower(owner), repo: strings.ToLower(repo), number: number}
}

// Skip reports whether the PR was already conclusively evaluated at exactly
// this head SHA and stamp.
func (m *prMemo) Skip(owner, repo string, number int, headSHA string, st prStamp) bool {
	if m == nil || headSHA == "" || !st.usable() {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[newPRMemoKey(owner, repo, number)]
	return ok && e.headSHA == headSHA && e.stamp == st
}

// Record memoises a conclusive evaluation. An unusable head or stamp is
// treated as Forget: the PR must be re-evaluated next poll.
func (m *prMemo) Record(owner, repo string, number int, headSHA string, st prStamp) {
	if m == nil {
		return
	}
	k := newPRMemoKey(owner, repo, number)
	m.mu.Lock()
	defer m.mu.Unlock()
	if headSHA == "" || !st.usable() {
		delete(m.entries, k)
		return
	}
	m.entries[k] = prMemoEntry{headSHA: headSHA, stamp: st}
}

// Forget drops the PR's entry (a non-conclusive evaluation must not leave an
// older conclusive one behind).
func (m *prMemo) Forget(owner, repo string, number int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, newPRMemoKey(owner, repo, number))
}

// Prune drops owner/repo's entries for PRs not in live. Call it only after a
// *successful* listing: absence from a failed read is not absence from GitHub.
func (m *prMemo) Prune(owner, repo string, live map[int]bool) {
	if m == nil {
		return
	}
	o, r := strings.ToLower(owner), strings.ToLower(repo)
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if k.owner == o && k.repo == r && !live[k.number] {
			delete(m.entries, k)
		}
	}
}

// RetainRepos drops every entry whose "owner/repo" (lowercased) is not in
// keep — repos that left the derived set.
func (m *prMemo) RetainRepos(keep map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.entries {
		if !keep[k.owner+"/"+k.repo] {
			delete(m.entries, k)
		}
	}
}

// Len reports the number of entries (tests, diagnostics).
func (m *prMemo) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
