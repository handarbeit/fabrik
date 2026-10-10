package pruefer

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// Skipping a pure merge-train catch-up (#2066, adrs/2066-pruefer-skip-pure-catch-up.md).
//
// The Fabrik engine's merge train (#2044) can push a merge commit of the
// pinned base onto a Queued member's own PR branch. When that merge is clean
// (no conflict-resolution edits) the PR's diff against its base is unchanged
// in substance, so a fresh review of the new head repeats the previous one.
// The engine vouches for such a push with a self-authored marker comment on
// the PR; this file recognises it.
//
// The skip is a trust decision — a forged signal would mute Pruefer — so it
// rests on the engine's marker comment (authored by an operator-configured
// login, for exactly this head, saying pure=true) plus the head's actual
// parent shape, never on the Fabrik-Train-Catch-Up commit trailer, which
// anyone with push access can type. Anything not positively verified means
// "review as today" (R4).

// maxCatchUpChain bounds how many consecutive catch-up merges are followed
// back to a head Pruefer reviewed. A skipped head leaves no review for the
// next catch-up to anchor on, so consecutive catch-ups chain; the cap bounds
// the API calls and forces a real review periodically.
const maxCatchUpChain = 4

// catchUpMarkerRe mirrors the engine's catchUpMarkerRe in
// engine/catchup_feedback.go (which is unexported, and pruefer does not
// import engine). engine/catchup_feedback_contract_test.go renders a marker
// with the engine's own formatter and parses it here, so a format change
// there fails CI instead of silently disabling the skip.
var catchUpMarkerRe = regexp.MustCompile(`<!-- fabrik:train-catch-up head=([0-9a-f]{7,64}) base=([0-9a-f]{7,64}) pure=(true|false) -->`)

// CatchUpMarker is one parsed engine catch-up marker.
type CatchUpMarker struct {
	Head string // the catch-up merge commit
	Base string // the pinned base SHA that was merged in
	Pure bool   // true: no conflict-resolution edits
}

// ParseCatchUpMarker returns every well-formed marker in body.
func ParseCatchUpMarker(body string) []CatchUpMarker {
	var out []CatchUpMarker
	for _, sm := range catchUpMarkerRe.FindAllStringSubmatch(body, -1) {
		out = append(out, CatchUpMarker{Head: sm[1], Base: sm[2], Pure: sm[3] == "true"})
	}
	return out
}

// isFullSHA reports whether s is a full-length (SHA-1 or SHA-256) lowercase
// hex object name. The engine's regex also admits abbreviated SHAs; a marker
// that short is not trusted here.
func isFullSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func loginIn(logins []string, login string) bool {
	for _, l := range logins {
		if strings.EqualFold(l, login) {
			return true
		}
	}
	return false
}

// catchUpMarkerStatus is the outcome of looking for a trusted marker.
type catchUpMarkerStatus int

const (
	markerAbsent    catchUpMarkerStatus = iota // no trusted marker for this head
	markerNotPure                              // a trusted marker says pure=false
	markerAmbiguous                            // trusted markers for this head disagree, or are not full SHAs
	markerPure                                 // exactly one coherent pure=true marker
)

// findCatchUpMarker looks for markers for exactly head among comments,
// trusting only comments authored by one of authors (case-insensitive). Any
// trusted pure=false marker for the head wins over a pure=true one; disagreeing
// bases are ambiguous. Both SHAs must be full length.
func findCatchUpMarker(comments []gh.Comment, authors []string, head string) (CatchUpMarker, catchUpMarkerStatus) {
	if len(authors) == 0 || !isFullSHA(head) {
		return CatchUpMarker{}, markerAbsent
	}
	var found CatchUpMarker
	status := markerAbsent
	for _, c := range comments {
		if !loginIn(authors, c.Author) {
			continue
		}
		for _, mk := range ParseCatchUpMarker(c.Body) {
			if mk.Head != head {
				continue
			}
			if !isFullSHA(mk.Base) {
				return CatchUpMarker{}, markerAmbiguous
			}
			if !mk.Pure {
				return mk, markerNotPure
			}
			if status == markerPure && mk.Base != found.Base {
				return CatchUpMarker{}, markerAmbiguous
			}
			found, status = mk, markerPure
		}
	}
	return found, status
}

// catchUpRecheckDelays are the waits before each bounded re-read of the PR's
// comments when the head is a merge of the base but no marker exists yet
// (R8): the engine posts the marker after the push, so the synchronize event
// can arrive first. Total ~8s, held only for a head that already looks like a
// catch-up. Package-level so tests can zero it.
var catchUpRecheckDelays = []time.Duration{2 * time.Second, 3 * time.Second, 3 * time.Second}

