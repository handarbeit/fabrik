package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var testRetention = transcriptRetention{MaxAge: 168 * time.Hour, MaxBytes: 1 << 30}

var transcriptT0 = time.Date(2026, 10, 15, 0, 54, 17, 123456789, time.UTC)

func transcriptFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSaveTranscript_NameContentAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	data := []byte(`{"type":"result"}` + "\n")
	path, err := saveTranscript(dir, "handarbeit", "fabrik", 2191, "56d9361e", data, transcriptT0, testRetention, "")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(path)
	for _, part := range []string{"handarbeit", "fabrik", "pr2191", "56d9361e", "20261015T005417"} {
		if !strings.Contains(base, part) {
			t.Errorf("name %q lacks %q", base, part)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(data) {
		t.Errorf("content = %q (%v), want raw input unchanged", got, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
		}
	}
	if n := transcriptFiles(t, dir); len(n) != 1 {
		t.Errorf("dir has %v, want only the transcript (no temp leftovers)", n)
	}
}

func TestSaveTranscript_HostileNamesStayInDir(t *testing.T) {
	dir := t.TempDir()
	for _, owner := range []string{"../x", "a/b", "", "..", `a\b`} {
		path, err := saveTranscript(dir, owner, "../../etc", 1, "../sha", []byte("x"), transcriptT0, testRetention, "")
		if err != nil {
			t.Fatalf("owner %q: %v", owner, err)
		}
		if filepath.Dir(path) != dir {
			t.Errorf("owner %q: path %q escaped %q", owner, path, dir)
		}
	}
}

func TestSaveTranscript_SameInstantDoesNotCollide(t *testing.T) {
	dir := t.TempDir()
	a, err := saveTranscript(dir, "o", "r", 1, "sha", []byte("a"), transcriptT0, testRetention, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := saveTranscript(dir, "o", "r", 1, "sha", []byte("b"), transcriptT0, testRetention, "")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("second save overwrote the first")
	}
	if got, _ := os.ReadFile(a); string(got) != "a" {
		t.Errorf("first file clobbered: %q", got)
	}
}

// A filesystem without hard-link support must still keep the transcript, with
// the same exclusive-create guarantee and mode.
func TestSaveTranscript_FallsBackWhenLinkUnsupported(t *testing.T) {
	prev := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.ENOTSUP} }
	t.Cleanup(func() { linkFile = prev })

	dir := t.TempDir()
	a, err := saveTranscript(dir, "o", "r", 1, "sha", []byte("a"), transcriptT0, testRetention, "")
	if err != nil {
		t.Fatalf("save without link support: %v", err)
	}
	b, err := saveTranscript(dir, "o", "r", 1, "sha", []byte("b"), transcriptT0, testRetention, "")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("fallback overwrote the first file")
	}
	if got, _ := os.ReadFile(a); string(got) != "a" {
		t.Errorf("first file clobbered: %q", got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(a); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, err = %v; want 0600", fi.Mode().Perm(), err)
		}
	}
	if n := len(transcriptFiles(t, dir)); n != 2 {
		t.Errorf("dir holds %d files, want 2 (no temp leftovers): %v", n, transcriptFiles(t, dir))
	}
}

// stampName builds a Pruefer-shaped transcript file name for a given time.
func stampName(at time.Time, pr int, ext string) string {
	return transcriptFileName(at, "o", "r", pr, "sha")[:len(transcriptFileName(at, "o", "r", pr, "sha"))-len(".jsonl")] + ext
}

func writeSized(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestPruneTranscripts_ByAgeUsesNameNotMtime(t *testing.T) {
	dir := t.TempDir()
	now := transcriptT0
	oldName := stampName(now.Add(-169*time.Hour), 1, ".jsonl")
	newName := stampName(now.Add(-167*time.Hour), 2, ".jsonl")
	oldDir := stampName(now.Add(-200*time.Hour), 3, ".tool-results")
	newDir := stampName(now.Add(-1*time.Hour), 4, ".tool-results")
	for _, n := range []string{oldName, newName} {
		writeSized(t, filepath.Join(dir, n), 10)
	}
	for _, n := range []string{oldDir, newDir} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
		writeSized(t, filepath.Join(dir, n, "a.txt"), 10)
	}
	// An old name with a fresh mtime is still old; a fresh name with an ancient
	// mtime is still fresh.
	ancient := now.Add(-1000 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, newName), ancient, ancient); err != nil {
		t.Fatal(err)
	}
	pruneTranscripts(dir, now, testRetention, "")
	for n, want := range map[string]bool{oldName: false, newName: true, oldDir: false, newDir: true} {
		if got := exists(filepath.Join(dir, n)); got != want {
			t.Errorf("%s exists = %v, want %v", n, got, want)
		}
	}
}

