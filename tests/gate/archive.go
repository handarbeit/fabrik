package gate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-leg log archive (R7). Nothing a leg produces is overwritten by the next
// one. Under <sha>/archive/<cell>/<invocation>/ every leg keeps:
//
//	go-test.<phase>.json  one `go test -json` stream per phase of the two-phase leg (#1977): go-test.shared.json,
//	                    go-test.default-base-train.json, go-test.exclusive.json (written there directly); an
//	                    INCONCLUSIVE retry adds go-test.retry-<n>.<phase>.json. Only the no-registry
//	                    fallback, one undivided `go test`, writes a bare go-test.json / go-test.retry-<n>.json
//	fabrik.log.<n>      the bed ENGINE log, one file per engine run (see below)
//	bed-run.log         the bed's stdout/stderr (append-only across harness restarts)
//	preflight.txt       the invocation's preflight output
//	load.json           the host's 1-minute load average at leg start and end, plus the peak seen while the leg ran (#1977)
//	phases.json         per-phase wall-clock, peak concurrent tests and GraphQL spend, with the host's peak load (#1977)
//	bed-concurrency.json the bed engine's effective max_concurrent (#1977)
//	probes.json         the preflight environment probes' results (#1974): host load + orphans, board-listing lag
//	bed-config.sha256   a hash of the bed's .fabrik/stages/ and config.yaml
//
// The log that used to vanish is NOT bed-run.log: the engine opens
// .fabrik/fabrik.log on EVERY start: the previous run is rotated to
// fabrik.log.1 (and older ones shift up, at most five are kept) and a fresh
// fabrik.log holds only the new run, opening with a start banner (#2094). So each
// restart — the mode switch that begins every leg, and a scenario that restarts
// the engine mid-leg — leaves the live file holding only the latest run. logArchiver therefore samples the file while the leg
// runs and starts a new segment whenever it sees a restart, instead of copying
// once at the end (which would keep only the engine's last run).

// logArchiver copies the engine log's growth into numbered segments.
type logArchiver struct {
	src, dir string
	interval time.Duration

	mu     sync.Mutex
	seg    int
	offset int64
	head   []byte // the first bytes of the current engine run, to recognise a restart
	err    error

	stop chan struct{}
	done chan struct{}
}

func newLogArchiver(src, dir string, interval time.Duration) *logArchiver {
	a := &logArchiver{src: src, dir: dir, interval: interval, stop: make(chan struct{}), done: make(chan struct{})}
	// Start AT the current end: what is already there belongs to the previous
	// leg, whose own final sample archived it.
	if st, err := os.Stat(src); err == nil {
		a.offset = st.Size()
		a.head = a.readAt(0, 256)
	}
	return a
}

func (a *logArchiver) readAt(off int64, n int) []byte {
	f, err := os.Open(a.src)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	m, _ := f.ReadAt(buf, off)
	return buf[:m]
}

// restarted: the file is smaller than what was already copied, or its opening
// bytes are no longer the ones recorded (truncated and regrown between samples).
func (a *logArchiver) restarted(size int64) bool {
	if size < a.offset {
		return true
	}
	if len(a.head) == 0 {
		return false
	}
	n := len(a.head)
	if int64(n) > size {
		return true
	}
	return !bytes.Equal(a.readAt(0, n), a.head)
}

// Sample copies whatever the engine log gained since the last sample.
func (a *logArchiver) Sample() {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := os.Stat(a.src)
	if err != nil {
		return // no engine log (yet): nothing to archive, not an error
	}
	size := st.Size()
	if a.restarted(size) {
		a.recoverTail()
		a.seg++
		a.offset = 0
		a.head = nil
	}
	if size > a.offset {
		f, err := os.Open(a.src)
		if err != nil {
			a.err = err
			return
		}
		defer f.Close()
		if _, err := f.Seek(a.offset, io.SeekStart); err != nil {
			a.err = err
			return
		}
		out, err := os.OpenFile(a.segPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			a.err = err
			return
		}
		n, cerr := io.CopyN(out, f, size-a.offset)
		if err := out.Close(); err != nil && cerr == nil {
			cerr = err
		}
		a.offset += n
		if cerr != nil && cerr != io.EOF {
			a.err = cerr
		}
	}
	if len(a.head) == 0 && a.offset > 0 {
		a.head = a.readAt(0, 256)
	}
}

