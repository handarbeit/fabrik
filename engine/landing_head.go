package engine

import (
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ciJudgedHeadTTL bounds how long a recorded CI-gate head is honoured. The record
// is written in Phase 1 and read in Phase 2 of the same poll pass, so anything
// older is a leftover from an earlier poll and is ignored.
const ciJudgedHeadTTL = 2 * time.Minute

// ciJudgedHead is the PR head SHA the stage CI gate cleared, with the instant it did.
type ciJudgedHead struct {
	sha string
	at  time.Time
}

func ciJudgedHeadKey(item gh.ProjectItem, defaultRepo string) string {
	return fmt.Sprintf("%s#%d", itemOwnerRepoString(item, defaultRepo), item.Number)
}

// noteCIJudgedHead records the head the CI gate just cleared for item (#2342). The
// gate reads it from the cached linked-PR read, which Phase 2's landing never sees;
// recording it lets attemptMergeOnValidate compare it with the head the review and
// feedback gates judged, at no extra GitHub read. In memory only: after a restart
// there is no CI claim to compare, and the landing's own pin still applies.
func (e *Engine) noteCIJudgedHead(item gh.ProjectItem, sha string) {
	if sha == "" {
		return
	}
	e.ciJudgedHeads.Store(ciJudgedHeadKey(item, e.defaultRepo()), ciJudgedHead{sha: sha, at: e.now()})
}

// takeCIJudgedHead consumes the recorded CI-gate head for item. It reports false
// when none was recorded or the record is older than ciJudgedHeadTTL.
func (e *Engine) takeCIJudgedHead(item gh.ProjectItem) (string, bool) {
	v, ok := e.ciJudgedHeads.LoadAndDelete(ciJudgedHeadKey(item, e.defaultRepo()))
	if !ok {
		return "", false
	}
	rec := v.(ciJudgedHead)
	if e.now().Sub(rec.at) > ciJudgedHeadTTL {
		return "", false
	}
	return rec.sha, true
}