func TestPruneTranscripts_ByteCeilingOldestFirstProtectsNewest(t *testing.T) {
	dir := t.TempDir()
	now := transcriptT0
	var names []string
	for i := 0; i < 4; i++ {
		n := stampName(now.Add(-time.Duration(4-i)*time.Hour), i, ".jsonl")
		names = append(names, n)
		writeSized(t, filepath.Join(dir, n), 100)
	}
	// The newest unit also carries a 300-byte tool-results dir, so sizes must
	// include directory contents: 100+100+100+(100+300) = 700.
	trDir := filepath.Join(dir, stampName(now.Add(-time.Hour), 3, ".tool-results"))
	if err := os.MkdirAll(trDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(trDir, "a.txt"), 300)

	pruneTranscripts(dir, now, transcriptRetention{MaxAge: 168 * time.Hour, MaxBytes: 450}, "")
	if exists(filepath.Join(dir, names[0])) || exists(filepath.Join(dir, names[1])) || exists(filepath.Join(dir, names[2])) {
		t.Errorf("older units survived the ceiling: %v", transcriptFiles(t, dir))
	}
	if !exists(filepath.Join(dir, names[3])) || !exists(trDir) {
		t.Errorf("newest unit was deleted: %v", transcriptFiles(t, dir))
	}

	// A lone unit larger than the ceiling survives when protected, and goes
	// once it is no longer the one just saved.
	stem := strings.TrimSuffix(names[3], ".jsonl")
	pruneTranscripts(dir, now, transcriptRetention{MaxAge: 168 * time.Hour, MaxBytes: 10}, stem)
	if !exists(filepath.Join(dir, names[3])) {
		t.Error("protected unit was deleted by the byte ceiling")
	}
	pruneTranscripts(dir, now, transcriptRetention{MaxAge: 168 * time.Hour, MaxBytes: 10}, "")
	if exists(filepath.Join(dir, names[3])) || exists(trDir) {
		t.Error("unprotected oversized unit survived")
	}
}

func TestPruneTranscripts_OrphanToolResultsDirAgesOut(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, stampName(transcriptT0.Add(-300*time.Hour), 9, ".tool-results"))
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(orphan, "a.txt"), 5)
	pruneTranscripts(dir, transcriptT0, testRetention, "")
	if exists(orphan) {
		t.Error("orphan .tool-results directory did not age out")
	}
}

func TestPruneTranscripts_ForeignEntriesSurvive(t *testing.T) {
	dir := t.TempDir()
	now := transcriptT0
	foreign := []string{
		"notes.txt", ".transcript-123.tmp",
		"20200101T000000.000000000Z_o_r_pr1_sha.txt", // old stamp, wrong extension
		"20200101T000000.000000000Z.tool-results",    // old stamp, not Pruefer's shape
		"something.tool-results",
	}
	for _, n := range foreign {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o700); err != nil && !strings.HasSuffix(n, ".txt") && !strings.HasSuffix(n, ".tmp") {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"notes.txt", ".transcript-123.tmp", "20200101T000000.000000000Z_o_r_pr1_sha.txt"} {
		_ = os.RemoveAll(filepath.Join(dir, n)) // replace the directory made above with a file
		writeSized(t, filepath.Join(dir, n), 1_000_000)
	}
	// A symlinked directory with a Pruefer-shaped, ancient name.
	target := t.TempDir()
	link := filepath.Join(dir, stampName(now.Add(-999*time.Hour), 5, ".tool-results"))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeSized(t, filepath.Join(target, "keep.txt"), 5)

	pruneTranscripts(dir, now, transcriptRetention{MaxAge: time.Hour, MaxBytes: 1}, "")
	for _, n := range foreign {
		if !exists(filepath.Join(dir, n)) {
			t.Errorf("foreign entry %s was deleted", n)
		}
	}
	if !exists(link) || !exists(filepath.Join(target, "keep.txt")) {
		t.Error("symlinked tool-results directory was followed or deleted")
	}
}

