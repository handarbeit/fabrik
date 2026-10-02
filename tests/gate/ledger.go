package gate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The per-SHA coverage ledger (#1972, ADR-1972). The live gate is coverage-based,
// not invocation-based: every live scenario test's terminal outcome is recorded,
// per leg, under the engine SHA being tested, so partial runs add up to a
// complete gate.
//
// Layout (all under Root, git-ignored):
//
//	<sha>/meta.json                    {version, sha, created, last_used}
//	<sha>/invocations.jsonl            one line per invocation that started a leg
//	<sha>/bedconfig.json               invocation -> bed config hash
//	<sha>/outcomes/<auth>-<train>.jsonl   append-only, one file per leg LABEL
//	<sha>/archive/<cell>/<ts>/         bulky logs (pruned by retention; archive.go)
//	pregate/<head>.json                pre-gate pass for a clean RepoRoot HEAD
//
// Outcome files are append-only JSONL fsynced per record, so a leg killed
// mid-way keeps every test that had already reached a terminal event, and a
// torn tail line is skipped (never read as a PASS). One file per leg plus
// append-only writes means concurrent legs (#1976/#1977) never share a document.

// LedgerVersion is the on-disk format version. A reader that meets a record with
// a different version treats that leg's file as corrupt (fails closed).
const LedgerVersion = 1

// Outcome is a test's recorded result. It is an open string enum: later issues
// add values (INCONCLUSIVE, #1973) without a format break, and a reader treats
// any value it does not know as NOT covered.
type Outcome string

const (
	OutcomePass Outcome = "PASS"
	OutcomeFail Outcome = "FAIL"
	OutcomeSkip Outcome = "SKIP"
	// OutcomeInconclusive is reserved for #1973; nothing emits it yet.
	OutcomeInconclusive Outcome = "INCONCLUSIVE"
)

const recordKindVoid = "void"

// Record is one line of an outcome file.
type Record struct {
	V int `json:"v"`
	// Kind is "" for an outcome record or "void": a void line discards every
	// earlier record of the same (Invocation, Cell) — a leg whose verdict cannot
	// be trusted (RUN INVALID).
	Kind       string  `json:"kind,omitempty"`
	Test       string  `json:"test,omitempty"`
	Leg        string  `json:"leg"`
	Cell       string  `json:"cell"`
	Invocation string  `json:"invocation"`
	Outcome    Outcome `json:"outcome,omitempty"`
	// Hash is the test's source hash when recorded (ledger_hash.go).
	Hash string `json:"hash,omitempty"`
	Head string `json:"head,omitempty"` // RepoRoot HEAD at record time
	TS   string `json:"ts"`
	// SkipMsg and Issues are set on SKIP records: the t.Skip message and the
	// issue numbers it cites (R6).
	SkipMsg string `json:"skip_msg,omitempty"`
	Issues  []int  `json:"issues,omitempty"`
}

// ledgerMeta is meta.json.
type ledgerMeta struct {
	Version  int    `json:"version"`
	SHA      string `json:"sha"`
	Created  string `json:"created"`
	LastUsed string `json:"last_used"`
}

// invocationLine is one line of invocations.jsonl.
type invocationLine struct {
	V          int    `json:"v"`
	Invocation string `json:"invocation"`
	TS         string `json:"ts"`
	Head       string `json:"head,omitempty"`
	Resume     bool   `json:"resume,omitempty"`
}

// Ledger is the per-SHA ledger directory. The zero value is unusable; a nil
// *Ledger is accepted by every recording hook and records nothing.
type Ledger struct {
	Root string // the coverage root (E2E_COVERAGE_DIR)
	SHA  string // the full engine SHA

	now func() time.Time
	mu  sync.Mutex
}

// legFile turns a leg label ("app/off") into its outcome file base name.
func legFile(label string) string { return strings.ReplaceAll(label, "/", "-") }

// legFromFile is the inverse of legFile for the auth-train pair.
func legFromFile(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	if i := strings.IndexByte(name, '-'); i > 0 {
		return name[:i] + "/" + name[i+1:]
	}
	return name
}

// OpenLedger creates (or reopens) the ledger for sha under root and stamps its
// meta.json. A directory that cannot be created or written is an error the
// caller treats as fatal before any leg runs.
func OpenLedger(root, sha string, now func() time.Time) (*Ledger, error) {
	if sha == "" {
		return nil, errors.New("ledger: empty engine SHA")
	}
	if now == nil {
		now = time.Now
	}
	l := &Ledger{Root: root, SHA: sha, now: now}
	if err := os.MkdirAll(l.outcomesDir(), 0o755); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	meta := ledgerMeta{Version: LedgerVersion, SHA: sha, Created: l.ts(), LastUsed: l.ts()}
	if old, err := l.readMeta(); err == nil && old.Created != "" {
		meta.Created = old.Created
	}
	if err := writeFileAtomic(filepath.Join(l.Dir(), "meta.json"), mustJSON(meta)); err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	return l, nil
}

// Dir is this SHA's ledger directory.
func (l *Ledger) Dir() string { return filepath.Join(l.Root, l.SHA) }

func (l *Ledger) outcomesDir() string { return filepath.Join(l.Dir(), "outcomes") }

// ArchiveDir is where this SHA's bulky per-leg logs live.
func (l *Ledger) ArchiveDir() string { return filepath.Join(l.Dir(), "archive") }

func (l *Ledger) ts() string { return l.now().UTC().Format(time.RFC3339) }

func (l *Ledger) readMeta() (ledgerMeta, error) {
	var m ledgerMeta
	data, err := os.ReadFile(filepath.Join(l.Dir(), "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // plain structs of strings and ints cannot fail
	}
	return append(b, '\n')
}

// writeFileAtomic writes data to path through a temp file and a rename, so a
// kill mid-write never leaves a half-written file under the final name.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// appendLine appends one JSON line to path and fsyncs it. If a previous writer
// was killed mid-line (the file does not end in a newline) the torn fragment is
// terminated first, so the new record never glues onto it.
func appendLine(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return err
	}
	return f.Sync()
}

// Append records one outcome. It is safe for concurrent use. An error means the
// outcome was NOT durably recorded; the caller logs it loudly and the test stays
// uncovered.
func (l *Ledger) Append(rec Record) error {
	if l == nil {
		return nil
	}
	rec.V = LedgerVersion
	if rec.TS == "" {
		rec.TS = l.ts()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return appendLine(filepath.Join(l.outcomesDir(), legFile(rec.Leg)+".jsonl"), rec)
}

// Void discards every earlier record of (invocation, cell) on the leg — the
// RUN INVALID reading of "this verdict cannot be trusted".
func (l *Ledger) Void(leg, cell, invocation string) error {
	return l.Append(Record{Kind: recordKindVoid, Leg: leg, Cell: cell, Invocation: invocation})
}

// NoteInvocation writes the invocation line (once per invocation — callers guard
// with their own sync.Once) and refreshes meta.last_used. An invocation that
// starts no leg never calls this, so a zero-leg resume is not counted.
func (l *Ledger) NoteInvocation(invocation, head string, resume bool) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := appendLine(filepath.Join(l.Dir(), "invocations.jsonl"),
		invocationLine{V: LedgerVersion, Invocation: invocation, TS: l.ts(), Head: head, Resume: resume}); err != nil {
		return err
	}
	return l.touch()
}

// Touch refreshes meta.last_used (retention orders SHAs by it).
func (l *Ledger) Touch() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.touch()
}

