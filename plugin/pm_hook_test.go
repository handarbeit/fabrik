package plugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runVersionCheck runs the PM plugin's SessionStart hook with a stub `fabrik`
// binary (or none, when stubBody is empty) as the only thing on PATH besides
// the system tools the script needs.
func runVersionCheck(t *testing.T, stubBody string, env ...string) (stdout string, exitCode int) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	script, err := filepath.Abs("fabrik/hooks/version-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs("fabrik")

	bin := t.TempDir()
	if stubBody != "" {
		stub := "#!/bin/sh\n" + stubBody + "\n"
		if err := os.WriteFile(filepath.Join(bin, "fabrik"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only /usr/bin:/bin besides the stub dir, so a developer's real `fabrik`
	// never leaks in.
	cmd := exec.Command("sh", script)
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "CLAUDE_PLUGIN_ROOT=" + root}, env...)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func pluginManifestVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("fabrik/.claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Version
}

func TestVersionCheckHook(t *testing.T) {
	pv := pluginManifestVersion(t)
	stub := func(v string) string { return `echo "` + v + `"` }

	t.Run("match is silent", func(t *testing.T) {
		out, code := runVersionCheck(t, stub(pv))
		if out != "" || code != 0 {
			t.Errorf("out=%q code=%d", out, code)
		}
	})
	t.Run("match with a leading v (go install build) is silent", func(t *testing.T) {
		out, code := runVersionCheck(t, stub("v"+pv))
		if out != "" || code != 0 {
			t.Errorf("out=%q code=%d", out, code)
		}
	})
	t.Run("mismatch prints exactly one JSON object", func(t *testing.T) {
		out, code := runVersionCheck(t, stub("9.9.9"))
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		var got struct {
			SystemMessage string `json:"systemMessage"`
			Hook          struct {
				Event   string `json:"hookEventName"`
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not one valid JSON object: %v\n%q", err, out)
		}
		if strings.Count(strings.TrimSpace(out), "\n") != 0 {
			t.Errorf("output spans several lines: %q", out)
		}
		for _, want := range []string{pv, "9.9.9", "differ"} {
			if !strings.Contains(got.SystemMessage, want) {
				t.Errorf("systemMessage %q lacks %q", got.SystemMessage, want)
			}
		}
		if got.Hook.Event != "SessionStart" || got.Hook.Context != got.SystemMessage {
			t.Errorf("hookSpecificOutput = %+v", got.Hook)
		}
	})
	t.Run("dev build gets its own wording", func(t *testing.T) {
		out, code := runVersionCheck(t, stub("dev(abc1234)+dirty"))
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		var got struct {
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("invalid JSON: %v\n%q", err, out)
		}
		if !strings.Contains(got.SystemMessage, "development build") || strings.Contains(got.SystemMessage, "differ") {
			t.Errorf("dev wording: %q", got.SystemMessage)
		}
	})
	// A missing binary yields an empty version, so this guards the empty-version
	// check (there is no separate `command -v` guard, #2207).
	t.Run("fabrik not installed is silent", func(t *testing.T) {
		out, code := runVersionCheck(t, "")
		if out != "" || code != 0 {
			t.Errorf("out=%q code=%d", out, code)
		}
	})
	t.Run("unreadable binary version is silent", func(t *testing.T) {
		out, code := runVersionCheck(t, "exit 1")
		if out != "" || code != 0 {
			t.Errorf("out=%q code=%d", out, code)
		}
	})
	t.Run("hostile version string is silent, never broken JSON", func(t *testing.T) {
		out, code := runVersionCheck(t, `echo 'x"}; rm -rf /'`)
		if out != "" || code != 0 {
			t.Errorf("out=%q code=%d", out, code)
		}
	})
	t.Run("opt-out is silent", func(t *testing.T) {
		out, code := runVersionCheck(t, stub("9.9.9"), "FABRIK_PLUGIN_VERSION_CHECK=off")
		if out != "" || code != 0 {
			t.Errorf("out=%q code=%d", out, code)
		}
	})
}
