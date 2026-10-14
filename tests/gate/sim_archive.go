package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The sim pre-gate's archive (#2247 R6): the raw `go test -json` stream, a
// rendered timeline and the goroutine dump, kept per pre-gate run beside the
// pre-gate record as .e2e-coverage/pregate/<head>.sim[.aN].{jsonl,log,dump.txt,
// timings.json}. A TSan-crash retry is a second attempt and is suffixed .a2 so it
// does not overwrite the first.
//
// With the ledger disabled (E2E_COVERAGE_DIR=""), the files go to a fresh temp
// directory instead — the evidence is still kept, and its path still printed.

const simArchiveMarker = ".sim." // <head>.sim.<ext>: what prunePregateSimArchive owns

type simArchive struct {
	dir    string
	prefix string // path without extension
	raw    *os.File
	log    *os.File
}

func (a *simArchive) path(ext string) string { return a.prefix + "." + ext }

func (a *simArchive) close() {
	if a == nil {
		return
	}
	for _, f := range []*os.File{a.raw, a.log} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// pattern is what the terminal prints as the archive's location.
func (a *simArchive) pattern() string { return a.prefix + ".*" }

// openSimArchive creates attempt n's files. It returns nil — never an error that
// could fail the pre-gate — when nothing can be written.
func (g *Gate) openSimArchive(ctx context.Context, attempt int) *simArchive {
	head := g.repoHead(ctx)
	if head == "" {
		head = "nohead"
	}
	var dir string
	if g.Cfg.CoverageDir != "" {
		dir = filepath.Join(g.Cfg.CoverageDir, "pregate")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			g.errf("warning: sim archive: %v\n", err)
			return nil
		}
		g.prunePregateSimArchive(dir, head)
	} else {
		d, err := os.MkdirTemp(g.Cfg.TmpDir, "fabrik-sim-")
		if err != nil {
			g.errf("warning: sim archive: %v\n", err)
			return nil
		}
		dir = d
	}
	name := head + ".sim"
	if attempt > 1 {
		name += fmt.Sprintf(".a%d", attempt)
	}
	a := &simArchive{dir: dir, prefix: filepath.Join(dir, name)}
	var err error
	if a.raw, err = os.Create(a.path("jsonl")); err != nil {
		g.errf("warning: sim archive: %v\n", err)
		return nil
	}
	if a.log, err = os.Create(a.path("log")); err != nil {
		a.close()
		g.errf("warning: sim archive: %v\n", err)
		return nil
	}
	return a
}

// prunePregateSimArchive keeps the bulky <head>.sim.* files of the newest
// CoverageKeepSHAs heads (by newest file mtime, the current head always among
// them) and removes the rest. <head>.json, <head>.retries.jsonl and the rolling
// sim-timings.json are the pre-gate's coverage record and are never touched.
func (g *Gate) prunePregateSimArchive(dir, current string) {
	keep := g.Cfg.CoverageKeepSHAs
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type headFiles struct {
		newest int64
		files  []string
	}
	byHead := map[string]*headFiles{}
	for _, e := range entries {
		i := strings.Index(e.Name(), simArchiveMarker)
		if i <= 0 || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		h := e.Name()[:i]
		hf := byHead[h]
		if hf == nil {
			hf = &headFiles{}
			byHead[h] = hf
		}
		hf.files = append(hf.files, filepath.Join(dir, e.Name()))
		if m := info.ModTime().UnixNano(); m > hf.newest {
			hf.newest = m
		}
	}
	heads := make([]string, 0, len(byHead))
	for h := range byHead {
		heads = append(heads, h)
	}
	sort.Slice(heads, func(i, j int) bool { return byHead[heads[i]].newest > byHead[heads[j]].newest })
	kept := 0
	for _, h := range heads {
		if h == current || kept < keep {
			if h != current {
				kept++
			}
			continue
		}
		for _, f := range byHead[h].files {
			_ = os.Remove(f)
		}
	}
}

// ---- the rolling timing baseline (R2's "last recorded duration") ----

type simBaselineFile struct {
	V     int                `json:"v"`
	Tests map[string]float64 `json:"tests"` // top-level test → seconds in the last passing run that ran it
}

func (g *Gate) simBaselinePath() string {
	if g.Cfg.CoverageDir == "" {
		return ""
	}
	return filepath.Join(g.Cfg.CoverageDir, "pregate", "sim-timings.json")
}

// readSimBaseline returns the last passing durations; an unreadable file is an
// empty baseline (every test then uses the no-history threshold).
func (g *Gate) readSimBaseline() map[string]float64 {
	p := g.simBaselinePath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var f simBaselineFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.Tests
}

// recordSimBaseline folds a PASSING run's durations in. A failed or killed run
// never calls it, so a hang's duration cannot become the next threshold.
func (g *Gate) recordSimBaseline(old map[string]float64, ts []Timing) {
	p := g.simBaselinePath()
	if p == "" || len(ts) == 0 {
		return
	}
	merged := map[string]float64{}
	for k, v := range old {
		merged[k] = v
	}
	for _, t := range ts {
		if t.Result == "pass" {
			merged[t.Test] = t.Elapsed
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		g.errf("warning: sim timing baseline: %v\n", err)
		return
	}
	if err := writeFileAtomic(p, mustJSON(simBaselineFile{V: 1, Tests: merged})); err != nil {
		g.errf("warning: sim timing baseline: %v\n", err)
	}
}