// recoverTail copies what the run that just ended wrote after the last sample
// into its segment. The engine rotates that run to <src>.1 on start (#2094), so
// the tail still exists there — but only when .1 is provably the same run being
// copied (its opening bytes equal the recorded head) and it is longer than what
// was copied. Any other state, or any error, copies nothing: the archiver then
// behaves exactly as it did before rotation existed.
func (a *logArchiver) recoverTail() {
	if len(a.head) == 0 {
		return
	}
	f, err := os.Open(a.src + ".1")
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= a.offset {
		return
	}
	head := make([]byte, len(a.head))
	if _, err := f.ReadAt(head, 0); err != nil || !bytes.Equal(head, a.head) {
		return
	}
	// Read the whole tail before writing so a read error cannot leave a partial copy.
	tail := make([]byte, st.Size()-a.offset)
	if _, err := f.ReadAt(tail, a.offset); err != nil {
		return
	}
	out, err := os.OpenFile(a.segPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, werr := out.Write(tail)
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	_ = werr // best effort: a failed write loses only the tail, as before
}

func (a *logArchiver) segPath() string {
	return filepath.Join(a.dir, fmt.Sprintf("fabrik.log.%d", a.seg))
}

// Start samples on the interval until Stop.
func (a *logArchiver) Start() {
	go func() {
		defer close(a.done)
		if a.interval <= 0 {
			<-a.stop
			return
		}
		t := time.NewTicker(a.interval)
		defer t.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-t.C:
				a.Sample()
			}
		}
	}()
}

// Stop ends the sampler and takes one last sample — on every exit path, a
// cancelled or killed leg included.
func (a *logArchiver) Stop() error {
	close(a.stop)
	<-a.done
	a.Sample()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// legArchive is one leg's archive directory plus what is live while it runs.
type legArchive struct {
	g        *Gate
	dir      string
	arch     *logArchiver
	loadFrom float64
	loadOK   bool
	finished bool

	// The load sampler (#1977, R5.1): the highest 1-minute load average seen while
	// the leg ran, sampled on the archive's cadence (start and end included).
	loadMu      sync.Mutex
	loadPeak    float64
	loadSamples int
	loadStop    chan struct{}
	loadDone    chan struct{}
}

// sampleLoad folds one reading of the host's load average into the peak.
func (a *legArchive) sampleLoad() {
	v, ok := a.g.loadAvg()
	if !ok {
		return
	}
	a.loadMu.Lock()
	defer a.loadMu.Unlock()
	if a.loadSamples == 0 || v > a.loadPeak {
		a.loadPeak = v
	}
	a.loadSamples++
}

// startLoadSampler samples the load average until stopLoadSampler. With a
// non-positive interval only the start and end readings are taken.
func (a *legArchive) startLoadSampler(interval time.Duration) {
	a.loadStop, a.loadDone = make(chan struct{}), make(chan struct{})
	a.sampleLoad()
	go func() {
		defer close(a.loadDone)
		if interval <= 0 {
			<-a.loadStop
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-a.loadStop:
				return
			case <-t.C:
				a.sampleLoad()
			}
		}
	}()
}

func (a *legArchive) stopLoadSampler() {
	if a.loadStop == nil {
		return
	}
	close(a.loadStop)
	<-a.loadDone
	a.loadStop = nil
	a.sampleLoad()
}

// LoadStats is the peak 1-minute load and the number of readings behind it;
// ok is false when the host reported none.
func (a *legArchive) LoadStats() (peak float64, samples int, ok bool) {
	a.loadMu.Lock()
	defer a.loadMu.Unlock()
	return a.loadPeak, a.loadSamples, a.loadSamples > 0
}

// ArchiveCellDir is <ledger>/archive/<cell>/<invocation>.
func (l *Ledger) ArchiveCellDir(cell, invocation string) string {
	return filepath.Join(l.ArchiveDir(), cell, invocation)
}

// beginArchive creates the leg's archive directory, writes the invocation-wide
// files, starts the engine-log sampler and takes the start load average. Failing
// to archive never fails a leg: the problem is warned about and the leg runs.
func (g *Gate) beginArchive(cell Cell, cs *covState) *legArchive {
	dir := cs.ledger.ArchiveCellDir(cellDirName(cell), cs.invocation)
	a := &legArchive{g: g, dir: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		g.errf("warning: cannot create the leg archive %s: %v — this leg's logs are not archived\n", dir, err)
		return a
	}
	if cs.preflight != "" {
		if err := os.WriteFile(filepath.Join(dir, "preflight.txt"), []byte(cs.preflight), 0o644); err != nil {
			g.errf("warning: archiving preflight output: %v\n", err)
		}
	}
	// #1976 (R4): which bed produced this leg.
	bedRec := struct {
		Name string `json:"name,omitempty"`
		Dir  string `json:"dir"`
	}{Dir: g.Cfg.TestBed}
	if g.bed != nil {
		bedRec.Name = g.bed.Name
	}
	if err := os.WriteFile(filepath.Join(dir, "bed.json"), mustJSON(bedRec), 0o644); err != nil {
		g.errf("warning: archiving the bed attribution: %v\n", err)
	}
	if h, err := BedConfigHash(g.Cfg.TestBed); err != nil {
		g.errf("warning: hashing the bed configuration: %v\n", err)
	} else {
		if err := os.WriteFile(filepath.Join(dir, "bed-config.sha256"), []byte(h+"\n"), 0o644); err != nil {
			g.errf("warning: archiving the bed config hash: %v\n", err)
		}
		if prev := cs.ledger.NoteBedConfig(cs.invocation, g.Cfg.TestBed, h); len(prev) > 0 {
			g.errf("warning: the bed configuration (.fabrik/stages/, config.yaml) differs from an earlier invocation of this ledger (%s) — results from different configurations are being combined\n", shortSHA(prev[0]))
		}
	}
	g.writeProbes(dir)
	a.loadFrom, a.loadOK = g.loadAvg()
	a.startLoadSampler(g.Cfg.ArchiveLogInterval)
	a.arch = newLogArchiver(g.Cfg.EngineLog, dir, g.Cfg.ArchiveLogInterval)
	a.arch.Start()
	return a
}

