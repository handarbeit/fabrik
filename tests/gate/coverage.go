package gate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/handarbeit/fabrik/tests/e2e/registry"
)

// PairStatus is the coverage state of one required (test, leg) pair.
type PairStatus string

const (
	PairCovered      PairStatus = "covered"         // a valid PASS for the current source hash
	PairKnownSkip    PairStatus = "known-skip"      // skipped, citing a still-open issue
	PairStructural   PairStatus = "structural-skip" // skipped by design on this leg (registry)
	PairInconclusive PairStatus = "inconclusive"    // INCONCLUSIVE (#1973) — never accepted
	PairMissing      PairStatus = "missing"         // anything else: no record, FAIL, stale hash, bad skip
)

// Resolved reports whether the pair needs no further run: covered, or a skip the
// ledger accepts. Only PairCovered counts as covered in the summary; the skips
// are listed separately and merely do not block.
func (s PairStatus) Resolved() bool {
	return s == PairCovered || s == PairKnownSkip || s == PairStructural
}

// Pair is one evaluated (test, leg).
type Pair struct {
	Test, Leg string
	Status    PairStatus
	Detail    string
}

// Evaluator judges (leg, test) pairs against a ledger snapshot.
type Evaluator struct {
	Snap    *Snapshot
	Hashes  map[string]string         // test -> current source hash
	Entries map[string]registry.Entry // test -> registry entry (skip_ok_legs)
	States  IssueStates
}

// Eval applies the coverage rule: covered iff the latest non-voided record is a
// PASS recorded against the test's CURRENT source hash on a trusted leg file.
// Every other shape — no record, a changed hash, FAIL, a skip the ledger does not
// accept, INCONCLUSIVE, an outcome it does not recognise — is not covered.
func (e *Evaluator) Eval(ctx context.Context, leg, test string) Pair {
	p := Pair{Test: test, Leg: leg, Status: PairMissing}
	hash := e.Hashes[test]
	if hash == "" {
		p.Detail = "no source hash (test not found in tests/e2e)"
		return p
	}
	if why, bad := e.Snap.Corrupt[leg]; bad {
		p.Detail = "ledger file for this leg is corrupt (" + why + ")"
		return p
	}
	rec, ok := e.Snap.Latest[leg][test]
	if !ok {
		p.Detail = "no record"
		return p
	}
	if rec.Hash != hash {
		p.Detail = fmt.Sprintf("test source changed since the %s was recorded", rec.Outcome)
		return p
	}
	switch rec.Outcome {
	case OutcomePass:
		p.Status = PairCovered
	case OutcomeFail:
		p.Detail = "last run FAILED"
	case OutcomeInconclusive:
		p.Status, p.Detail = PairInconclusive, "last run was INCONCLUSIVE"
	case OutcomeSkip:
		sc := ClassifySkip(ctx, rec, e.Entries[test].SkipOK(leg), e.States)
		switch sc.Kind {
		case SkipKnown:
			p.Status = PairKnownSkip
		case SkipStructural:
			p.Status = PairStructural
		}
		p.Detail = sc.Reason
		if sc.Kind == SkipMissing && rec.SkipMsg != "" {
			p.Detail += fmt.Sprintf(" — skip message: %q", rec.SkipMsg)
		} else if sc.Kind == SkipKnown {
			p.Detail += fmt.Sprintf(" — %q", rec.SkipMsg)
		}
	default:
		p.Detail = fmt.Sprintf("unrecognised outcome %q", rec.Outcome)
	}
	return p
}

// LegReport is every required pair of one leg.
type LegReport struct {
	Leg   string
	Pairs []Pair
}

func (l LegReport) count(st PairStatus) int {
	n := 0
	for _, p := range l.Pairs {
		if p.Status == st {
			n++
		}
	}
	return n
}

// Report is the coverage summary for one engine SHA.
type Report struct {
	SHA         string
	Legs        []LegReport
	Drift       *Drift // nil when no drift check was run
	Pregate     string // informational pre-gate status line ("" omits it)
	Invocations int
	Warnings    []string
}

// BuildReport evaluates every required pair. legs gives the order; required maps
// each leg to its required tests.
func BuildReport(ctx context.Context, ev *Evaluator, sha string, legs []string, required map[string][]string) *Report {
	r := &Report{SHA: sha}
	for _, leg := range legs {
		lr := LegReport{Leg: leg}
		for _, t := range required[leg] {
			lr.Pairs = append(lr.Pairs, ev.Eval(ctx, leg, t))
		}
		r.Legs = append(r.Legs, lr)
	}
	r.Warnings = append(r.Warnings, ev.Snap.Warnings...)
	for leg, why := range ev.Snap.Corrupt {
		r.Warnings = append(r.Warnings, fmt.Sprintf("leg %s: ledger file distrusted: %s", leg, why))
	}
	sort.Strings(r.Warnings)
	return r
}

// Complete reports whether the gate's live coverage is accepted: every required
// pair resolved, and — when a drift check ran — the ledger valid for the tree.
func (r *Report) Complete() bool {
	if r.Drift != nil && !r.Drift.Valid {
		return false
	}
	if len(r.Legs) == 0 {
		return false
	}
	for _, l := range r.Legs {
		for _, p := range l.Pairs {
			if !p.Status.Resolved() {
				return false
			}
		}
	}
	return true
}

// Totals sums the statuses across legs.
func (r *Report) Totals() (covered, missing, known, structural, inconclusive int) {
	for _, l := range r.Legs {
		covered += l.count(PairCovered)
		missing += l.count(PairMissing)
		known += l.count(PairKnownSkip)
		structural += l.count(PairStructural)
		inconclusive += l.count(PairInconclusive)
	}
	return
}

// Format is the human summary: per leg covered / missing / known-skip /
// structural / inconclusive, then each non-covered pair with why.
func (r *Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "== live coverage for engine SHA %s ==\n", shortSHA(r.SHA))
	if r.Drift != nil {
		fmt.Fprintf(&b, "%s\n", r.Drift.Report())
	}
	if r.Pregate != "" {
		fmt.Fprintf(&b, "%s\n", r.Pregate)
	}
	for _, l := range r.Legs {
		fmt.Fprintf(&b, "  %-8s covered %d/%d  missing %d  known-skip %d  structural-skip %d  inconclusive %d\n",
			l.Leg, l.count(PairCovered), len(l.Pairs), l.count(PairMissing), l.count(PairKnownSkip), l.count(PairStructural), l.count(PairInconclusive))
		for _, p := range l.Pairs {
			if p.Status == PairCovered {
				continue
			}
			fmt.Fprintf(&b, "    %-16s %s: %s\n", p.Status, p.Test, p.Detail)
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	cov, miss, known, structural, inc := r.Totals()
	verdict := "COMPLETE"
	if !r.Complete() {
		verdict = "INCOMPLETE"
	}
	fmt.Fprintf(&b, "== coverage %s: %d covered, %d missing, %d known-skip, %d structural-skip, %d inconclusive; %d invocation(s) ==",
		verdict, cov, miss, known, structural, inc, r.Invocations)
	return b.String()
}

// NotesLine is the release-notes line: what was gated, and how many invocations
// the coverage took (R5).
func (r *Report) NotesLine() string {
	cov, _, known, structural, _ := r.Totals()
	n := r.Invocations
	plural := "s"
	if n == 1 {
		plural = ""
	}
	return fmt.Sprintf("Live e2e gate: coverage complete for engine SHA %s (%d test/leg pairs passed, %d known skip(s), %d structural skip(s)) across %d gate invocation%s.",
		r.SHA, cov, known, structural, n, plural)
}
