package engine

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
	"github.com/handarbeit/fabrik/internal/workerowner"
)

// Worker owner marker (#2128, ADR-2128).
//
// runClaude sets FABRIK_WORKER_OWNER=<owner file path> in the worker's
// environment and writes {pid, start token} to that file right after
// cmd.Start(). The variable is inherited by every descendant, so (a) a test
// binary can tell when its worker is provably dead and exit, and (b) the engine
// can find an orphan by what it carries rather than by its place in the process
// tree — a backgrounded command that is still in use looks identical to an
// orphan by ppid/pgid, but carries a live worker's marker.

// workerOwnerStaleAge is how old an unreferenced owner file must be before the
// janitor removes it. The file is written within milliseconds of the worker
// starting and its record is appended right after, so anything older with no
// record is debris from a crash between the two.
const workerOwnerStaleAge = 24 * time.Hour

// workerOwnersDir is <state dir>/worker-owners, beside workers.json (so the
// workerRecordsPathOverride test seam isolates it too).
func workerOwnersDir() string {
	return filepath.Join(filepath.Dir(workerRecordsPath()), "worker-owners")
}

// newWorkerOwnerPath returns a fresh owner-file path, or "" (logged by the
// caller) when one cannot be allocated — the worker then runs unmarked, exactly
// as before this feature.
func newWorkerOwnerPath() string {
	p, err := workerowner.NewPath(workerOwnersDir())
	if err != nil {
		return ""
	}
	return p
}

// writeWorkerOwnerFile records the worker's identity in its owner file. The
// start token is the one sessionreap records everywhere else, so it is
// comparable by the reader. An empty token is still written: the reader treats
// it as Unknown and never exits on it.
func writeWorkerOwnerFile(path string, pid int) error {
	return workerowner.Write(path, workerowner.Info{PID: pid, StartToken: sessionreap.StartToken(pid)})
}

// inWorkerOwnersDir reports whether path is a direct child of the owner-file
// directory, so removal can never be steered elsewhere by a corrupt record.
func inWorkerOwnersDir(path string) bool {
	return path != "" && filepath.Dir(filepath.Clean(path)) == filepath.Clean(workerOwnersDir())
}

// removeWorkerOwnerFile deletes an owner file the engine created.
func removeWorkerOwnerFile(path string) {
	if inWorkerOwnersDir(path) {
		_ = os.Remove(path)
	}
}

// sweepStaleWorkerOwnerFiles removes owner files that no worker record
// references and that are older than workerOwnerStaleAge. Returns the count.
func sweepStaleWorkerOwnerFiles(recs []workerRecord, now time.Time) int {
	dir := workerOwnersDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	ref := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.Owner != "" {
			ref[filepath.Clean(r.Owner)] = true
		}
	}
	removed := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if ref[p] {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < workerOwnerStaleAge {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed
}
