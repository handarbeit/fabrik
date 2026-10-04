package testenv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestIsolateUnsetsAndRestores(t *testing.T) {
	t.Setenv("FABRIK_TOKEN", "secret")
	t.Setenv("GH_TOKEN", "tok")
	t.Setenv("GIT_CONFIG_COUNT", "5")
	t.Setenv("GIT_CONFIG_KEY_4", "a.b")
	t.Setenv("GIT_CONFIG_VALUE_4", "c")

	t.Run("inner", func(t *testing.T) {
		Isolate(t)
		for _, k := range []string{"FABRIK_TOKEN", "GH_TOKEN", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_4", "GIT_CONFIG_VALUE_4", "HOOKDECK_API_KEY"} {
			if v, ok := os.LookupEnv(k); ok {
				t.Errorf("%s still set to %q (must be truly unset, not empty)", k, v)
			}
		}
	})
	if got := os.Getenv("FABRIK_TOKEN"); got != "secret" {
		t.Errorf("FABRIK_TOKEN not restored: %q", got)
	}
	if got := os.Getenv("GIT_CONFIG_KEY_4"); got != "a.b" {
		t.Errorf("GIT_CONFIG_KEY_4 not restored: %q", got)
	}
}

func TestNamesEnumeratesAnyGitConfigIndex(t *testing.T) {
	names := Names([]string{"GIT_CONFIG_KEY_97=x", "GIT_CONFIG_VALUE_97=y", "UNRELATED=1"})
	joined := strings.Join(names, " ")
	for _, want := range []string{"GIT_CONFIG_KEY_97", "GIT_CONFIG_VALUE_97", "FABRIK_REVIEWER_TOKEN", "GH_HOST"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Names missing %s", want)
		}
	}
	if strings.Contains(joined, "UNRELATED") {
		t.Error("Names must not include unrelated variables")
	}
}

func TestIsolateGitIgnoresInheritedCommandScopeConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// What the ADR-1846 helper injection looks like in a worker.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "fabrik.workertest")
	t.Setenv("GIT_CONFIG_VALUE_0", "leaked")
	IsolateGit(t, "[fabrik]\n\tworkertest = from-global\n")
	out, err := exec.Command("git", "config", "--get", "fabrik.workertest").Output()
	if err != nil {
		t.Fatalf("git config: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "from-global" {
		t.Fatalf("got %q; inherited command-scope entry outranked the isolated global file", got)
	}
}
