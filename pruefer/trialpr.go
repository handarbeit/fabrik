package pruefer

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

	gh "github.com/handarbeit/fabrik/github"
)

// Merge-train trial and integration PRs (#2230 R2,
// adrs/2230-pruefer-patch-identity-skip.md).
//
// The Fabrik engine's merge train assembles Queued member PRs into a disposable
// branch `fabrik/merge-train/merge-train-<base>-<unix-ts>[-t<n>]` and opens a PR
// for it. Its patch is the members' patches combined, plus any edits made while
// resolving a merge conflict. Everything in the first group was already
// reviewed on the member PRs; only the second group is new.
//
// Recognition is positive and needs both: the PR is authored by a login on the
// operator-configured engine-login list (the same list that anchors #2066's
// catch-up markers, cfg.CatchUpMarkerAuthors) and its head branch has the
// engine's trial naming. Only then is the PR body — which the engine wrote —
// trusted to name the members (its `Closes #<issue>` lines).
//
// Equivalence is decided per file, from diff text alone: a trial file is
// "covered" when exactly one member touches it, that member's patch was
// reviewed (a completed Pruefer review of an identical patch), and the trial's
// per-file patch equals the member's. Every file covered → skip. Some covered →
// the covered files are withheld from this review (disclosed to the model, and
// never anchorable) and only the rest — the conflict-resolution edits and files
// two members share — are reviewed. Anything that cannot be established
// (unresolvable member, unreviewed member, failed read) → review as today.

// trialBranchRE mirrors the engine's trial branch naming
// (engine/worktree.go trainBranchPrefix + engine/merge_train.go
// trialSuffixPattern); the sanitized base may itself contain hyphens.
// engine/trial_branch_contract_test.go renders a branch with the engine's own
// code and matches it here, so a naming change there fails CI instead of
// silently disabling the skip.
var trialBranchRE = regexp.MustCompile(`^fabrik/merge-train/merge-train-.+-[0-9]+(-t[0-9]+)?$`)

// trialClosesRE matches the `Closes #<issue>` lines the engine writes into
// trial and integration PR bodies, one member issue per line.
var trialClosesRE = regexp.MustCompile(`(?m)^Closes #([0-9]+)[ \t]*\r?$`)

// maxTrialMembers bounds the member lookups one trial PR can cost.
const maxTrialMembers = 20

// memberBranchPrefix is the engine's per-issue branch namespace.
const memberBranchPrefix = "fabrik/issue-"

// isTrialPR reports whether pr is an engine-authored trial/integration PR:
// authored by a configured engine login AND carrying the trial branch naming.
func isTrialPR(cfg Config, pr gh.PRDetails) bool {
	return loginIn(cfg.CatchUpMarkerAuthors, pr.Author) && trialBranchRE.MatchString(pr.HeadRefName)
}

