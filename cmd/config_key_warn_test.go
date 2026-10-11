package cmd

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/config"
)

func TestWarnUnrecognizedConfigKeys_NoMatchingFlag(t *testing.T) {
	resetFlags()
	var buf bytes.Buffer

	warnUnrecognizedConfigKeys([]string{"totally_bogus_key"}, &buf)

	got := buf.String()
	if !strings.Contains(got, `[startup] warning: .fabrik/config.yaml: unrecognised key "totally_bogus_key"`) {
		t.Errorf("missing base warning line, got: %q", got)
	}
	if strings.Contains(got, "CLI/env-only") {
		t.Errorf("did not expect a suggestion clause for a key with no matching flag, got: %q", got)
	}
}

func TestWarnUnrecognizedConfigKeys_MatchingFlag(t *testing.T) {
	resetFlags()
	flag.String("archive-after", "", "test flag")
	var buf bytes.Buffer

	warnUnrecognizedConfigKeys([]string{"archive_after"}, &buf)

	got := buf.String()
	if !strings.Contains(got, `[startup] warning: .fabrik/config.yaml: unrecognised key "archive_after"`) {
		t.Errorf("missing base warning line, got: %q", got)
	}
	if !strings.Contains(got, "FABRIK_ARCHIVE_AFTER") {
		t.Errorf("expected suggested env var FABRIK_ARCHIVE_AFTER, got: %q", got)
	}
	if !strings.Contains(got, "-archive-after") {
		t.Errorf("expected suggested flag -archive-after, got: %q", got)
	}
}

func TestWarnUnrecognizedConfigKeys_MultipleKeysAllReported(t *testing.T) {
	resetFlags()
	flag.String("archive-after", "", "test flag")
	flag.String("reconcile-interval", "", "test flag")
	var buf bytes.Buffer

	warnUnrecognizedConfigKeys([]string{"archive_after", "reconcile_interval", "totally_bogus_key"}, &buf)

	got := buf.String()
	for _, want := range []string{
		`unrecognised key "archive_after"`,
		"FABRIK_ARCHIVE_AFTER",
		"-archive-after",
		`unrecognised key "reconcile_interval"`,
		"FABRIK_RECONCILE_INTERVAL",
		"-reconcile-interval",
		`unrecognised key "totally_bogus_key"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected output to contain %q, got: %q", want, got)
		}
	}
	// The bogus key must not get a suggestion clause.
	bogusIdx := strings.Index(got, `unrecognised key "totally_bogus_key"`)
	if bogusIdx == -1 {
		t.Fatal("bogus key line not found")
	}
	if strings.Contains(got[bogusIdx:], "CLI/env-only") {
		t.Errorf("did not expect a suggestion clause after the bogus key, got: %q", got[bogusIdx:])
	}
}

func TestWarnUnrecognizedConfigKeys_Empty(t *testing.T) {
	resetFlags()
	var buf bytes.Buffer

	warnUnrecognizedConfigKeys(nil, &buf)

	if buf.Len() != 0 {
		t.Errorf("expected no output for empty key list, got: %q", buf.String())
	}
}

// plugin_dirs is a real config.yaml key (ADR 2136); the singular plugin_dir
// stays CLI/env-only, so a stray one is still reported.
func TestUnrecognizedConfigKeys_PluginDirs(t *testing.T) {
	dir := t.TempDir()
	chdirTest(t, dir)
	if err := os.MkdirAll(".fabrik", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".fabrik/config.yaml", []byte("plugin_dirs:\n  - a\nplugin_dir: b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := config.UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "plugin_dir" {
		t.Fatalf("unrecognised = %v, want [plugin_dir]", got)
	}
}
