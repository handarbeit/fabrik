package bedstate

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func seed(t *testing.T, bed string, files map[string]string) {
	t.Helper()
	dir := DecisionsDir(bed)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClearTrainCapsRemovesOnlyTheTrainCapsFile(t *testing.T) {
	bed := t.TempDir()
	seed(t, bed, map[string]string{
		TrainCapsFile:               `{"version":1,"trials":{"o/r":[1,2,3]}}`,
		UsageLimitFile:              `{"version":1,"until":"2026-10-29T00:00:00Z"}`,
		TrainCapsFile + ".corrupt": "quarantined",
	})
	removed, err := ClearTrainCaps(bed)
	if err != nil || !removed {
		t.Fatalf("ClearTrainCaps = %v, %v; want true, nil", removed, err)
	}
	if _, err := os.Stat(TrainCapsPath(bed)); !os.IsNotExist(err) {
		t.Errorf("train-caps file still present: %v", err)
	}
	for name, want := range map[string]string{
		UsageLimitFile:              `{"version":1,"until":"2026-10-29T00:00:00Z"}`,
		TrainCapsFile + ".corrupt": "quarantined",
	} {
		got, err := os.ReadFile(filepath.Join(DecisionsDir(bed), name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want untouched %q", name, got, err, want)
		}
	}
}

func TestClearTrainCapsMissingIsNotAnError(t *testing.T) {
	bed := t.TempDir()
	if removed, err := ClearTrainCaps(bed); err != nil || removed {
		t.Errorf("no engine-decisions dir: %v, %v; want false, nil", removed, err)
	}
	seed(t, bed, nil)
	if removed, err := ClearTrainCaps(bed); err != nil || removed {
		t.Errorf("no file: %v, %v; want false, nil", removed, err)
	}
}

func TestClearTrainCapsDirectoryInPlaceFailsLoudly(t *testing.T) {
	bed := t.TempDir()
	dir := TrainCapsPath(bed)
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if removed, err := ClearTrainCaps(bed); err == nil || removed {
		t.Fatalf("ClearTrainCaps = %v, %v; want an error", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "child")); err != nil {
		t.Errorf("the directory was deleted: %v", err)
	}
}

// TestFileNamesMatchTheEngine pins the two literals to engine/decision_state.go,
// so a rename there cannot silently turn the clean into a no-op.
func TestFileNamesMatchTheEngine(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "engine", "decision_state.go"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"trainCapsFileName": TrainCapsFile, "usageLimitFileName": UsageLimitFile} {
		re := regexp.MustCompile(name + `\s*=\s*"([^"]+)"`)
		m := re.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in engine/decision_state.go", name)
		}
		if string(m[1]) != want {
			t.Errorf("%s = %q in the engine, %q here", name, m[1], want)
		}
	}
}
