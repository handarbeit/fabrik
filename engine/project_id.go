package engine

import (
	"errors"

	gh "github.com/handarbeit/fabrik/github"
)

// errProjectIDUnknown is returned by a board write that has no project ID to
// send: the board's is empty and the engine has never seen a real one (#2396).
// It is a deferral, not a failure of the item: callers must not count it
// against a retry budget or escalate on it.
var errProjectIDUnknown = errors.New("project ID unknown")

// writeProjectID returns the project ID a board mutation must use: the
// board's own, else the engine's last seen one. When both are empty it returns
// errProjectIDUnknown and nothing may be sent to GitHub. The unknown condition
// is logged once per episode; the episode ends when an ID is next resolved.
func (e *Engine) writeProjectID(board *gh.ProjectBoard) (string, error) {
	if e.projectIDWriteGuardDisabledForTest.Load() {
		if board == nil {
			return "", nil
		}
		return board.ProjectID, nil
	}
	if board != nil && board.ProjectID != "" {
		e.noteProjectIDKnown()
		return board.ProjectID, nil
	}
	e.mu.Lock()
	id := e.boardProjectID
	e.mu.Unlock()
	if id != "" {
		e.noteProjectIDKnown()
		return id, nil
	}
	e.projectIDEpisodeMu.Lock()
	first := !e.projectIDUnknownLogged
	e.projectIDUnknownLogged = true
	e.projectIDEpisodeMu.Unlock()
	if first {
		e.logf(0, "warn", "board write deferred: project ID unknown (the board and the engine both lack one); will retry on a later poll\n")
	}
	return "", errProjectIDUnknown
}

// noteProjectIDKnown ends a project-ID-unknown episode.
func (e *Engine) noteProjectIDKnown() {
	e.projectIDEpisodeMu.Lock()
	e.projectIDUnknownLogged = false
	e.projectIDEpisodeMu.Unlock()
}

// fillBoardProjectID gives a board that arrived without a project ID the
// engine's last seen one, so every board-threaded write site sees it (#2396).
func (e *Engine) fillBoardProjectID(board *gh.ProjectBoard) {
	if board == nil || board.ProjectID != "" || e.projectIDWriteGuardDisabledForTest.Load() {
		return
	}
	e.mu.Lock()
	id := e.boardProjectID
	e.mu.Unlock()
	board.ProjectID = id
}

// SetProjectIDWriteGuardDisabledForTest neutralises the write-path project-ID
// fallback and guard (#2396), so a test can show the empty-ID write it prevents.
func (e *Engine) SetProjectIDWriteGuardDisabledForTest(v bool) {
	e.projectIDWriteGuardDisabledForTest.Store(v)
}
