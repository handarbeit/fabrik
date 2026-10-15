package pruefer

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	path, err := saveTranscript(dir, "handarbeit", "fabrik", 2191, "56d9361e", data, transcriptT0, 50)
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
		path, err := saveTranscript(dir, owner, "../../etc", 1, "../sha", []byte("x"), transcriptT0, 50)
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
	a, err := saveTranscript(dir, "o", "r", 1, "sha", []byte("a"), transcriptT0, 50)
	if err != nil {
		t.Fatal(err)
	}
	b, err := saveTranscript(dir, "o", "r", 1, "sha", []byte("b"), transcriptT0, 50)
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

func TestSaveTranscript_CapDeletesOldest(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 4; i++ {
		p, err := saveTranscript(dir, "o", "r", i, "sha", []byte("x"), transcriptT0.Add(time.Duration(i)*time.Second), 3)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Errorf("oldest transcript still present: %v", err)
	}
	for _, p := range paths[1:] {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("newer transcript %s missing: %v", p, err)
		}
	}
}

func TestSaveTranscript_PruneIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	unrelated := []string{"notes.txt", ".transcript-123.tmp", "20200101T000000.000000000Z_o_r_pr1_sha.txt"}
	for _, n := range unrelated {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := saveTranscript(dir, "o", "r", i, "sha", []byte("x"), transcriptT0.Add(time.Duration(i)*time.Second), 1); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range unrelated {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("prune touched unrelated file %s: %v", n, err)
		}
	}
	if got := len(transcriptFiles(t, dir)); got != len(unrelated)+1 {
		t.Errorf("dir has %d files, want %d", got, len(unrelated)+1)
	}
}

func TestSaveTranscript_UnwritableDirErrors(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveTranscript(filepath.Join(blocker, "sub"), "o", "r", 1, "sha", []byte("x"), transcriptT0, 50); err == nil {
		t.Fatal("expected an error when the directory cannot be created")
	}
}

func TestSaveTranscript_ConcurrentWritersRespectCap(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := saveTranscript(dir, "o", "r", i, "sha", []byte("x"), transcriptT0.Add(time.Duration(i)*time.Millisecond), 5); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(transcriptFiles(t, dir)); got != 5 {
		t.Errorf("dir has %d files, want 5", got)
	}
}

func TestMaybeKeepTranscript_Policy(t *testing.T) {
	partial := Coverage{Complete: false}
	complete := Coverage{Complete: true}
	cases := []struct {
		mode string
		cov  Coverage
		data string
		want int
	}{
		{"", partial, "x", 1},
		{KeepTranscriptsPartial, partial, "x", 1},
		{KeepTranscriptsPartial, complete, "x", 0},
		{KeepTranscriptsAll, complete, "x", 1},
		{KeepTranscriptsOff, partial, "x", 0},
		{KeepTranscriptsPartial, partial, "", 0},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%t/%q", c.mode, c.cov.Complete, c.data), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "t")
			path := maybeKeepTranscript(Config{KeepTranscripts: c.mode, TranscriptDir: dir}, 1, "o", "r", "sha", c.cov, []byte(c.data))
			n := len(transcriptFiles(t, dir))
			if n != c.want || (path != "") != (c.want == 1) {
				t.Errorf("files = %d, path = %q, want %d file(s)", n, path, c.want)
			}
		})
	}
}
