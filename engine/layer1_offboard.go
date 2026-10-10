package engine

import (
	"fmt"
	"time"
)

// layer1OffBoardTTL is how long a successful "not on the board" lookup
// suppresses further Layer 1 fallback lookups for the same issue (#2093). It
// matches boardcache's recentMissTTL.
const layer1OffBoardTTL = 10 * time.Minute

func layer1OffBoardKey(projectID, repo string, number int) string {
	return fmt.Sprintf("%s|%s#%d", projectID, repo, number)
}

// layer1OffBoardHit reports whether key has an unexpired negative entry.
func (e *Engine) layer1OffBoardHit(key string) bool {
	e.layer1OffBoardMu.Lock()
	defer e.layer1OffBoardMu.Unlock()
	exp, ok := e.layer1OffBoard[key]
	return ok && e.now().Before(exp)
}

// layer1OffBoardMark records key as off-board until now+layer1OffBoardTTL and
// sweeps expired entries so the map stays bounded.
func (e *Engine) layer1OffBoardMark(key string) {
	e.layer1OffBoardMu.Lock()
	defer e.layer1OffBoardMu.Unlock()
	now := e.now()
	if e.layer1OffBoard == nil {
		e.layer1OffBoard = make(map[string]time.Time)
	}
	for k, exp := range e.layer1OffBoard {
		if !now.Before(exp) {
			delete(e.layer1OffBoard, k)
		}
	}
	e.layer1OffBoard[key] = now.Add(layer1OffBoardTTL)
}

// layer1OffBoardClear drops the negative entry for key, if any.
func (e *Engine) layer1OffBoardClear(key string) {
	e.layer1OffBoardMu.Lock()
	defer e.layer1OffBoardMu.Unlock()
	delete(e.layer1OffBoard, key)
}

// layer1OffBoardClearAll drops every negative entry. Called when an item is
// added to the board (projects_v2_item created), whose payload carries no
// repo/number to target a single key.
func (e *Engine) layer1OffBoardClearAll() {
	e.layer1OffBoardMu.Lock()
	defer e.layer1OffBoardMu.Unlock()
	e.layer1OffBoard = nil
}
