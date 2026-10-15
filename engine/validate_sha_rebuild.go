package engine

import (
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Validate completion SHA rebuild after a restart (#2343, ADR-2343).
//
// ItemState.ValidateCompletedSHA lives only in the in-memory Store, so a restart wipes
// it and settleSHAInvalidationScan skips every item with an empty value (FR-5): a push
// after the restart would never invalidate Validate. The value is rebuilt from durable
// GitHub evidence — the `commit:` field of the engine's own Validate stage comment,
// which captureGitMeta reads from the worktree HEAD after the run and so can never be
// newer than the head Validate ran on.
//
// The hydrate runs before the catch-up loop in poll(): runCatchUpPhase2 (the landing
// decision) runs ahead of settleSHAInvalidationScan, so rebuilding inside the scan would
// still let a yolo item land on the first post-restart poll.

// validateCommentsFirstPageCap is GitHub's first-page comment limit. A list at the cap
// may have lost the Validate comment, so it is not reliable evidence.
const validateCommentsFirstPageCap = 100

// minStageCommitLen is the shortest hex prefix accepted as a recorded commit.
const minStageCommitLen = 7

// parseStageCommit extracts the commit: field from a stage comment's metadata line
// (`*branch: … | commit: <sha> | main: … | ts*`). It returns "" when the field is absent,
// not lowercase-or-uppercase hex of at least minStageCommitLen characters (e.g. the
// literal "unknown"), or longer than a full SHA.
func parseStageCommit(commentBody string) string {
	lines := strings.SplitN(commentBody, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	for _, segment := range strings.Split(lines[1], "|") {
		segment = strings.TrimSpace(segment)
		if !strings.HasPrefix(segment, "commit:") {
			continue
		}
		sha := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(segment, "commit:")), "*")
		sha = strings.ToLower(strings.TrimSpace(sha))
		if len(sha) < minStageCommitLen || len(sha) > 40 {
			return ""
		}
		for _, r := range sha {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return ""
			}
		}
		return sha
	}
	return ""
}

// rebuildValidateSHA returns the completion SHA recoverable from comments, trusting only
// the last Validate stage comment authored by self (the (comment review) and (review
// feedback addressed) variants never match the header). When the live head starts with
// the recorded (abbreviated) commit the head is unchanged since Validate ran and the full
// live head is returned; otherwise the abbreviated commit is returned as-is, which
// differs from the live head and so reads as "head moved" under every != comparison.
// ok is false when no SHA can be recovered: an empty self, a comment list at the
// first-page cap, no own Validate comment, or a malformed commit.
func rebuildValidateSHA(comments []gh.Comment, self, liveHead string) (sha string, ok bool) {
	if self == "" || len(comments) >= validateCommentsFirstPageCap {
		return "", false
	}
	const prefix = "🏭 **Fabrik — stage: Validate**"
	var found *gh.Comment
	for i := range comments {
		if comments[i].Author == self && strings.HasPrefix(comments[i].Body, prefix) {
			found = &comments[i]
		}
	}
	if found == nil {
		return "", false
	}
	commit := parseStageCommit(found.Body)
	if commit == "" {
		return "", false
	}
	if live := strings.ToLower(liveHead); live != "" && strings.HasPrefix(live, commit) {
		return liveHead, true
	}
	return commit, true
}

// hydrateValidateCompletionSHAs fills the completion SHA of every open
// stage:Validate:complete item at Validate whose Store value is empty (a restart), then
// runs the scan's own invalidation check on it. An item with no recoverable SHA is
// re-validated (R3): landing it on an unverified head is the unsafe direction, and the
// re-run records a fresh SHA. It returns the keys of items it invalidated, whose
// label copies in the caller's loop are now stale.
func (e *Engine) hydrateValidateCompletionSHAs(candidates []gh.ProjectItem) map[string]bool {
	if e.validateSHARebuildOff.Load() {
		return nil
	}
	var invalidated map[string]bool
	for _, item := range candidates {
		if !hasLabel(item.Labels, "stage:Validate:complete") || hasLabel(item.Labels, "fabrik:paused") || item.IsClosed {
			continue
		}
		if st := stages.FindStage(e.cfg.Stages, item.Status); st == nil || st.Name != "Validate" {
			continue
		}
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		snap, err := e.store.Get(repoStr, item.Number)
		if err != nil || snap.ValidateCompletedSHA() != "" {
			continue
		}
		lpr := snap.LinkedPR()
		if lpr == nil || lpr.ObservedHead() == "" {
			continue // PR or head not known yet — retry next poll; not an R3 case.
		}
		liveHead := lpr.ObservedHead()
		if snap.Worker() != nil {
			continue // FR-6: the running Validate will record its own SHA.
		}
		if e.validateItemLanded(item, snap, lpr) != "" {
			continue
		}
		if lpr.Number != 0 && e.catchUpPushInFlight(owner, repo, lpr.Number) {
			continue
		}
		sha, ok := rebuildValidateSHA(item.Comments, e.selfLogin(), liveHead)
		if !ok {
			e.logf(item.Number, "validate-sha", "no Validate completion SHA recoverable after restart — re-running Validate instead of trusting head %s\n", liveHead)
			e.handleValidateSHAInvalidation(item, owner, repo)
			if invalidated == nil {
				invalidated = map[string]bool{}
			}
			invalidated[issueKey(item, e.defaultRepo())] = true
			continue
		}
		e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repoStr, Number: item.Number, SHA: sha})
		e.logf(item.Number, "validate-sha", "rebuilt completion SHA %s from the Validate stage comment\n", sha)
		if e.checkValidateSHAInvalidation(item) {
			if invalidated == nil {
				invalidated = map[string]bool{}
			}
			invalidated[issueKey(item, e.defaultRepo())] = true
		}
	}
	return invalidated
}
