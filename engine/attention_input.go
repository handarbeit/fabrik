package engine

import (
	"regexp"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Cache-only inputs for the attention classifier's Suspended state and its
// decision options (#2120, ADR-2120). Like engine/localapi.go, nothing here may
// reach GitHub: item state is read through Store.Peek/Scan only, and the one
// GitHub-anchored time comes from data the engine already holds (the
// record-on-write LabelAppliedAt map a poll-side recorder fills, see
// decision_anchor.go, and the cached stage comment's CreatedAt).

// blockerNode is what the resolver needs to know about one potential blocker,
// copied out of the store so it can outlive a Scan visitor.
type blockerNode struct {
	closed    bool
	column    string
	blockedBy []gh.Dependency
	// enteredAt/movedAt are the item's StatusEnteredAt and StatusMovedAt: when the
	// daemon first saw it in its column, and when it watched it move there.
	enteredAt time.Time
	movedAt   time.Time
	// hydrated is true once the item was deep-fetched, i.e. blockedBy is known
	// rather than merely empty.
	hydrated bool
}

// blockerLookup resolves "repo#number" to a node; false means the cache does
// not hold the item (unknown).
type blockerLookup func(repo string, n int) (blockerNode, bool)

func nodeOf(st *itemstate.ItemState) blockerNode {
	return blockerNode{
		closed:    st.IsClosed,
		column:    st.Status,
		blockedBy: append([]gh.Dependency(nil), st.BlockedBy...),
		enteredAt: st.StatusEnteredAt,
		movedAt:   st.StatusMovedAt,
		hydrated:  !st.LastDeepFetchAt.IsZero(),
	}
}

// peekBlockerLookup resolves blockers with Store.Peek. Use it outside a Scan
// visitor only: Peek takes the store's read lock, which a nested acquisition
// inside a Scan could turn into a deadlock behind a waiting writer.
func (e *Engine) peekBlockerLookup() blockerLookup {
	return func(repo string, n int) (blockerNode, bool) {
		snap, ok := e.store.Peek(repo, n)
		if !ok {
			return blockerNode{}, false
		}
		st := snap.State()
		return nodeOf(&st), true
	}
}

// scanBlockerIndex copies every cached issue's blocker-relevant fields in one
// Scan, so a later Scan can classify every item without a nested lock.
func (e *Engine) scanBlockerIndex() blockerLookup {
	idx := map[string]blockerNode{}
	e.store.Scan(func(st *itemstate.ItemState) {
		if st.IsPR {
			return
		}
		idx[issueRef(st.Repo, st.Number)] = nodeOf(st)
	})
	return func(repo string, n int) (blockerNode, bool) {
		node, ok := idx[issueRef(repo, n)]
		return node, ok
	}
}

const maxBlockerDepth = 8

// resolveParkedBlockers reports the parked items st is blocked on and whether
// they are its only open blockers (#2120 R1b). A blocker is stuck when it sits
// in an unmanaged parking column, or is itself blocked only by stuck blockers.
// Anything the cache cannot vouch for — a blocker it does not hold, one not yet
// deep-fetched, an active-column blocker with no open blockers of its own, a
// cycle, or a chain deeper than maxBlockerDepth — fails closed (only == false),
// so the item stays `waiting`.
func (e *Engine) resolveParkedBlockers(st *itemstate.ItemState, look blockerLookup) (parked []attention.ParkedBlocker, only bool) {
	if look == nil || st.LastDeepFetchAt.IsZero() {
		return nil, false
	}
	seen := map[string]bool{}
	var leaves []attention.ParkedBlocker
	var stuck func(repo string, n, depth int, visited map[string]bool) bool
	stuck = func(repo string, n, depth int, visited map[string]bool) bool {
		ref := issueRef(repo, n)
		node, ok := look(repo, n)
		if !ok || depth > maxBlockerDepth || visited[ref] {
			return false
		}
		if node.closed {
			return true // no longer blocks
		}
		if node.column != "" && stages.IsParkingColumn(e.cfg.Stages, node.column) {
			if !seen[ref] {
				seen[ref] = true
				leaves = append(leaves, attention.ParkedBlocker{
					Ref: ref, Column: node.column,
					EnteredAt: node.enteredAt, MovedAt: node.movedAt, Transitive: depth > 1,
				})
			}
			return true
		}
		if !node.hydrated {
			return false
		}
		visited[ref] = true
		defer delete(visited, ref)
		open := 0
		for _, d := range node.blockedBy {
			if !strings.EqualFold(d.State, "OPEN") {
				continue
			}
			open++
			dr := d.Repo
			if dr == "" {
				dr = repo
			}
			if !stuck(dr, d.Number, depth+1, visited) {
				return false
			}
		}
		return open > 0
	}
	open := 0
	for _, d := range st.BlockedBy {
		if !strings.EqualFold(d.State, "OPEN") {
			continue
		}
		open++
		dr := d.Repo
		if dr == "" {
			dr = st.Repo
		}
		if !stuck(dr, d.Number, 1, map[string]bool{issueRef(st.Repo, st.Number): true}) {
			return nil, false
		}
	}
	if open == 0 || len(leaves) == 0 {
		return nil, false
	}
	return leaves, true
}

// decisionNextStage is the stage a human advance would move the item to: the
// next configured stage that is not a parking column, as fabrik_advance sees it.
// Empty when that stage is a holding or cleanup stage (Validate: use the PR
// merge) or there is none, because then "advance" is not the decision.
func (e *Engine) decisionNextStage(status string) string {
	seen := false
	for _, s := range e.cfg.Stages {
		if s.Name == status {
			seen = true
			continue
		}
		if seen && !s.Unmanaged {
			if s.HoldingStage || s.CleanupWorktree {
				return ""
			}
			return s.Name
		}
	}
	return ""
}

// stageCommentCreatedAt is the creation time of the cached Fabrik comment for
// the item's current stage: a GitHub time that costs no call.
func stageCommentCreatedAt(st *itemstate.ItemState) time.Time {
	if st.Status == "" {
		return time.Time{}
	}
	if c := findStageComment(st.Comments, st.Status); c != nil {
		return c.CreatedAt
	}
	return time.Time{}
}

var (
	removePausedRE = regexp.MustCompile("(?i)\\bremove\\b[^\\n]{0,100}fabrik:paused")
	// commentResumeRE matches the engine's "reply/comment on this issue to
	// resume" pause wording (push_guard.go, item.go).
	commentResumeRE = regexp.MustCompile(`(?i)\b(reply|comment)\b[^\n]{0,60}\bto resume\b`)
)

const (
	fabrikCommentPrefix = "🏭 **Fabrik"
	// fabrikStageCommentPrefix opens a stage's output comment, which is also how
	// an awaiting-input question reaches the issue.
	fabrikStageCommentPrefix = "🏭 **Fabrik — stage:"
	// overseerAuditPrefix opens the audit comment of an overseer action
	// (overseerAuditBody); it never describes a pause.
	overseerAuditPrefix = "🏭 **Fabrik — overseer action:"
)

// resumeMethodOf says how the item's pause is lifted according to the engine's
// own pause comment (R-options-2). It walks back from the newest Fabrik comment
// to the newest one that is state-bearing, skipping comments that say nothing
// about a pause: an overseer audit comment or other informational Fabrik comment
// posted after the pause must not shadow it. A comment telling the operator to
// remove fabrik:paused is ResumeRemoveLabel; one asking for a reply or comment is
// ResumeComment. A stage comment with neither (an awaiting-input question)
// supersedes any older pause comment and leaves the method unknown, so the
// classifier claims nothing the engine's comment does not.
func resumeMethodOf(st *itemstate.ItemState) attention.ResumeMethod {
	for i := len(st.Comments) - 1; i >= 0; i-- {
		c := st.Comments[i]
		if c.FromPR != 0 || !strings.HasPrefix(c.Body, fabrikCommentPrefix) ||
			strings.HasPrefix(c.Body, overseerAuditPrefix) {
			continue
		}
		switch {
		case removePausedRE.MatchString(c.Body):
			return attention.ResumeRemoveLabel
		case commentResumeRE.MatchString(c.Body):
			return attention.ResumeComment
		case strings.HasPrefix(c.Body, fabrikStageCommentPrefix):
			return attention.ResumeUnknown
		}
	}
	return attention.ResumeUnknown
}
