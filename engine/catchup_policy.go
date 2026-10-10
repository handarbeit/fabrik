package engine

import (
	"strings"
	"time"
	"unicode"
)

// Catch-up push policy memo (#2065, ADR-2065).
//
// A repo can forbid the merge commit a singleton catch-up (#2044) pushes onto the
// member's own branch — a linear-history rule, a ruleset, branch protection. The push is
// then rejected on every attempt for every member, each attempt costing a worktree
// preparation, a merge, a push and a log line, with an outcome that never changes. When a
// rejection is *positively* identified as repo policy, the engine remembers it in memory,
// per repo, and skips the catch-up there until the memo expires. Nothing is persisted: a
// restart clears the memo, and so does expiry, so a policy change is eventually re-probed.

// catchUpPolicyMemoTTL bounds how long a policy rejection suppresses catch-up attempts on
// a repo. After it the next singleton re-probes by attempting the catch-up normally.
const catchUpPolicyMemoTTL = 24 * time.Hour

// catchUpPolicyReasonMax caps the rejection text carried into the single operator log
// line. Remote output is influenceable (a ruleset's custom message), so it is bounded.
const catchUpPolicyReasonMax = 300

// catchUpPolicyMemo is one repo's remembered policy rejection.
type catchUpPolicyMemo struct {
	until  time.Time
	reason string
}

// catchUpPolicyAttemptReason is the short per-attempt fallback text used for a policy
// rejection, in place of git's raw multi-line output.
const catchUpPolicyAttemptReason = "push rejected by repository policy"

// Markers GitHub puts in a rejected push's output. They are trusted over prose.
const (
	ghMarkerProtectedBranch = "GH006" // protected-branch refusal
	ghMarkerRepoRule        = "GH013" // repository rule (ruleset) violation
)

// catchUpPushVetoes are lease-failure and transport/permission markers. If any appears
// the rejection is treated as transient and sets no memo, even next to a GH marker.
var catchUpPushVetoes = []string{
	// lease failure: the member's branch moved
	"stale info",
	"fetch first",
	"non-fast-forward",
	// transport
	"fatal: unable to access",
	"could not resolve host",
	"connection reset",
	"connection timed out",
	"connection refused",
	"the requested url returned error: 5",
	"rpc failed",
	"unexpected disconnect",
	// credentials/permission shortfall, not a branch rule
	"permission to ",
	"authentication failed",
}

// catchUpPushSecretMarkers mark a GH013 rejection that depends on the content being
// pushed (secret scanning / push protection), not on the repo as a whole.
var catchUpPushSecretMarkers = []string{"secret", "push protection"}

// classifyCatchUpPushRejection decides whether a failed catch-up push's git output is a
// repo-policy rejection. It returns the sanitised one-line reason and true only on a
// positive GH006/GH013 match with no lease/transport veto and no content-dependent
// (secrets) rule. Everything else — including free prose such as a bare "protected
// branch" — is ambiguous and returns ("", false): no memo, today's per-attempt behaviour.
func classifyCatchUpPushRejection(output string) (string, bool) {
	lower := strings.ToLower(output)
	for _, v := range catchUpPushVetoes {
		if strings.Contains(lower, v) {
			return "", false
		}
	}
	hasMarker := strings.Contains(output, ghMarkerProtectedBranch) || strings.Contains(output, ghMarkerRepoRule)
	if !hasMarker {
		return "", false
	}
	if strings.Contains(output, ghMarkerRepoRule) {
		for _, s := range catchUpPushSecretMarkers {
			if strings.Contains(lower, s) {
				return "", false
			}
		}
	}
	return catchUpPolicyReason(output), true
}

// catchUpPolicyReason extracts the rejection text GitHub returned — the `GH0xx` header,
// any further `remote: error:` lines and the `- …` rule bullets — as one sanitised line:
// `remote:` prefixes dropped, joined with "; ", control characters and runs of whitespace
// collapsed, capped at catchUpPolicyReasonMax runes.
func catchUpPolicyReason(output string) string {
	var parts []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "remote:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		switch {
		case strings.HasPrefix(rest, "error:"):
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(rest, "error:")))
		case strings.HasPrefix(rest, "- "):
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(rest, "- ")))
		}
	}
	reason := strings.Join(parts, "; ")
	reason = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, reason)), " ")
	if reason == "" {
		reason = catchUpPolicyAttemptReason
	}
	if r := []rune(reason); len(r) > catchUpPolicyReasonMax {
		reason = string(r[:catchUpPolicyReasonMax]) + "…"
	}
	return reason
}

// catchUpPolicyBackoff reports whether an unexpired policy memo covers repoKey
// ("owner/repo"), dropping an elapsed entry. Always false when the test-only neutralisation
// seam is set, which makes the skip observable by its absence.
func (e *Engine) catchUpPolicyBackoff(repoKey string) bool {
	if e.catchUpPolicyMemoSkipDisabledForTest {
		return false
	}
	e.trainCatchUp.mu.Lock()
	defer e.trainCatchUp.mu.Unlock()
	memo, ok := e.trainCatchUp.policy[repoKey]
	if !ok {
		return false
	}
	if !e.now().Before(memo.until) {
		delete(e.trainCatchUp.policy, repoKey)
		return false
	}
	return true
}

// recordCatchUpPolicyRejection sets repoKey's memo and reports whether it is new. A
// rejection arriving while a memo is still active (two partitions of one repo racing) is
// ignored — it neither extends the expiry nor reports as new — so exactly one operator line
// is logged per memo. The check and the set share one lock acquisition.
func (e *Engine) recordCatchUpPolicyRejection(repoKey, reason string) bool {
	e.trainCatchUp.mu.Lock()
	defer e.trainCatchUp.mu.Unlock()
	now := e.now()
	if memo, ok := e.trainCatchUp.policy[repoKey]; ok && now.Before(memo.until) {
		return false
	}
	if e.trainCatchUp.policy == nil {
		e.trainCatchUp.policy = make(map[string]catchUpPolicyMemo)
	}
	e.trainCatchUp.policy[repoKey] = catchUpPolicyMemo{until: now.Add(catchUpPolicyMemoTTL), reason: reason}
	return true
}
