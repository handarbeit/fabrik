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

// Bounds on the red-verdict failing-check section (#2116, ADR-2116): the file
// is read by workers on every invocation, so it stays a few KB. Per-check detail
// reuses the CI-fix prompt's bounds (ADR-1991); annotations cost one REST call
// per check, so only the first ciStatusMaxAnnotated failing checks are read.
const (
	ciStatusMaxFailing   = 10
	ciStatusMaxAnnotated = 5
)

// ciStatusExtras is the part of ci-status.md beyond the verdict (#2116).
type ciStatusExtras struct {
	// gateAhead is true when a later stage than the current one waits for CI.
	gateAhead bool
	// failing lists the failing check runs (red only), capped at
	// ciStatusMaxFailing; omitted counts those beyond the cap.
	failing []ciStatusFailingCheck
	omitted int
	// redWithoutRun is true for a red verdict with no identifiable failing run
	// (for example a required status context failed).
	redWithoutRun bool
}

// ciStatusFailingCheck is one entry of the "Failing checks" section.
type ciStatusFailingCheck struct {
	run    gh.CheckRun
	detail string // already indented; "" or begins with a newline
}

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

	gated := stageWaitsForCI(currentStage)
	extras := &ciStatusExtras{gateAhead: e.laterStageWaitsForCI(currentStage)}

	pr, err := e.client.FetchPRDetails(owner, repo, prNumber)
	if err != nil || pr == nil {
		e.logf(item.Number, "warn", "ci-status: could not read PR #%d details: %v\n", prNumber, err)
		e.emitCIStatus(item, path, prNumber, "", ciVerdictUnknown, gated, nil, extras)
		return
	}
	if pr.State != "open" || pr.Merged {
		// A closed or merged PR's head is moot.
		removeStale()
		return
	}
	if pr.HeadSHA == "" {
		e.emitCIStatus(item, path, prNumber, "", ciVerdictUnknown, gated, nil, extras)
		return
	}

	runs, err := e.client.FetchCheckRuns(owner, repo, pr.HeadSHA)
	if err != nil {
		e.logf(item.Number, "warn", "ci-status: could not read check runs for %s: %v\n", pr.HeadSHA, err)
		e.emitCIStatus(item, path, prNumber, pr.HeadSHA, ciVerdictUnknown, gated, nil, extras)
		return
	}

	verdict := e.ciStatusVerdictFor(owner, repo, pr.MergeableState, pr.HeadSHA, runs)
	if verdict == ciVerdictRed {
		e.collectFailingChecks(item, owner, repo, runs, extras)
	}
	e.emitCIStatus(item, path, prNumber, pr.HeadSHA, verdict, gated, runs, extras)
}

// stageWaitsForCI reports whether st has the wait_for_ci gate.
func stageWaitsForCI(st *stages.Stage) bool {
	return st != nil && st.WaitForCI != nil && *st.WaitForCI
}

// laterStageWaitsForCI reports whether any managed stage ordered after current
// has wait_for_ci, i.e. whether a push from the current stage still meets a CI
// gate before the item can land (#2116). It is a static property of the stage
// config; holding, unmanaged and cleanup stages never gate.
func (e *Engine) laterStageWaitsForCI(current *stages.Stage) bool {
	if current == nil {
		return false
	}
	for _, st := range e.cfg.Stages {
		if st == nil || st.HoldingStage || st.Unmanaged || st.CleanupWorktree {
			continue
		}
		if st.Order > current.Order && stageWaitsForCI(st) {
			return true
		}
	}
	return false
}

// collectFailingChecks fills extras for a red head: the latest run of each check
// name that failed, with the CI-fix prompt's rendering of its output. Annotation
// reads are live, soft-failing and capped; they use checks:read, which the
// engine already holds, not the optional actions permission.
func (e *Engine) collectFailingChecks(item gh.ProjectItem, owner, repo string, runs []gh.CheckRun, extras *ciStatusExtras) {
	failing := failingCheckRuns(runs)
	if len(failing) == 0 {
		extras.redWithoutRun = true
		return
	}
	for i, cr := range failing {
		if i >= ciStatusMaxFailing {
			extras.omitted = len(failing) - ciStatusMaxFailing
			break
		}
		extras.failing = append(extras.failing, ciStatusFailingCheck{
			run:    cr,
			detail: e.checkRunFailureDetail(item, "ci-status", owner, repo, cr, i < ciStatusMaxAnnotated),
		})
	}
}

