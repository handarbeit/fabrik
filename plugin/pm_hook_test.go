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

// runVersionCheckAt runs the hook against a temp plugin root whose manifest
// declares pluginVersion, so the directional cases do not depend on the real
// manifest version (which every release cut bumps). The trailing
// CLAUDE_PLUGIN_ROOT entry overrides the one runVersionCheck sets (exec.Cmd
// keeps the last duplicate).
func runVersionCheckAt(t *testing.T, pluginVersion, stubBody string, env ...string) (string, int) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"fabrik","version":"` + pluginVersion + `"}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return runVersionCheck(t, stubBody, append([]string{"CLAUDE_PLUGIN_ROOT=" + root}, env...)...)
}

// hookMessage decodes the hook's single JSON object and returns its message,
// failing unless systemMessage and additionalContext agree.
func hookMessage(t *testing.T, out string) string {
	t.Helper()
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
	if got.Hook.Event != "SessionStart" || got.Hook.Context != got.SystemMessage {
		t.Errorf("hookSpecificOutput = %+v", got.Hook)
	}
	return got.SystemMessage
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
	t.Run("plugin behind names only the plugin remedy", func(t *testing.T) {
		out, code := runVersionCheckAt(t, "0.0.91", stub("0.0.92"))
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		msg := hookMessage(t, out)
		for _, want := range []string{"0.0.91", "0.0.92", "/plugin marketplace update fabrik", "/plugin update fabrik@fabrik"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
		}
		if strings.Index(msg, "marketplace update") > strings.Index(msg, "/plugin update") {
			t.Errorf("marketplace refresh must come first: %q", msg)
		}
		if strings.Contains(msg, "fabrik upgrade") || strings.Contains(msg, "differ") {
			t.Errorf("plugin-behind message names the binary remedy: %q", msg)
		}
	})
	t.Run("binary behind names only the binary remedy", func(t *testing.T) {
		out, code := runVersionCheckAt(t, "0.0.93", stub("0.0.92"))
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		msg := hookMessage(t, out)
		for _, want := range []string{"0.0.93", "0.0.92", "fabrik upgrade"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q lacks %q", msg, want)
			}
		}
		if strings.Contains(msg, "/plugin") || strings.Contains(msg, "differ") {
			t.Errorf("binary-behind message names the plugin remedy: %q", msg)
		}
	})
	t.Run("comparison is numeric, tolerates v and leading zeros", func(t *testing.T) {
		for _, c := range []struct {
			plugin, binary string
			pluginBehind   bool
		}{
			{"0.0.9", "v0.0.10", true},
			{"0.0.10", "0.0.9", false},
			{"0.0.09", "0.0.10", true},
			{"0.0.10", "0.0.09", false},
			{"v0.0.9", "0.0.10", true},
			{"1.9.0", "1.10.0", true},
			{"2.0.0", "1.99.99", false},
			{"99999999999999999999998.0.0", "99999999999999999999999.0.0", true},
			{"99999999999999999999999.0.0", "99999999999999999999998.0.0", false},
		} {
			out, code := runVersionCheckAt(t, c.plugin, stub(c.binary))
			if code != 0 {
				t.Fatalf("%s vs %s: exit %d", c.plugin, c.binary, code)
			}
			msg := hookMessage(t, out)
			hasPlugin, hasBinary := strings.Contains(msg, "/plugin update"), strings.Contains(msg, "fabrik upgrade")
			if hasPlugin != c.pluginBehind || hasBinary == c.pluginBehind {
				t.Errorf("%s vs %s: pluginBehind=%v but message %q", c.plugin, c.binary, c.pluginBehind, msg)
			}
		}
	})
	t.Run("numerically equal versions are silent", func(t *testing.T) {
		for _, c := range [][2]string{{"v0.0.92", "0.0.92"}, {"0.0.09", "0.0.9"}, {"0.0.92", "v0.0.92"}} {
			out, code := runVersionCheckAt(t, c[0], stub(c[1]))
			if out != "" || code != 0 {
				t.Errorf("%v: out=%q code=%d", c, out, code)
			}
		}
	})
	t.Run("unparseable versions keep the two-remedy wording", func(t *testing.T) {
		for _, c := range [][2]string{
			{"0.0.92", "0.0.93-rc1"},
			{"0.0.92", "0.0.93+abc"},
			{"0.0.93-rc1", "0.0.92"},
			{"0.0.92", "0.0"},
			{"0.0.92", "1.2.3.4"},
			{"0.0.92", "1.2.3."},
			{"0.0.92", "1..3"},
			{"0.0.92", "garbage"},
		} {
			out, code := runVersionCheckAt(t, c[0], stub(c[1]))
			if code != 0 {
				t.Fatalf("%v: exit %d", c, code)
			}
			msg := hookMessage(t, out)
			for _, want := range []string{c[0], c[1], "differ", "/plugin update fabrik@fabrik", "fabrik upgrade"} {
				if !strings.Contains(msg, want) {
					t.Errorf("%v: message %q lacks %q", c, msg, want)
				}
			}
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
		if !strings.Contains(got.SystemMessage, "development build") || strings.Contains(got.SystemMessage, "differ") ||
			strings.Contains(got.SystemMessage, "fabrik upgrade") || strings.Contains(got.SystemMessage, "/plugin") {
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
