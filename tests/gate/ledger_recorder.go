package gate

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/handarbeit/fabrik/tests/e2e/inconclusive"
)

// legRecorder turns the live `go test -json` event stream of one cell into
// ledger records AS THEY STREAM IN (R1). It is fed from suiteWriter.line, not
// from Gate.OnLeg: OnLeg fires only for a leg that reached a normal post-suite
// result, and a leg that is killed or suspended mid-way is exactly the case the
// ledger exists for. A test is recorded only on its own terminal pass / fail /
// skip event; a test still running (or never started) leaves no record, so it
// can never be read as a PASS. Subtests roll up into their parent's outcome.
type legRecorder struct {
	ledger     *Ledger
	leg        string // "auth/train"
	cell       string // archive-safe cell name
	invocation string
	head       string
	hashes     map[string]string // test -> source hash
	live       map[string]bool   // the live-test set; non-nil restricts what is recorded
	warn       func(format string, args ...any)

	mu       sync.Mutex
	output   map[string]*testOutput // top-level test -> its own output
	recorded int
	failed   int // ledger writes that failed
}

// maxRecordedOutput caps the per-test output kept to extract a skip message.
const maxRecordedOutput = 64 << 10

// maxMarkerOverflow bounds what testOutput keeps past maxRecordedOutput: only
// inconclusive-marker lines (and their continuations), so it stays small.
const maxMarkerOverflow = 16 << 10

// testOutput is one top-level test's own output, capped at maxRecordedOutput.
// The cap would otherwise hide the INCONCLUSIVE marker — t.Skipf writes it at
// the END of a test, after every poll-loop t.Logf — and silently turn an
// inconclusive test into an ordinary skip (no retry, no exit 8; #1973). So once
// the cap is reached, marker lines (plus their indented continuation lines) are
// still kept, in a separate small budget; everything else is dropped as before.
type testOutput struct {
	b        strings.Builder
	overflow int  // bytes kept past the cap
	cont     bool // the last kept overflow line was a marker line
}

func (o *testOutput) add(chunk string) {
	if o.b.Len() < maxRecordedOutput {
		o.b.WriteString(chunk)
		return
	}
	for _, l := range strings.SplitAfter(chunk, "\n") {
		if l == "" {
			continue
		}
		keep := false
		if m := skipLineRE.FindStringSubmatch(strings.TrimRight(l, "\n")); m != nil {
			keep = inconclusive.IsMarked(m[1])
			o.cont = keep
		} else if o.cont && (l[0] == ' ' || l[0] == '\t') && strings.TrimSpace(l) != "" {
			keep = true
		} else {
			o.cont = false
		}
		if keep && o.overflow < maxMarkerOverflow {
			o.overflow += len(l)
			o.b.WriteString(l)
		}
	}
}

func (o *testOutput) String() string { return o.b.String() }

func newLegRecorder(l *Ledger, cell Cell, invocation, head string, hashes map[string]string, live map[string]bool, warn func(string, ...any)) *legRecorder {
	return &legRecorder{
		ledger: l, leg: cell.Label(), cell: cellDirName(cell), invocation: invocation, head: head,
		hashes: hashes, live: live, warn: warn, output: map[string]*testOutput{},
	}
}

// cellDirName names a cell for the archive and for void scoping.
func cellDirName(c Cell) string {
	return c.Auth + "-" + c.Train
}

