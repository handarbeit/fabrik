package engine

import "github.com/handarbeit/fabrik/internal/itemstate"

// SeedValidateCompletedSHAForTest records sha as the Validate completion SHA of an
// item, as a real Validate completion would (#2158). The sim never runs Validate, and
// the store is unexported; without this the SHA-invalidation scan skips every sim item
// (no recorded SHA). Tests only.
func (e *Engine) SeedValidateCompletedSHAForTest(repo string, number int, sha string) {
	e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repo, Number: number, SHA: sha})
}

// SeedLinkedPRHeadForTest records sha as the linked PR's live head, as the
// pull_request webhook or the reconcile would when the head moves (#2158). The sim has
// no webhook feed, and a settled item is not deep-fetched, so without this the
// SHA-invalidation scan never sees a head change on an open item. Tests only.
func (e *Engine) SeedLinkedPRHeadForTest(repo string, number, prNumber int, sha string) {
	e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: repo, Number: number, LinkedPRNum: prNumber, SHA: sha})
}

// SetValidateSHARebuildDisabledForTest turns off the post-restart rebuild of the Validate
// completion SHA (#2343), restoring the FR-5 "empty SHA means do nothing" behaviour, and
// returns the restore function. It is how the restart twins show they are not vacuous.
func (e *Engine) SetValidateSHARebuildDisabledForTest() (restore func()) {
	e.validateSHARebuildOff.Store(true)
	return func() { e.validateSHARebuildOff.Store(false) }
}