func (g *Gate) loadAvg() (float64, bool) {
	if g.LoadAvg != nil {
		return g.LoadAvg()
	}
	return probeLoadAvg()
}

// Finish stops the sampler (taking the final sample), copies bed-run.log and
// writes load.json. Idempotent, and safe to defer.
func (a *legArchive) Finish() {
	if a == nil || a.finished {
		return
	}
	a.finished = true
	a.stopLoadSampler()
	if a.arch != nil {
		if err := a.arch.Stop(); err != nil {
			a.g.errf("warning: archiving the engine log: %v\n", err)
		}
	}
	if a.dir == "" {
		return
	}
	if _, err := os.Stat(a.dir); err != nil {
		return
	}
	if err := copyFile(filepath.Join(a.g.Cfg.TestBed, "bed-run.log"), filepath.Join(a.dir, "bed-run.log")); err != nil && !os.IsNotExist(err) {
		a.g.errf("warning: archiving bed-run.log: %v\n", err)
	}
	type load struct {
		Start   *float64 `json:"start_1m"`
		End     *float64 `json:"end_1m"`
		Peak    *float64 `json:"peak_1m,omitempty"`
		Samples int      `json:"samples,omitempty"`
	}
	var l load
	if a.loadOK {
		v := a.loadFrom
		l.Start = &v
	}
	if v, ok := a.g.loadAvg(); ok {
		l.End = &v
	}
	if peak, n, ok := a.LoadStats(); ok {
		l.Peak, l.Samples = &peak, n
	}
	if err := os.WriteFile(filepath.Join(a.dir, "load.json"), mustJSON(l), 0o644); err != nil {
		a.g.errf("warning: archiving load average: %v\n", err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// BedConfigHash hashes the bed's stage configs and config.yaml — never .env,
// which holds secrets. A missing file contributes nothing (a bed with no stages
// directory hashes as empty), so the hash differs only when content does.
func BedConfigHash(bed string) (string, error) {
	h := sha256.New()
	files, err := filepath.Glob(filepath.Join(bed, ".fabrik", "stages", "*"))
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	files = append(files, filepath.Join(bed, ".fabrik", "config.yaml"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.Base(f), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NoteBedConfig records this invocation's hash of bed's configuration in the
// ledger and returns the DIFFERENT hashes other invocations recorded for the SAME
// bed (empty when all agree). Entries are keyed "<invocation>@<bed>" (#1976): two
// beds legitimately differ (their own boards and repos in config.yaml), so only a
// bed's change against itself is drift. A legacy entry keyed by invocation alone
// names no bed and is ignored.
func (l *Ledger) NoteBedConfig(invocation, bed, hash string) (differing []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.Dir(), "bedconfig.json")
	m := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m) // a corrupt file is advisory data: start over
	}
	key := invocation + "@" + bed
	seen := map[string]bool{}
	for k, h := range m {
		inv, b, ok := strings.Cut(k, "@")
		if !ok || b != bed || inv == invocation || h == hash || seen[h] {
			continue
		}
		seen[h] = true
		differing = append(differing, h)
	}
	sort.Strings(differing)
	m[key] = hash
	_ = writeFileAtomic(path, mustJSON(m)) // advisory; a failed write must not fail a leg
	return differing
}

// PruneArchives applies the retention rule: the bulky archive/ subtrees of all
// but the newest keep SHAs (by meta.last_used) are removed; the SHA in use is
// never pruned. Outcome records, meta.json, invocations.jsonl, bedconfig.json
// and pregate/ are never touched, so coverage survives pruning.
func PruneArchives(root string, keep int, current string) (removed []string) {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type shaDir struct {
		name string
		used string
	}
	var shas []shaDir
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "pregate" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, e.Name(), "meta.json"))
		if err != nil {
			continue // not a ledger directory
		}
		var m ledgerMeta
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		shas = append(shas, shaDir{e.Name(), m.LastUsed})
	}
	sort.Slice(shas, func(i, j int) bool {
		if shas[i].used != shas[j].used {
			return shas[i].used > shas[j].used
		}
		return shas[i].name > shas[j].name
	})
	for i, s := range shas {
		if i < keep || s.name == current {
			continue
		}
		arch := filepath.Join(root, s.name, "archive")
		if _, err := os.Stat(arch); err != nil {
			continue
		}
		if os.RemoveAll(arch) == nil {
			removed = append(removed, s.name)
		}
	}
	return removed
}
