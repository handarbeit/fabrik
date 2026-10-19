package engine

import (
	"strconv"
	"strings"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// localWriter is the write-through surface the engine uses after a successful
// GitHub write: the label, close and comment mirrors of CacheImpl. Production
// engines have the board cache and use it as is (it also moves the freshness
// baseline). An engine with no cache (the bare-adapter shape NewWithDeps builds)
// had no write-through at all, so its own pause or label write never reached the
// Store and any decision made from the Store missed it (#2350, ADR 2350). For
// that engine localWriter applies the same mutations straight to the Store.
type localWriter interface {
	ApplyLabelAdded(key, label string)
	ApplyLabelRemoved(key, label string)
	ApplyLabelAddedBy(key, label, requester string)
	ApplyLabelRemovedBy(key, label, requester string)
	ApplyIssueClosed(key string)
	ApplyCommentAdded(key string, comment gh.Comment)
}

var _ localWriter = (*boardcache.CacheImpl)(nil)

// localWriter returns the cache when one is wired, else a Store-direct writer.
// It is never nil, so a call site needs no cache check.
func (e *Engine) localWriter() localWriter {
	if c := e.cache(); c != nil {
		return c
	}
	return storeWriter{store: e.store, off: e.cachelessWriteThroughOff.Load()}
}

// SetCachelessWriteThroughDisabledForTest restores the pre-#2350 behaviour for an
// engine with no board cache: its label, close and comment writes do not reach
// the Store. Test seam only (tests/sim), used to show a scenario is not vacuous.
func (e *Engine) SetCachelessWriteThroughDisabledForTest(off bool) {
	e.cachelessWriteThroughOff.Store(off)
}

// storeWriter mirrors CacheImpl's write-through for an engine with no cache. Like
// the cache it never creates a record for an item the Store has not seen.
type storeWriter struct {
	store *itemstate.Store
	off   bool
}

func splitItemKey(key string) (repo string, number int, ok bool) {
	i := strings.LastIndex(key, "#")
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return "", 0, false
	}
	return key[:i], n, true
}

func (w storeWriter) apply(key string, build func(repo string, number int) itemstate.Mutation) {
	if w.off || w.store == nil {
		return
	}
	repo, number, ok := splitItemKey(key)
	if !ok {
		return
	}
	if _, found := w.store.Peek(repo, number); !found {
		return
	}
	w.store.Apply(build(repo, number))
}

func (w storeWriter) ApplyLabelAdded(key, label string) { w.ApplyLabelAddedBy(key, label, "") }
func (w storeWriter) ApplyLabelAddedBy(key, label, requester string) {
	if !cachelessMirroredLabel(label) && !isStageCompleteLabel(label) {
		return
	}
	w.apply(key, func(repo string, n int) itemstate.Mutation {
		return itemstate.LocalLabelAdded{Repo: repo, Number: n, Label: label, Requester: requester}
	})
}
func (w storeWriter) ApplyLabelRemoved(key, label string) { w.ApplyLabelRemovedBy(key, label, "") }
func (w storeWriter) ApplyLabelRemovedBy(key, label, requester string) {
	if !cachelessMirroredLabel(label) && !isStageCompleteLabel(label) {
		return
	}
	w.apply(key, func(repo string, n int) itemstate.Mutation {
		return itemstate.LocalLabelRemoved{Repo: repo, Number: n, Label: label, Requester: requester}
	})
}
func (w storeWriter) ApplyIssueClosed(key string) {
	w.apply(key, func(repo string, n int) itemstate.Mutation {
		return itemstate.LocalIssueClosed{Repo: repo, Number: n}
	})
}
func (w storeWriter) ApplyCommentAdded(key string, comment gh.Comment) {
	w.apply(key, func(repo string, n int) itemstate.Mutation {
		return itemstate.LocalCommentAdded{Repo: repo, Number: n, Comment: comment}
	})
}

// isStageCompleteLabel reports whether label is a stage completion label
// (stage:<name>:complete). Both its removal and its add are mirrored into a
// cacheless Store: a phase of the poll that strips it (handleValidateSHAInvalidation,
// the rework bracket) must be seen by the dispatch re-read (FR-003), and the add that
// restores it must be seen too, or the Store would keep reading the stage as
// incomplete and the item would be re-dispatched.
func isStageCompleteLabel(label string) bool {
	return strings.HasPrefix(label, "stage:") && strings.HasSuffix(label, ":complete")
}

// cachelessMirroredLabel reports whether a label write reaches the Store of an
// engine with no board cache. Only the labels that decide whether an item still
// needs work are mirrored: the pause pair, the dependency marker and the
// revalidation trigger, plus stage completion labels (isStageCompleteLabel). The
// gate labels (awaiting-ci, awaiting-review) keep reaching the Store at the next
// poll's listing, as they did before; mirroring them at the instant of the write
// exposes transient intermediate label states to the channel deriver that a
// poll-start listing never showed (#2350).
func cachelessMirroredLabel(label string) bool {
	switch label {
	case "fabrik:paused", "fabrik:awaiting-input", "fabrik:blocked", "fabrik:revalidate":
		return true
	}
	return false
}
