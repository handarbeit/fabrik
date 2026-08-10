package simgh

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRepoBareDir_SeededRepo_ReturnsRealBareGitRepo confirms RepoBareDir
// returns a path that is genuinely usable as a git remote — the harness's
// local-clone wiring (#1449) runs `git clone --bare` against it, so a
// plausible-looking-but-wrong path would only surface as a mysterious clone
// failure deep inside a scenario.
func TestRepoBareDir_SeededRepo_ReturnsRealBareGitRepo(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets")
	if err := s.Err(); err != nil {
		t.Fatalf("SeedRepo: %v", err)
	}

	dir, err := s.RepoBareDir("acme/widgets")
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("RepoBareDir returned %q, which does not exist: %v", dir, statErr)
	}

	cmd := exec.Command("git", "rev-parse", "--is-bare-repository")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --is-bare-repository in %q: %v: %s", dir, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Errorf("RepoBareDir(%q) = %q is not a bare repository (rev-parse said %q)", "acme/widgets", dir, got)
	}
}

// TestRepoBareDir_UnseededRepo_Errors confirms the not-found path is a real
// error rather than a path into thin air a caller might accidentally clone
// against (which would produce a confusing "repository does not exist" from
// git rather than a clear simgh-level error).
func TestRepoBareDir_UnseededRepo_Errors(t *testing.T) {
	s, _ := newSim(t)
	if _, err := s.RepoBareDir("acme/never-seeded"); err == nil {
		t.Error("expected an error for an unseeded repo")
	}
}