// trialMemberIssues returns the member issue numbers named by body's
// `Closes #N` lines, in order, without duplicates.
func trialMemberIssues(body string) []int {
	var out []int
	seen := map[int]bool{}
	for _, m := range trialClosesRE.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// trialVerdict is trialCheck's result.
type trialVerdict struct {
	Skip     bool     // every file is covered by a reviewed member patch
	Covered  []string // when !Skip: files to withhold from the review (sorted); empty = review everything
	Detail   string
	Degraded bool
}

// trialMember is one resolved, reviewed member PR.
type trialMember struct {
	Issue   int
	PR      int
	PerFile map[string]string
}

// reviewedMemberPatch resolves issue to its open member PR and reports its
// per-file fingerprint iff Pruefer completed a review of an identical patch.
func reviewedMemberPatch(client GitHubReviewer, botLogin, owner, repo string, issue int) (m trialMember, ok, degraded bool, detail string) {
	prs, err := client.FindPRByHead(owner, repo, memberBranchPrefix+strconv.Itoa(issue))
	if err != nil {
		return m, false, true, fmt.Sprintf("looking up the PR for member #%d failed: %v", issue, err)
	}
	var member *gh.PRDetails
	for i := range prs {
		if prs[i].State != "open" {
			continue
		}
		if member != nil {
			return m, false, false, fmt.Sprintf("member #%d has more than one open PR", issue)
		}
		member = &prs[i]
	}
	if member == nil || member.HeadSHA == "" {
		return m, false, false, fmt.Sprintf("member #%d has no open PR", issue)
	}
	mdiff, err := client.FetchPRDiff(owner, repo, member.Number)
	if err != nil {
		return m, false, true, fmt.Sprintf("reading the diff of member PR #%d failed: %v", member.Number, err)
	}
	cur, fpOK := patchFingerprint(mdiff)
	if !fpOK {
		return m, false, false, fmt.Sprintf("member PR #%d has an empty or not comparable diff", member.Number)
	}
	reviews, err := client.FetchPRReviews(owner, repo, member.Number)
	if err != nil {
		return m, false, true, fmt.Sprintf("reading the reviews of member PR #%d failed: %v", member.Number, err)
	}
	last, has := lastBotReview(reviews, botLogin)
	if !has {
		return m, false, false, fmt.Sprintf("member PR #%d was never reviewed", member.Number)
	}
	if !reviewBodyComplete(last.Body) {
		return m, false, false, fmt.Sprintf("the last review of member PR #%d is not marked PRUEFER_COVERAGE: complete", member.Number)
	}
	if last.CommitID != member.HeadSHA {
		base := member.BaseRef
		if base == "" {
			return m, false, false, fmt.Sprintf("member PR #%d has no base branch", member.Number)
		}
		prev, prevOK, deg, d := fetchComparePrint(client, owner, repo, base, last.CommitID)
		if !prevOK {
			return m, false, deg, d
		}
		if prev.Whole != cur.Whole {
			return m, false, false, fmt.Sprintf("member PR #%d changed since its last review", member.Number)
		}
	}
	return trialMember{Issue: issue, PR: member.Number, PerFile: cur.PerFile}, true, false, ""
}

// trialCheck decides what to do with a recognised trial/integration PR whose
// current raw diff is diff. For a PR that is not recognised, or when the
// feature is off, it returns the zero verdict (review as today).
func trialCheck(client GitHubReviewer, cfg Config, botLogin, owner, repo string, pr gh.PRDetails, diff string) trialVerdict {
	if !cfg.PatchSkip || !isTrialPR(cfg, pr) || diff == "" {
		return trialVerdict{}
	}
	trial, ok := patchFingerprint(diff)
	if !ok {
		return trialVerdict{Detail: "the trial's change is empty or not comparable"}
	}
	issues := trialMemberIssues(pr.Body)
	if len(issues) == 0 {
		return trialVerdict{Detail: "the trial PR names no members"}
	}
	if len(issues) > maxTrialMembers {
		return trialVerdict{Detail: fmt.Sprintf("the trial PR names %d members (limit %d)", len(issues), maxTrialMembers)}
	}

	owners := map[string]int{} // path → members touching it
	var members []trialMember
	for _, n := range issues {
		m, ok, degraded, detail := reviewedMemberPatch(client, botLogin, owner, repo, n)
		if !ok {
			return trialVerdict{Detail: detail, Degraded: degraded}
		}
		members = append(members, m)
		for p := range m.PerFile {
			owners[p]++
		}
	}

	covered := map[string]bool{}
	for _, m := range members {
		for p, h := range m.PerFile {
			th, inTrial := trial.PerFile[p]
			if !inTrial {
				// A reviewed member change the trial no longer carries was
				// dropped while resolving a conflict — not the sum of the parts.
				return trialVerdict{Detail: fmt.Sprintf("member #%d's change to %s is absent from the trial", m.Issue, p)}
			}
			if owners[p] == 1 && th == h {
				covered[p] = true
			}
		}
	}
	if len(covered) == len(trial.PerFile) {
		return trialVerdict{Skip: true, Detail: fmt.Sprintf("every file of the trial is a reviewed patch of members %s", memberList(members))}
	}
	if len(covered) == 0 {
		return trialVerdict{Detail: "no file of the trial is covered by a reviewed member patch"}
	}
	paths := make([]string, 0, len(covered))
	for p := range covered {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return trialVerdict{
		Covered: paths,
		Detail: fmt.Sprintf("%d of %d file(s) are reviewed patches of members %s; reviewing the other %d",
			len(covered), len(trial.PerFile), memberList(members), len(trial.PerFile)-len(covered)),
	}
}

func memberList(ms []trialMember) string {
	s := ""
	for i, m := range ms {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("#%d(PR#%d)", m.Issue, m.PR)
	}
	return s
}

// withholdCovered splits blocks into those still to review and those withheld
// because covered names their path.
func withholdCovered(blocks []diffFileBlock, covered []string) (keep []diffFileBlock, withheld []string) {
	set := make(map[string]bool, len(covered))
	for _, p := range covered {
		set[p] = true
	}
	for _, b := range blocks {
		if set[b.Path] {
			withheld = append(withheld, b.Path)
			continue
		}
		keep = append(keep, b)
	}
	return keep, withheld
}

// IsTrialBranch reports whether headRef has the engine's merge-train trial
// naming. Exported only so engine's contract test can pin it against the
// engine's own branch construction (pruefer does not import engine).
func IsTrialBranch(headRef string) bool { return trialBranchRE.MatchString(headRef) }

// TrialMemberIssues is trialMemberIssues for the same contract test.
func TrialMemberIssues(body string) []int { return trialMemberIssues(body) }
