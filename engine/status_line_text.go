package engine

import (
	"fmt"
	"strings"
	"time"
)

// The status-line wording lives here and only here (#2048), so the catalogue
// in docs/USER_GUIDE.md has a single source. Every builder returns a short,
// single-line string; the writer truncates anything that still runs long.

// statusLineQueued: a merge-train member admitted to a batch of n.
func statusLineQueued(batchSize int) string {
	return fmt.Sprintf("queued · batch of %d", batchSize)
}

// statusLineQueuedWaiting: in the holding column with no batch in flight (just
// advanced to Queued, or a trial was abandoned and the member awaits the next
// formation).
const statusLineQueuedWaiting = "queued"

// statusLineTrial: a trial phase ("resolving conflicts", "CI running"). The
// number is the draft CI PR, which only exists after assembly; before that the
// line carries no number.
func statusLineTrial(prNum int, phase string) string {
	if prNum <= 0 {
		return "trial · " + phase
	}
	return fmt.Sprintf("trial #%d · %s", prNum, phase)
}

// statusLineBisect: step i of at most n validations.
func statusLineBisect(step, ceiling int) string {
	return fmt.Sprintf("bisecting · step %d of %d", step, ceiling)
}

// statusLineLanding: the batch (or singleton) is being landed.
const statusLineLanding = "landing"

// statusLineDeferred: held out of a batch because it overlaps another member.
func statusLineDeferred(otherIssue int) string {
	return fmt.Sprintf("deferred: overlaps #%d", otherIssue)
}

// statusLineCatchUp: the singleton catch-up is waiting on a PR's CI.
func statusLineCatchUp(prNum int) string {
	return fmt.Sprintf("catch-up · CI on PR #%d", prNum)
}

// statusLineStageRunning / statusLineCommentReview: a worker is running.
func statusLineStageRunning(stage string) string { return stage + " · running" }

func statusLineCommentReview(stage string) string { return stage + " · comment review" }

// statusLineBlocked: waiting on a dependency.
func statusLineBlocked(blocker int) string {
	return fmt.Sprintf("blocked by #%d", blocker)
}

// statusLinePaused: a one-line pause reason.
func statusLinePaused(reason string) string {
	if reason == "" {
		return "paused"
	}
	return "paused: " + reason
}

// statusLineClaudeLimit: the account-wide suspension ends at until, in the
// daemon's local time.
func statusLineClaudeLimit(until time.Time) string {
	return "claude-limit until " + until.Local().Format("15:04")
}

// statusLineAwaitingCI: a stage is waiting on CI for a PR.
func statusLineAwaitingCI(prNum int) string {
	if prNum <= 0 {
		return "awaiting CI"
	}
	return fmt.Sprintf("awaiting CI on PR #%d", prNum)
}

// pauseReasonFromComment derives the one-line reason for "paused: <reason>"
// from an engine pause comment, whose first line is the stable header
// "🏭 **Fabrik — <what happened>**" (or "🏭 **Fabrik merge-train — …**").
// Falls back to "" (a bare "paused") for a comment that does not follow it.
func pauseReasonFromComment(comment string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(comment), "\n")
	first = strings.TrimSpace(strings.Trim(strings.TrimSpace(first), "*"))
	first = strings.TrimSpace(strings.TrimPrefix(first, "🏭"))
	first = strings.Trim(first, "* ")
	for _, sep := range []string{" — ", " - ", ": "} {
		if _, after, ok := strings.Cut(first, sep); ok && strings.HasPrefix(first, "Fabrik") {
			return strings.TrimSpace(strings.Trim(after, "* "))
		}
	}
	return ""
}
