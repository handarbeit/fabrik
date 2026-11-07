package engine

import "sync"

// emptyBoardPolls is how many consecutive polls of a never-populated board must
// each return zero items before the engine concludes the board is genuinely
// empty (#1894, ADR 1894). Each of those polls has already exhausted the
// github layer's three-attempt retry, so the conclusion rests on about six
// consecutive zeros spread over at least one poll interval.
const emptyBoardPolls = 2

// boardRetryQuieter is the optional capability of the concrete GitHub client
// that switches the board fetch/probe between the full empty-result retry
// policy and a single attempt. It is an optional interface rather than a
// GitHubClient method so the interface, boardcache, simgh and every fake stay
// untouched.
type boardRetryQuieter interface {
	SetBoardRetryQuiet(bool)
}

// emptyBoardState is the engine's in-memory verdict on "is this board
// genuinely empty, or is a zero a transient indexer fault?" (#1894).
//
// The verdict changes only retry cost and logging. It never touches the item
// Store, so a wrong "empty" self-heals on the next non-zero probe, and the
// populated-cache guard in runProbeAndDeepFetch stays authoritative.
//
// Fed from two places: the virgin branch of refreshCacheForPoll (a poll's
// probe returned zero items) and every site that bootstraps the cache from
// real items. Bootstrap also runs on transport goroutines, hence the mutex.
type emptyBoardState struct {
	mu           sync.Mutex
	everSawItems bool // items were seen at any point this process lifetime
	zeroPolls    int  // consecutive zero-item polls while everSawItems is false
	concluded    bool // currently concluded empty (quiet single-attempt mode)

	pollsOverride int // test seam (SetEmptyBoardPollsForTest); 0 = default
}

// noteBoardItems records that the board returned real items. If the engine had
// concluded the board was empty, it ends that episode and restores the full
// retry policy, so the item an operator just added is found with no restart.
func (e *Engine) noteBoardItems() {
	s := &e.emptyBoard
	s.mu.Lock()
	wasConcluded := s.concluded
	s.everSawItems = true
	s.zeroPolls = 0
	s.concluded = false
	s.mu.Unlock()
	if wasConcluded {
		e.setBoardRetryQuiet(false)
		e.logf(0, "cache", "board has items again — resuming normal polling\n")
	}
}

// observeZeroItemPoll records a poll whose probe returned zero items (with a
// project ID) against a never-populated cache. It returns true when the caller
// should log the "deferring bootstrap" line: only once items have been seen
// before, when a zero is genuinely suspicious. A never-populated board stays
// silent until the verdict, which logs exactly one line per episode.
func (e *Engine) observeZeroItemPoll() (logDeferral bool) {
	s := &e.emptyBoard
	s.mu.Lock()
	if s.everSawItems {
		s.mu.Unlock()
		return true
	}
	s.zeroPolls++
	conclude := !s.concluded && s.zeroPolls >= e.emptyBoardPollsN()
	if conclude {
		s.concluded = true
	}
	s.mu.Unlock()
	if conclude {
		e.setBoardRetryQuiet(true)
		e.logf(0, "cache", "board is empty — add issues to the board to start\n")
	}
	return false
}

// boardConcludedEmpty reports whether the current episode is concluded empty.
func (e *Engine) boardConcludedEmpty() bool {
	e.emptyBoard.mu.Lock()
	defer e.emptyBoard.mu.Unlock()
	return e.emptyBoard.concluded
}

func (e *Engine) setBoardRetryQuiet(quiet bool) {
	if q, ok := e.client.(boardRetryQuieter); ok {
		q.SetBoardRetryQuiet(quiet)
	}
}