// failingCheckRuns returns the latest run of each check name that
// gh.ClassifyCheckRuns reports as failed (failure, timed_out, action_required)
// followed by those it reports as non-passing (cancelled, stale, empty,
// unrecognised — #2296), so this section and the verdict cannot disagree about
// what is not green. Each is listed under its real conclusion.
func failingCheckRuns(runs []gh.CheckRun) []gh.CheckRun {
	_, _, failed := gh.ClassifyCheckRuns(runs)
	return append(append([]gh.CheckRun(nil), failed...), gh.NonPassingCheckRuns(runs)...)
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
	case TrainCIRed, TrainCINotPassed:
		// #2296: a cancelled/stale/unrecognised latest run is red for the worker
		// too, so the status file never disagrees with the gates. The entries are
		// listed under their real conclusion (failingCheckRuns).
		return ciVerdictRed
	case TrainCIGreen:
		// A cancelled/stale/unrecognised conclusion no longer classifies as ready
		// (#2296: TrainCINotPassed → red above). What is left to demote is
		// skipped: it is a pass for the gates, but "green" here means a check
		// actually ran, so a skipped latest run must never license a local test
		// skip. Demote unless every latest run is success or neutral.
		if !allCheckRunsPassed(runs) {
			return ciVerdictPending
		}
		return ciVerdictGreen
	default:
		return ciVerdictPending
	}
}

// allCheckRunsPassed reports whether the latest run of every check name is
// completed with a passing conclusion (success or neutral). skipped is
// deliberately not passing: a path-filtered or conditional test job reports
// skipped without having run the suite on this head, so it must never license a
// local skip. Demoting it only ever removes a skip.
func allCheckRunsPassed(runs []gh.CheckRun) bool {
	for _, cr := range gh.LatestCheckRuns(runs) {
		if cr.Status != "completed" {
			return false
		}
		switch cr.Conclusion {
		case "success", "neutral":
		default:
			return false
		}
	}
	return true
}

// emitCIStatus renders and writes the file, then logs one line.
func (e *Engine) emitCIStatus(item gh.ProjectItem, path string, prNumber int, headSHA string, verdict ciStatusVerdict, gated bool, runs []gh.CheckRun, extras *ciStatusExtras) {
	body := renderCIStatus(prNumber, headSHA, verdict, gated, e.now(), runs, extras)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		e.logf(item.Number, "warn", "could not write .fabrik-context/%s: %v\n", ciStatusFile, err)
		return
	}
	e.logf(item.Number, "ci-status", "head=%s verdict=%s gated=%t gate_ahead=%t failing=%d\n", headSHA, verdict, gated, extras.gateAhead, len(extras.failing))
}

// renderCIStatus builds the ci-status.md body: simple key: value lines a worker
// can read reliably, a short reading guide, the failing checks (red only) and a
// table of every check run. extras may be nil.
func renderCIStatus(prNumber int, headSHA string, verdict ciStatusVerdict, gated bool, at time.Time, runs []gh.CheckRun, extras *ciStatusExtras) string {
	if extras == nil {
		extras = &ciStatusExtras{}
	}
	var b strings.Builder
	b.WriteString("# CI Status\n\n")
	fmt.Fprintf(&b, "pr: %d\n", prNumber)
	fmt.Fprintf(&b, "head_sha: %s\n", headSHA)
	fmt.Fprintf(&b, "verdict: %s\n", verdict)
	fmt.Fprintf(&b, "ci_gated: %t\n", gated)
	fmt.Fprintf(&b, "ci_gate_ahead: %t\n", extras.gateAhead)
	fmt.Fprintf(&b, "written_at: %s\n\n", at.UTC().Format(time.RFC3339))
	b.WriteString("This is a snapshot taken when this invocation started.\n\n")
	b.WriteString("- A local full-suite run may be skipped only when ALL of these hold: `verdict` is\n")
	b.WriteString("  `green`, `git rev-parse HEAD` equals `head_sha`, and `git status --porcelain` is empty.\n")
	b.WriteString("- When `verdict` is `red`, reproduce only the checks under \"Failing checks\", not the\n")
	b.WriteString("  full suite.\n")
	b.WriteString("- `ci_gated` is true when this stage waits for CI; `ci_gate_ahead` is true when a later\n")
	b.WriteString("  stage does. Either means CI re-checks the head you push.\n\n")
	if verdict == ciVerdictRed {
		renderFailingChecks(&b, extras)
	}
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

// renderFailingChecks writes the "Failing checks" section of a red verdict.
func renderFailingChecks(b *strings.Builder, extras *ciStatusExtras) {
	b.WriteString("## Failing checks\n\n")
	if len(extras.failing) == 0 {
		b.WriteString("(no failing check run identified; a required status may have failed — see the table below)\n\n")
		return
	}
	for _, f := range extras.failing {
		name := strings.ReplaceAll(f.run.Name, "\n", " ")
		fmt.Fprintf(b, "### %s\n\n- conclusion: %s\n", name, nonPassingConclusion(f.run))
		if !gh.IsFailureConclusion(f.run.Conclusion) {
			fmt.Fprintf(b, "- note: %s\n", nonPassingNote)
		}
		if f.run.DetailsURL != "" {
			fmt.Fprintf(b, "- details: %s\n", f.run.DetailsURL)
		} else if f.run.HTMLURL != "" {
			fmt.Fprintf(b, "- details: %s\n", f.run.HTMLURL)
		}
		if f.detail != "" {
			b.WriteString("- output:" + strings.ReplaceAll(f.detail, "\n", "\n  ") + "\n")
		}
		b.WriteString("\n")
	}
	if extras.omitted > 0 {
		fmt.Fprintf(b, "(%d more failing check(s) omitted; see the table below)\n\n", extras.omitted)
	}
}