func (l *Ledger) touch() error {
	m, err := l.readMeta()
	if err != nil {
		m = ledgerMeta{Version: LedgerVersion, SHA: l.SHA, Created: l.ts()}
	}
	m.LastUsed = l.ts()
	return writeFileAtomic(filepath.Join(l.Dir(), "meta.json"), mustJSON(m))
}

// InvocationCount is how many invocations have started at least one leg.
func (l *Ledger) InvocationCount() int {
	data, err := os.ReadFile(filepath.Join(l.Dir(), "invocations.jsonl"))
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range bytes.Split(data, []byte("\n")) {
		var il invocationLine
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &il) == nil && il.Invocation != "" {
			n++
		}
	}
	return n
}

// Snapshot is the readable state of a ledger: the latest non-voided record per
// (leg, test), plus everything that was skipped or distrusted while reading.
type Snapshot struct {
	// Latest maps leg label -> test -> its latest non-voided record.
	Latest map[string]map[string]Record
	// Corrupt maps a leg label to why its whole file is distrusted (unknown
	// version, unreadable). Nothing on such a leg counts as covered.
	Corrupt map[string]string
	// Warnings are skipped (unparseable) lines and similar, reported loudly.
	Warnings []string
}

// Load reads every outcome file. A missing directory is an empty snapshot.
func (l *Ledger) Load() *Snapshot {
	s := &Snapshot{Latest: map[string]map[string]Record{}, Corrupt: map[string]string{}}
	entries, err := os.ReadDir(l.outcomesDir())
	if err != nil {
		return s
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		leg := legFromFile(e.Name())
		recs, warns, err := readRecords(filepath.Join(l.outcomesDir(), e.Name()))
		for _, w := range warns {
			s.Warnings = append(s.Warnings, fmt.Sprintf("%s: %s", e.Name(), w))
		}
		if err != nil {
			s.Corrupt[leg] = err.Error()
			continue
		}
		s.Latest[leg] = latestRecords(recs)
	}
	return s
}

