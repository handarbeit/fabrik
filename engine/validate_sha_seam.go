package engine

import "github.com/handarbeit/fabrik/internal/itemstate"

// SeedValidateCompletedSHAForTest records sha as the Validate completion SHA of an
// item, as a real Validate completion would (#2158). The sim never runs Validate, and
// the store is unexported; without this the SHA-invalidation scan skips every sim item
// (no recorded SHA). Tests only.
func (e *Engine) SeedValidateCompletedSHAForTest(repo string, number int, sha string) {
	e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repo, Number: number, SHA: sha})
}
