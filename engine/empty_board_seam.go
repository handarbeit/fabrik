package engine

// SetEmptyBoardPollsForTest overrides how many consecutive zero-item polls
// conclude an empty board (#1894). n <= 0 restores the default. Per-Engine.
func (e *Engine) SetEmptyBoardPollsForTest(n int) {
	e.emptyBoard.mu.Lock()
	defer e.emptyBoard.mu.Unlock()
	e.emptyBoard.pollsOverride = n
}

// BoardConcludedEmptyForTest reports the empty-board verdict.
func (e *Engine) BoardConcludedEmptyForTest() bool { return e.boardConcludedEmpty() }

func (e *Engine) emptyBoardPollsN() int {
	// Called with e.emptyBoard.mu held by observeZeroItemPoll.
	if n := e.emptyBoard.pollsOverride; n > 0 {
		return n
	}
	return emptyBoardPolls
}
