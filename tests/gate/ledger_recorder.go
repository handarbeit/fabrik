package gate

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	cell       string // archive-safe cell name (distinguishes the isolated cell)
	invocation string
	head       string
	hashes     map[string]string // test -> source hash
	live       map[string]bool   // the live-test set; non-nil restricts what is recorded
	warn       func(format string, args ...any)

	mu       sync.Mutex
	output   map[string]*strings.Builder // top-level test -> its own output
	recorded int
	failed   int // ledger writes that failed
}

// maxRecordedOutput caps the per-test output kept to extract a skip message.
const maxRecordedOutput = 64 << 10

func newLegRecorder(l *Ledger, cell Cell, invocation, head string, hashes map[string]string, live map[string]bool, warn func(string, ...any)) *legRecorder {
	return &legRecorder{
		ledger: l, leg: cell.Label(), cell: cellDirName(cell), invocation: invocation, head: head,
		hashes: hashes, live: live, warn: warn, output: map[string]*strings.Builder{},
	}
}

// cellDirName names a cell for the archive and for void scoping: the two "on"
// cells of one auth mode share a leg label but not a cell name.
func cellDirName(c Cell) string {
	n := c.Auth + "-" + c.Train
	if c.Isolated {
		n += "-isolated"
	}
	return n
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
			b = &strings.Builder{}
			r.output[e.Test] = b
		}
		if b.Len() < maxRecordedOutput {
			b.WriteString(e.Output)
		}
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
		if isInconclusiveSkip(text) {
			// #1973: "the precondition never arose" — uncovered, retried by the
			// leg, re-run by --resume; never a SKIP (so skip_ok_legs and the skip
			// classifier never see it) and never a PASS. The reason rides in
			// SkipMsg.
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
