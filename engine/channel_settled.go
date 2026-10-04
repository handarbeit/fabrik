package engine

import "github.com/handarbeit/fabrik/internal/itemstate"

// validateSettledNow reports whether st currently satisfies the cache-only
// validate-settled predicate. Completed in the validate-settled task.
func (e *Engine) validateSettledNow(st *itemstate.ItemState) bool { return false }