// catchUpSleep waits d or until ctx is done, reporting whether the full wait
// elapsed. A var so tests can observe or skip it.
var catchUpSleep = func(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// catchUpVerdict is catchUpSkip's result.
type catchUpVerdict struct {
	Skip     bool
	Detail   string // human-readable reason, logged; empty for an ordinary push
	Degraded bool   // a tolerated read failed, so the outcome must not be memoised
}

// catchUpSkip decides whether pr's head is a pure merge-train catch-up that
// can be skipped. reviews are the PR's reviews already fetched by ReviewPR.
// Every uncertain input yields Skip=false.
func catchUpSkip(ctx context.Context, client GitHubReviewer, cfg Config, botLogin, owner, repo string, pr gh.PRDetails, reviews []gh.PRReview) catchUpVerdict {
	if len(cfg.CatchUpMarkerAuthors) == 0 || pr.HeadSHA == "" || pr.BaseRef == "" {
		return catchUpVerdict{}
	}

	// R6: the skip is only sound on top of a head Pruefer actually reviewed.
	reviewed := map[string]bool{}
	for _, r := range reviews {
		if r.CommitID != "" && strings.EqualFold(r.Author, botLogin) {
			reviewed[r.CommitID] = true
		}
	}
	if len(reviewed) == 0 {
		return catchUpVerdict{}
	}

	var comments []gh.Comment
	commentsRead := false
	readComments := func() error {
		cs, err := client.FetchIssueComments(owner, repo, pr.Number)
		if err != nil {
			return err
		}
		comments, commentsRead = cs, true
		return nil
	}

	candidate := pr.HeadSHA
	for depth := 0; depth < maxCatchUpChain; depth++ {
		parents, err := client.FetchCommitParents(owner, repo, candidate)
		if err != nil {
			return catchUpVerdict{Degraded: true, Detail: fmt.Sprintf("reading parents of %s failed: %v", shortSHA(candidate), err)}
		}
		if len(parents) != 2 {
			if depth == 0 {
				return catchUpVerdict{} // ordinary push: nothing to explain
			}
			return catchUpVerdict{Detail: fmt.Sprintf("%s has %d parents, not a catch-up merge", shortSHA(candidate), len(parents))}
		}

		// The second parent must be on the PR's base branch (an ancestor of,
		// or equal to, its tip). Checked before the marker so a merge of
		// something else never pays the marker re-check wait below, and
		// because the marker's base is then required to equal it.
		behind, err := client.FetchCommitsBehind(owner, repo, parents[1], pr.BaseRef)
		if err != nil {
			return catchUpVerdict{Degraded: true, Detail: fmt.Sprintf("checking %s against %s failed: %v", shortSHA(parents[1]), pr.BaseRef, err)}
		}
		if behind != 0 {
			if depth == 0 {
				return catchUpVerdict{} // a merge of something that is not on the base branch
			}
			return catchUpVerdict{Detail: fmt.Sprintf("second parent %s of %s is not on %s", shortSHA(parents[1]), shortSHA(candidate), pr.BaseRef)}
		}

		if !commentsRead {
			if err := readComments(); err != nil {
				return catchUpVerdict{Degraded: true, Detail: fmt.Sprintf("reading PR comments failed: %v", err)}
			}
		}
		mk, status := findCatchUpMarker(comments, cfg.CatchUpMarkerAuthors, candidate)
		// R8: the marker follows the push. Re-read a bounded number of times
		// for the current head only; older heads' markers long since exist.
		for i := 0; depth == 0 && status == markerAbsent && i < len(catchUpRecheckDelays); i++ {
			if !catchUpSleep(ctx, catchUpRecheckDelays[i]) {
				return catchUpVerdict{Degraded: true, Detail: "marker re-check cancelled"}
			}
			if err := readComments(); err != nil {
				return catchUpVerdict{Degraded: true, Detail: fmt.Sprintf("re-reading PR comments failed: %v", err)}
			}
			mk, status = findCatchUpMarker(comments, cfg.CatchUpMarkerAuthors, candidate)
		}

		switch status {
		case markerAbsent:
			return catchUpVerdict{Detail: fmt.Sprintf("no catch-up marker for %s from a configured catch_up_marker_authors login", shortSHA(candidate))}
		case markerNotPure:
			return catchUpVerdict{Detail: fmt.Sprintf("catch-up marker for %s says pure=false (conflict-resolution edits)", shortSHA(candidate))}
		case markerAmbiguous:
			return catchUpVerdict{Detail: fmt.Sprintf("catch-up markers for %s are ambiguous", shortSHA(candidate))}
		}
		if mk.Base != parents[1] {
			return catchUpVerdict{Detail: fmt.Sprintf("marker base %s is not the second parent %s of %s", shortSHA(mk.Base), shortSHA(parents[1]), shortSHA(candidate))}
		}

		if reviewed[parents[0]] {
			return catchUpVerdict{Skip: true, Detail: fmt.Sprintf("engine marker for %s, base %s merged over reviewed head %s", shortSHA(candidate), shortSHA(mk.Base), shortSHA(parents[0]))}
		}
		candidate = parents[0] // a skipped catch-up leaves no review: follow the chain
	}
	return catchUpVerdict{Detail: fmt.Sprintf("no reviewed head within %d consecutive catch-ups", maxCatchUpChain)}
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
