package itemstate

import (
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// DefaultEngineWriteGrace is how long after an engine write a board listing
// fetch may still be treated as not having seen it (#2159). It covers GitHub's
// own board-listing lag (measured by the #1974 probe), not just a fetch that
// was in flight when the write happened. A reconcile tick is minutes apart, so
// a real external change inside the window is corrected on the next tick.
const DefaultEngineWriteGrace = 30 * time.Second

const (
	engineWriteStatusKey = "status"
	engineWriteClosedKey = "closed"
	engineWriteLabelPfx  = "label:"
)

func engineWriteLabelKey(label string) string { return engineWriteLabelPfx + label }

// EngineWriteLabelKey is the ItemState.EngineWriteAt key under which the engine's
// write of label is recorded, for callers that need to ask "did the engine itself
// write this label?" (shutdown lock cleanup, #2159).
func EngineWriteLabelKey(label string) string { return engineWriteLabelKey(label) }

// recordEngineWrite stamps key as written by the engine at now and prunes
// entries that can no longer protect anything. Reports whether the map changed
// (it always does for the stamp, so callers use it only for the flag).
func recordEngineWrite(item *ItemState, key string, now time.Time, grace time.Duration) {
	pruneEngineWrites(item, now, grace)
	if item.EngineWriteAt == nil {
		item.EngineWriteAt = make(map[string]time.Time)
	}
	item.EngineWriteAt[key] = now
}

// pruneEngineWrites drops entries whose grace period has elapsed: any listing
// fetched from now on starts after write+grace and so wins. The map is set back
// to nil when empty so equality checks against an untouched item stay stable.
func pruneEngineWrites(item *ItemState, now time.Time, grace time.Duration) bool {
	changed := false
	for k, w := range item.EngineWriteAt {
		if !now.Before(w.Add(grace)) {
			delete(item.EngineWriteAt, k)
			changed = true
		}
	}
	if len(item.EngineWriteAt) == 0 {
		item.EngineWriteAt = nil
	}
	return changed
}

// shallowView is the effective result of applying a listing item to an item
// under the engine-write protection rule.
type shallowView struct {
	// Labels is the label set after protection; nil means "use the listing's".
	Labels []string
	// Status is the status after protection.
	Status string
	// KeepClosed means the local closed state is kept over the listing's.
	KeepClosed bool
	// Active reports that at least one engine write still shields this item
	// from a listing fetched at fetchedAt.
	Active bool
	// Drop lists EngineWriteAt keys to delete: confirmed by this listing or
	// expired.
	Drop []string
}

// engineWriteProtected reports whether key's record at W shields a listing
// fetched at fetchedAt: fetchedAt < W+grace.
func engineWriteProtected(item *ItemState, key string, fetchedAt time.Time, grace time.Duration) bool {
	w := recordedAt(item, key)
	return !w.IsZero() && fetchedAt.Before(w.Add(grace))
}

// engineWriteConfirm marks key for dropping once a listing fetched at or after
// its write time W is seen to agree with the local value.
func engineWriteConfirm(item *ItemState, v *shallowView, key string, fetchedAt time.Time) {
	if w, ok := item.EngineWriteAt[key]; ok && !fetchedAt.Before(w) {
		v.Drop = append(v.Drop, key)
	}
}

// protectStatusClosed decides whether the engine's own recent write of Status or
// closed state wins over a listing's (status, isClosed), fetched at fetchedAt. It
// is the single rule shared by every listing apply path: the reconcile apply
// (through protectShallow), drift detection (Store.EffectiveShallow) and the
// probe apply (applyProbeItem, #2178), so they can never disagree. It never looks
// at labels, so a listing that carries none (the probe) cannot confirm and drop a
// label record.
//
// A field is protected when its record at W satisfies fetchedAt < W+grace: the
// listing may predate the write (in flight during it, or lagging behind it). The
// local value — "what the cache says now", not the engine's intended value, so a
// later webhook change is respected — is kept. A listing fetched at or after
// W+grace is trusted and wins, as does any field with no record or any listing
// with a zero fetchedAt. A record is confirmed (dropped) once a listing fetched
// at or after W agrees with the local value.
func protectStatusClosed(item *ItemState, status string, isClosed bool, fetchedAt time.Time, grace time.Duration, now time.Time) shallowView {
	v := shallowView{Status: status}
	for k, w := range item.EngineWriteAt {
		if !now.Before(w.Add(grace)) {
			v.Drop = append(v.Drop, k)
		}
	}
	if fetchedAt.IsZero() || !hasRecords(item) {
		return v
	}
	v.Active = anyRecordActive(item, fetchedAt, grace)

	if engineWriteProtected(item, engineWriteStatusKey, fetchedAt, grace) && status != item.Status {
		v.Status = item.Status
	} else if status == item.Status {
		engineWriteConfirm(item, &v, engineWriteStatusKey, fetchedAt)
	}

	// Closed state only ever moves one way through the engine (LocalIssueClosed),
	// so only a locally-closed item shielded against an open listing is kept.
	if engineWriteProtected(item, engineWriteClosedKey, fetchedAt, grace) && item.IsClosed && !isClosed {
		v.KeepClosed = true
	} else if item.IsClosed == isClosed {
		engineWriteConfirm(item, &v, engineWriteClosedKey, fetchedAt)
	}
	return v
}

// protectLabels merges pi's labels with the engine's recent label writes into v
// (Labels, plus Drop for confirmed records), per label: local presence wins where
// protected. Only listings that carry label data may call it.
func protectLabels(item *ItemState, pi gh.ProjectItem, fetchedAt time.Time, grace time.Duration, v *shallowView) {
	inListing := make(map[string]bool, len(pi.Labels))
	for _, l := range pi.Labels {
		inListing[l] = true
	}
	inLocal := make(map[string]bool, len(item.Labels))
	for _, l := range item.Labels {
		inLocal[l] = true
	}
	kept := false
	// A full label set older than the one the Store last applied (the baseline
	// every label-carrying listing or live read records) is ignored as a whole:
	// the local set stands (#2348).
	if base := item.ObservedAt[observedLabelsKey]; !base.IsZero() && fetchedAt.Before(base.Add(grace)) && !sameStringSet(item.Labels, pi.Labels) {
		v.Labels = append([]string(nil), item.Labels...)
		return
	}
	merged := make([]string, 0, len(pi.Labels)+len(item.Labels))
	for _, l := range pi.Labels {
		if !inLocal[l] && engineWriteProtected(item, engineWriteLabelKey(l), fetchedAt, grace) {
			kept = true // engine removed it; the listing predates the removal
			continue
		}
		merged = append(merged, l)
	}
	for _, l := range item.Labels {
		if !inListing[l] && engineWriteProtected(item, engineWriteLabelKey(l), fetchedAt, grace) {
			kept = true // engine added it; the listing predates the add
			merged = append(merged, l)
		}
	}
	if kept {
		v.Labels = merged
	}
	for k := range item.EngineWriteAt {
		if len(k) > len(engineWriteLabelPfx) && k[:len(engineWriteLabelPfx)] == engineWriteLabelPfx {
			l := k[len(engineWriteLabelPfx):]
			if inListing[l] == inLocal[l] {
				engineWriteConfirm(item, v, k, fetchedAt)
			}
		}
	}
}

// protectShallow decides, per field, whether the engine's own recent write wins
// over the full listing pi (fetched at fetchedAt): Status and closed state via
// protectStatusClosed, labels via protectLabels. It is the rule shared by the
// apply step (applyShallowItem) and drift detection (Store.EffectiveShallow) so
// the two can never disagree.
func protectShallow(item *ItemState, pi gh.ProjectItem, fetchedAt time.Time, grace time.Duration, now time.Time) shallowView {
	v := protectStatusClosed(item, pi.Status, pi.IsClosed, fetchedAt, grace, now)
	if fetchedAt.IsZero() || !hasRecords(item) {
		return v
	}
	protectLabels(item, pi, fetchedAt, grace, &v)
	return v
}

// dropEngineWrites deletes keys from item.EngineWriteAt and reports whether
// anything was removed.
func dropEngineWrites(item *ItemState, keys []string) bool {
	changed := false
	for _, k := range keys {
		if _, ok := item.EngineWriteAt[k]; ok {
			delete(item.EngineWriteAt, k)
			changed = true
		}
	}
	if len(item.EngineWriteAt) == 0 {
		item.EngineWriteAt = nil
	}
	return changed
}

// SetEngineWriteGraceForTest overrides the protection grace margin. Test seam.
func (s *Store) SetEngineWriteGraceForTest(d time.Duration) {
	s.mu.Lock()
	s.engineWriteGrace = d
	s.mu.Unlock()
}

// graceLocked returns the configured grace; s.mu must be held.
func (s *Store) graceLocked() time.Duration {
	if s.engineWriteGrace > 0 {
		return s.engineWriteGrace
	}
	return DefaultEngineWriteGrace
}

// EffectiveShallow returns pi with its Status, Labels and IsClosed adjusted by
// the engine-write protection, for drift detection (#2159 R2): exactly the
// values Reconcile would leave the cached item with. active reports that an
// engine write still shields the item from a listing fetched at fetchedAt, in
// which case the caller should also discount UpdatedAt (engine writes never
// bump the cached value, so a confirming listing always looks newer). ok is
// false when the item is not cached. Read-only.
func (s *Store) EffectiveShallow(pi gh.ProjectItem, fetchedAt time.Time) (eff gh.ProjectItem, active, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, found := s.items[itemKeyFor(pi.Repo, pi.Number)]
	if !found {
		return pi, false, false
	}
	v := protectShallow(item, pi, fetchedAt, s.graceLocked(), time.Now())
	eff = pi
	eff.Status = v.Status
	if v.Labels != nil {
		eff.Labels = v.Labels
	}
	if v.KeepClosed {
		eff.IsClosed = item.IsClosed
	}
	return eff, v.Active, true
}

// EffectiveProbe returns the Status and closed state a probe listing item (fetched
// at fetchedAt) would leave the cached item with, for the probe loop's pre-apply
// reads (#2178): exactly what applyProbeItem will keep or overwrite. ok is false
// when the item is not cached. Read-only.
func (s *Store) EffectiveProbe(repo string, number int, pi gh.BoardProbeItem, fetchedAt time.Time) (status string, isClosed, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, found := s.items[itemKeyFor(repo, number)]
	if !found {
		return pi.Status, pi.IsClosed, false
	}
	v := protectStatusClosed(item, pi.Status, pi.IsClosed, fetchedAt, s.graceLocked(), time.Now())
	isClosed = pi.IsClosed
	if v.KeepClosed {
		isClosed = item.IsClosed
	}
	return v.Status, isClosed, true
}