// readRecords parses one outcome file. An unparseable line is skipped with a
// warning (it can only ever reduce coverage); a record of another format version
// makes the whole file an error (fail closed).
func readRecords(path string) ([]Record, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var recs []Record
	var warns []string
	br := bufio.NewReaderSize(f, 1<<16)
	for n := 1; ; n++ {
		line, rerr := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			var r Record
			switch jerr := json.Unmarshal([]byte(trimmed), &r); {
			case jerr != nil:
				torn := ""
				if rerr == io.EOF {
					torn = " (torn final line)"
				}
				warns = append(warns, fmt.Sprintf("line %d unparseable%s — skipped, never counted as covered", n, torn))
			case r.V != LedgerVersion:
				return nil, warns, fmt.Errorf("line %d has format version %d, this runner reads %d", n, r.V, LedgerVersion)
			default:
				recs = append(recs, r)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return recs, warns, nil
			}
			return nil, warns, rerr
		}
	}
}

// latestRecords applies void lines (each discards every earlier record of the
// same invocation+cell) and keeps the last remaining outcome record per test.
func latestRecords(recs []Record) map[string]Record {
	var live []Record
	for _, r := range recs {
		if r.Kind == recordKindVoid {
			kept := live[:0:0]
			for _, p := range live {
				if p.Invocation == r.Invocation && p.Cell == r.Cell {
					continue
				}
				kept = append(kept, p)
			}
			live = kept
			continue
		}
		if r.Test == "" {
			continue
		}
		live = append(live, r)
	}
	out := map[string]Record{}
	for _, r := range live {
		out[r.Test] = r
	}
	return out
}

// Covered reports whether (leg, test) has a valid PASS for the given source
// hash: the latest non-voided record is a PASS recorded against that very hash,
// on a leg whose file is trusted. Anything else — a different hash, FAIL, SKIP,
// INCONCLUSIVE, an unknown outcome — is uncovered.
func (s *Snapshot) Covered(leg, test, hash string) bool {
	if _, bad := s.Corrupt[leg]; bad {
		return false
	}
	r, ok := s.Latest[leg][test]
	return ok && r.Outcome == OutcomePass && r.Hash != "" && r.Hash == hash
}

// Record returns the latest record for (leg, test).
func (s *Snapshot) Record(leg, test string) (Record, bool) {
	if _, bad := s.Corrupt[leg]; bad {
		return Record{}, false
	}
	r, ok := s.Latest[leg][test]
	return r, ok
}

// Legs lists every leg label with an outcome file (trusted or not), sorted.
func (s *Snapshot) Legs() []string {
	seen := map[string]bool{}
	for l := range s.Latest {
		seen[l] = true
	}
	for l := range s.Corrupt {
		seen[l] = true
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