// Observe consumes one decoded event.
func (r *legRecorder) Observe(e Event) {
	if r == nil || !topLevel(e) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.Action == "output" {
		b := r.output[e.Test]
		if b == nil {
			b = &testOutput{}
			r.output[e.Test] = b
		}
		b.add(e.Output)
		return
	}
	if !terminal(e) {
		return
	}
	text := ""
	if b := r.output[e.Test]; b != nil {
		text = b.String()
		delete(r.output, e.Test)
	}
	if r.live != nil && !r.live[e.Test] {
		return
	}
	rec := Record{Test: e.Test, Leg: r.leg, Cell: r.cell, Invocation: r.invocation, Hash: r.hashes[e.Test], Head: r.head}
	switch e.Action {
	case "pass":
		rec.Outcome = OutcomePass
	case "fail":
		rec.Outcome = OutcomeFail
	case "skip":
		rec.SkipMsg = skipMessage(text)
		if msg := inconclusiveMessage(text); msg != "" {
			// #1973: "the precondition never arose" — uncovered, retried by the
			// leg, re-run by --resume; never a SKIP (so skip_ok_legs and the skip
			// classifier never see it) and never a PASS. The reason rides in
			// SkipMsg (the marker line, even when a cleanup logged after it).
			rec.SkipMsg = msg
			rec.Outcome = OutcomeInconclusive
		} else {
			rec.Outcome = OutcomeSkip
			rec.Issues = citedIssues(rec.SkipMsg)
		}
	}
	if err := r.ledger.Append(rec); err != nil {
		r.failed++
		r.warn("ledger: could not record %s on %s (it stays uncovered): %v\n", e.Test, r.leg, err)
		return
	}
	r.recorded++
}

// Counts reports how many records were written / failed to write.
func (r *legRecorder) Counts() (recorded, failed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorded, r.failed
}

var skipLineRE = regexp.MustCompile(`^\s+[\w.\-]+\.go:\d+: (.*)$`)

// skipMessage extracts the t.Skip message from a test's output: the last
// "file.go:N: text" log line (t.Skip logs its message last), plus any indented
// continuation lines. "" when the output has none.
//
// "Last" is only right for an ORDINARY skip: a t.Cleanup that logs after the
// skip (the bed-restart cleanups do) pushes its own line behind it. The
// INCONCLUSIVE marker therefore does not go through here — see
// inconclusiveMessage.
func skipMessage(out string) string {
	lines := strings.Split(out, "\n")
	idx := -1
	for i, l := range lines {
		if skipLineRE.MatchString(l) {
			idx = i
		}
	}
	if idx < 0 {
		return ""
	}
	return messageAt(lines, idx)
}

// inconclusiveMessage is the message of the first "file.go:N: text" log line in
// a test's own output that STARTS with the inconclusive marker, or "" when none
// does. It is order-independent on purpose: Cleanup functions run after t.Skip
// and may log after it, so the marker line is not necessarily the last log line
// (#1973). The marker must still start the message — a line that merely
// mentions it does not match — and callers only consult this for a test whose
// terminal action is "skip", so a test that logs the marker and then goes on to
// pass or fail is never inconclusive.
func inconclusiveMessage(out string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if m := skipLineRE.FindStringSubmatch(l); m != nil && inconclusive.IsMarked(m[1]) {
			return messageAt(lines, i)
		}
	}
	return ""
}

// messageAt is the log line at idx plus its indented continuation lines.
func messageAt(lines []string, idx int) string {
	msg := skipLineRE.FindStringSubmatch(lines[idx])[1]
	for _, l := range lines[idx+1:] {
		if strings.HasPrefix(l, "---") || strings.HasPrefix(l, "===") || strings.TrimSpace(l) == "" {
			break
		}
		if l[0] != ' ' && l[0] != '\t' {
			break
		}
		msg += " " + strings.TrimSpace(l)
	}
	return strings.TrimSpace(msg)
}

// issueRefRE matches a bare "#N" — not the tail of a path or anchor such as
// "README.md#17-foo", which names a document section, not an issue.
var issueRefRE = regexp.MustCompile(`(?:^|[^\w./])#(\d+)\b`)

// citedIssues returns the distinct issue numbers a skip message cites ("#916").
func citedIssues(msg string) []int {
	var out []int
	seen := map[int]bool{}
	for _, m := range issueRefRE.FindAllStringSubmatch(msg, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
