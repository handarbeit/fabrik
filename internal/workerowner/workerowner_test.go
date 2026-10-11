package workerowner

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker-owners")
	p, err := NewPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := NewPath(dir)
	if p == p2 {
		t.Fatal("NewPath returned a duplicate")
	}
	if err := Write(p, Info{PID: 4242, StartToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	got, err := Read(p)
	if err != nil || got.PID != 4242 || got.StartToken != "tok" {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestReadRejectsGarbageAndUnsafePIDs(t *testing.T) {
	d := t.TempDir()
	for name, body := range map[string]string{"junk": "{", "pid1": `{"pid":1}`, "pid0": `{"pid":0}`} {
		p := filepath.Join(d, name)
		_ = os.WriteFile(p, []byte(body), 0600)
		if _, err := Read(p); err == nil {
			t.Errorf("%s: Read accepted it", name)
		}
	}
	if _, err := Read(filepath.Join(d, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	self := os.Getpid()
	tok := sessionreap.StartToken(self)
	if tok == "" {
		t.Skip("no start token on this platform")
	}
	if got := Check(Info{PID: self, StartToken: tok}, sessionreap.Options{}); got != Alive {
		t.Errorf("live owner = %v, want alive", got)
	}
	// Recycled PID: alive, different token -> the recorded owner is gone.
	if got := Check(Info{PID: self, StartToken: tok + "x"}, sessionreap.Options{}); got != Gone {
		t.Errorf("recycled owner = %v, want gone", got)
	}
	// Unreadable token on a live PID, or no recorded token: never Gone.
	if got := Check(Info{PID: self, StartToken: tok}, sessionreap.Options{Start: func(int) string { return "" }}); got != Unknown {
		t.Errorf("unreadable token = %v, want unknown", got)
	}
	if got := Check(Info{PID: self}, sessionreap.Options{}); got != Unknown {
		t.Errorf("no recorded token = %v, want unknown", got)
	}
	if got := Check(Info{PID: 0}, sessionreap.Options{}); got != Unknown {
		t.Errorf("pid 0 = %v, want unknown", got)
	}
	// Absent PID: a reaped child.
	if got := Check(Info{PID: deadPID(t), StartToken: tok}, sessionreap.Options{}); got != Gone {
		t.Errorf("dead owner = %v, want gone", got)
	}
}
