package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// ciStatusFile is the name of the CI status context file inside
// .fabrik-context/ (#1997, ADR-1997).
const ciStatusFile = "ci-status.md"

// ciStatusVerdict is the verdict recorded in ci-status.md for a PR head.
type ciStatusVerdict string

const (
	ciVerdictGreen   ciStatusVerdict = "green"   // complete and passing
	ciVerdictRed     ciStatusVerdict = "red"     // a check (or required context) failed
	ciVerdictPending ciStatusVerdict = "pending" // incomplete, including a check-suite hold
	ciVerdictNone    ciStatusVerdict = "none"    // no check runs on the head
	ciVerdictUnknown ciStatusVerdict = "unknown" // a read failed
)

// writeCIStatus writes .fabrik-context/ci-status.md for an item with a linked
// open PR (#1997): the PR number, its live head SHA and the engine's existing
// green-and-complete verdict for that head, so a worker can skip a local
// full-suite run that CI has already performed on exactly that SHA.
//
// All reads are live and uncached (e.client, never e.readClient), the same form
// the landing paths use. The verdict is conservative by construction: any read
// error is unknown, zero check runs is none (never the ADR-933 mergeable_state
// green), and the classifier's green is demoted to pending unless every latest
// run actually passed. Only "green" ever licenses a skip.
//
// With no linked open PR no file is written and any stale file from an earlier
// invocation is removed, so a worker can never read a leftover "green". Errors
// are non-fatal: log and continue, like the rest of writeContextFiles.
func (e *Engine) writeCIStatus(item gh.ProjectItem, currentStage *stages.Stage, fabrikDir string) {
	path := filepath.Join(fabrikDir, ciStatusFile)
	removeStale := func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			e.logf(item.Number, "warn", "could not remove stale .fabrik-context/%s: %v\n", ciStatusFile, err)
		}
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	prNumber, err := e.client.FindPRForIssue(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "ci-status: could not find linked PR: %v\n", err)
		removeStale()
		return
	}
	if prNumber == 0 {
		removeStale()
		return
	}

	gated := currentStage != nil && currentStage.WaitForCI != nil && *currentStage.WaitForCI

	pr, err := e.client.FetchPRDetails(owner, repo, prNumber)
	if err != nil || pr == nil {
		e.logf(item.Number, "warn", "ci-status: could not read PR #%d details: %v\n", prNumber, err)
		e.emitCIStatus(item, path, prNumber, "", ciVerdictUnknown, gated, nil)
		return
	}
	if pr.State != "open" || pr.Merged {
		// A closed or merged PR's head is moot.
		removeStale()
		return
	}
	if pr.HeadSHA == "" {
		e.emitCIStatus(item, path, prNumber, "", ciVerdictUnknown, gated, nil)
		return
	}

	runs, err := e.client.FetchCheckRuns(owner, repo, pr.HeadSHA)
	if err != nil {
		e.logf(item.Number, "warn", "ci-status: could not read check runs for %s: %v\n", pr.HeadSHA, err)
		e.emitCIStatus(item, path, prNumber, pr.HeadSHA, ciVerdictUnknown, gated, nil)
		return
	}

	verdict := e.ciStatusVerdictFor(owner, repo, pr.MergeableState, pr.HeadSHA, runs)
	e.emitCIStatus(item, path, prNumber, pr.HeadSHA, verdict, gated, runs)
}

// ciStatusVerdictFor maps a head's check runs to a verdict using the landing
// paths' classifyLandingCI unmodified, plus two writer-local guards that only
// ever remove a skip.
func (e *Engine) ciStatusVerdictFor(owner, repo, mergeableState, headSHA string, runs []gh.CheckRun) ciStatusVerdict {
	// Zero check runs is never evidence the suite ran on this head, whatever
	// mergeable_state says (singletonFastPathEligible rejects it the same way).
	if len(runs) == 0 {
		return ciVerdictNone
	}
	result, _ := e.classifyLandingCI(owner, repo, mergeableState, headSHA, runs)
	switch result {
	case TrainCIRed:
		return ciVerdictRed
	case TrainCIGreen:
		// gh.ClassifyCheckRuns only fails failure/timed_out/action_required, so
		// a cancelled or unrecognised conclusion classifies as ready. "green"
		// means complete and passing, so demote unless every latest run passed.
		if !allCheckRunsPassed(runs) {
			return ciVerdictPending
		}
		return ciVerdictGreen
	default:
		return ciVerdictPending
	}
}

// allCheckRunsPassed reports whether the latest run of every check name is
// completed with a passing conclusion (success, neutral or skipped).
func allCheckRunsPassed(runs []gh.CheckRun) bool {
	latest := make(map[string]gh.CheckRun, len(runs))
	for _, cr := range runs {
		if existing, ok := latest[cr.Name]; !ok || cr.ID > existing.ID {
			latest[cr.Name] = cr
		}
	}
	for _, cr := range latest {
		if cr.Status != "completed" {
			return false
		}
		switch cr.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return false
		}
	}
	return true
}

// emitCIStatus renders and writes the file, then logs one line.
func (e *Engine) emitCIStatus(item gh.ProjectItem, path string, prNumber int, headSHA string, verdict ciStatusVerdict, gated bool, runs []gh.CheckRun) {
	body := renderCIStatus(prNumber, headSHA, verdict, gated, e.now(), runs)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik-context/%s: %v\n", ciStatusFile, err)
		return
	}
	e.logf(item.Number, "ci-status", "head=%s verdict=%s gated=%t\n", headSHA, verdict, gated)
}

// renderCIStatus builds the ci-status.md body: simple key: value lines a worker
// can read reliably, then a table of every check run.
func renderCIStatus(prNumber int, headSHA string, verdict ciStatusVerdict, gated bool, at time.Time, runs []gh.CheckRun) string {
	var b strings.Builder
	b.WriteString("# CI Status\n\n")
	fmt.Fprintf(&b, "pr: %d\n", prNumber)
	fmt.Fprintf(&b, "head_sha: %s\n", headSHA)
	fmt.Fprintf(&b, "verdict: %s\n", verdict)
	fmt.Fprintf(&b, "ci_gated: %t\n", gated)
	fmt.Fprintf(&b, "written_at: %s\n\n", at.UTC().Format(time.RFC3339))
	b.WriteString("This is a snapshot taken when this invocation started. It licenses skipping a local\n")
	b.WriteString("full-suite run only when ALL of these hold: `verdict` is `green`, `git rev-parse HEAD`\n")
	b.WriteString("equals `head_sha`, and `git status --porcelain` is empty.\n\n")
	b.WriteString("## Check runs\n\n")
	if len(runs) == 0 {
		b.WriteString("(none)\n")
		return b.String()
	}
	b.WriteString("| name | status | conclusion |\n|---|---|---|\n")
	for _, cr := range runs {
		name := strings.ReplaceAll(strings.ReplaceAll(cr.Name, "|", "\\|"), "\n", " ")
		conclusion := cr.Conclusion
		if conclusion == "" {
			conclusion = "-"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, cr.Status, conclusion)
	}
	return b.String()
}