func TestSaveTranscript_PrunesAfterSaveAndRelocatesToolResults(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, stampName(transcriptT0.Add(-200*time.Hour), 1, ".jsonl"))
	writeSized(t, stale, 5)

	stage := t.TempDir()
	writeSized(t, filepath.Join(stage, "toolu_1.txt"), 7)
	writeSized(t, filepath.Join(stage, "toolu_2.txt"), 9)

	p, err := saveTranscript(dir, "o", "r", 2, "sha", []byte("x"), transcriptT0, testRetention, stage)
	if err != nil {
		t.Fatal(err)
	}
	if exists(stale) {
		t.Error("a transcript older than the age bound survived the save")
	}
	trDir := strings.TrimSuffix(p, ".jsonl") + ".tool-results"
	for n, size := range map[string]int64{"toolu_1.txt": 7, "toolu_2.txt": 9} {
		fi, err := os.Stat(filepath.Join(trDir, n))
		if err != nil || fi.Size() != size {
			t.Errorf("%s not relocated: %v", n, err)
			continue
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", n, fi.Mode().Perm())
		}
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(trDir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("tool-results dir mode = %v, %v; want 0700", fi, err)
		}
	}
	if left := transcriptFiles(t, stage); len(left) != 0 {
		t.Errorf("stage dir still holds %v", left)
	}
}

func TestSaveTranscript_UnwritableDirErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveTranscript(filepath.Join(blocker, "sub"), "o", "r", 1, "sha", []byte("x"), transcriptT0, testRetention, ""); err == nil {
		t.Fatal("expected an error when the directory cannot be created")
	}
}

func TestSaveTranscript_ConcurrentWritersAreSerialised(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stage := t.TempDir()
			writeSized(t, filepath.Join(stage, "a.txt"), 1)
			if _, err := saveTranscript(dir, "o", "r", i, "sha", []byte("x"), transcriptT0.Add(time.Duration(i)*time.Millisecond), testRetention, stage); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(transcriptFiles(t, dir)); got != 40 {
		t.Errorf("dir has %d entries, want 20 transcripts + 20 tool-results dirs", got)
	}
}

func TestPruneTranscriptsAtStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "t")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldP := filepath.Join(dir, stampName(transcriptT0.Add(-169*time.Hour), 1, ".jsonl"))
	newP := filepath.Join(dir, stampName(transcriptT0.Add(-time.Hour), 2, ".jsonl"))
	writeSized(t, oldP, 1)
	writeSized(t, newP, 1)
	pruneTranscriptsAtStartup(Config{TranscriptDir: dir}, transcriptT0) // zero bounds: defaults
	if exists(oldP) || !exists(newP) {
		t.Errorf("startup prune: old exists=%v new exists=%v", exists(oldP), exists(newP))
	}
	pruneTranscriptsAtStartup(Config{TranscriptDir: filepath.Join(dir, "missing")}, transcriptT0) // no panic
}

func TestMaybeKeepTranscript_Policy(t *testing.T) {
	partial := Coverage{Complete: false}
	complete := Coverage{Complete: true}
	cases := []struct {
		mode      string
		cov       Coverage
		data      string
		want      int
		wantMoved bool
	}{
		{"", partial, "x", 1, true},
		{"", complete, "x", 1, true},
		{KeepTranscriptsAll, complete, "x", 1, true},
		{KeepTranscriptsPartial, partial, "x", 1, true},
		{KeepTranscriptsPartial, complete, "x", 0, false},
		{KeepTranscriptsOff, partial, "x", 0, false},
		{KeepTranscriptsAll, partial, "", 0, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%t/%q", c.mode, c.cov.Complete, c.data), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "t")
			stage := t.TempDir()
			writeSized(t, filepath.Join(stage, "a.txt"), 3)
			path := maybeKeepTranscript(Config{KeepTranscripts: c.mode, TranscriptDir: dir}, 1, "o", "r", "sha", c.cov, []byte(c.data), stage)
			n := len(transcriptFiles(t, dir))
			if (path != "") != (c.want == 1) {
				t.Errorf("path = %q, want %d file(s)", path, c.want)
			}
			moved := path != "" && exists(filepath.Join(strings.TrimSuffix(path, ".jsonl")+".tool-results", "a.txt"))
			if moved != c.wantMoved {
				t.Errorf("tool results moved = %v, want %v", moved, c.wantMoved)
			}
			if c.want == 0 && n != 0 {
				t.Errorf("dir holds %d entries, want none", n)
			}
			if !c.wantMoved && !exists(filepath.Join(stage, "a.txt")) {
				t.Error("staged file was touched although no transcript was kept")
			}
		})
	}
}

// A tool-results relocation failure is logged and the transcript is still kept.
func TestMaybeKeepTranscript_RelocationFailureStillKeepsTranscript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "t")
	path := maybeKeepTranscript(Config{KeepTranscripts: KeepTranscriptsAll, TranscriptDir: dir}, 1, "o", "r", "sha", Coverage{}, []byte("x"), filepath.Join(t.TempDir(), "not-there", "x"))
	if path == "" || !exists(path) {
		t.Fatalf("transcript not kept after a relocation failure: %q", path)
	}
}
