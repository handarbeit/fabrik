package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

func TestIsDegenerateOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// True positives: bare file references.
		{"at-ref absolute path", "@/tmp/plan_comment.md", true},
		{"bare absolute path", "/var/data/foo.md", true},
		{"at-ref bare filename", "@plan.md", true},
		{"at-ref relative path", "@relative/path.md", true},
		{"at-ref wrapped in backticks", "`@/tmp/plan_comment.md`", true},
		{"bare path wrapped in quotes", "\"/var/data/foo.md\"", true},

		// False positives to guard against.
		{"short prose N/A", "N/A", false},
		{"short prose TBD", "TBD", false},
		{"short sentence", "Plan is ready.", false},
		{"sentence mentioning a path", "See docs/README.md for details.", false},
		{"bare relative path, no @ or leading slash", "docs/README.md", false},
		{"empty string", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDegenerateOutput(c.in); got != c.want {
				t.Errorf("isDegenerateOutput(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestIsDegenerateOutput_MultiLineNeverFlagged confirms that a multi-line body is
// never treated as degenerate, even if its only non-blank line is a bare @-reference —
// a real plan/research body is never single-line, so this keeps the detector conservative.
func TestIsDegenerateOutput_MultiLineNeverFlagged(t *testing.T) {
	in := "Some heading\n@/tmp/x.md"
	if isDegenerateOutput(in) {
		t.Errorf("isDegenerateOutput(%q) = true, want false (multi-line body must never be flagged)", in)
	}

	in2 := "@/tmp/x.md\nmore content"
	if isDegenerateOutput(in2) {
		t.Errorf("isDegenerateOutput(%q) = true, want false (multi-line body must never be flagged)", in2)
	}
}

// TestInvokeClaude_ClaudeConfigDirSurvives pins #1350's R4/R5: CLAUDE_CONFIG_DIR,
// when present in the engine's own environment (typically via .env, per
// config.LoadDotenv), must survive into the environment constructed for a real
// Claude invocation. Neither buildClaudeEnv nor mergeEnv ever add an override
// entry for this key, so it passes through from os.Environ() untouched — this
// is the exact mechanism the issue describes as unpinned and at risk from #1346's
// planned mergeEnv rewrite. The two subtests assert on the constructed
// subprocess environment directly (not on invocation success), and the
// present/absent pairing proves the assertion is non-vacuous: a broken
// passthrough would make the "set" subtest fail while the "unset" subtest
// would pass regardless, so only the pairing together confirms the test can
// detect a regression.
func TestInvokeClaude_ClaudeConfigDirSurvives(t *testing.T) {
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	envFile := filepath.Join(binDir, "env.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
env > %s
printf '%%s\n' '{"result":"claude config dir test\nFABRIK_STAGE_COMPLETE\n","session_id":"sess_claudeconfigdir","num_turns":1,"total_cost_usd":0.0}'
`, envFile)
	if err := os.WriteFile(fakeClaude, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	workDir := t.TempDir()
	stage := &stages.Stage{
		Name:       "Implement",
		Prompt:     "Implement it",
		Completion: stages.CompletionCriteria{Type: "claude"},
	}
	issue := gh.ProjectItem{Number: 1350, Repo: "acme/widgets", Title: "CLAUDE_CONFIG_DIR passthrough test"}

	t.Run("present in engine env survives into the constructed subprocess env", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "/home/user/.claude-alt-profile")
		_, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		if err != nil {
			t.Fatalf("InvokeClaude: %v", err)
		}
		data, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatalf("reading env file: %v", err)
		}
		env := string(data)
		if !strings.Contains(env, "CLAUDE_CONFIG_DIR=/home/user/.claude-alt-profile") {
			t.Errorf("expected CLAUDE_CONFIG_DIR to survive into the subprocess env, got:\n%s", env)
		}
	})

	t.Run("absent from engine env stays absent from the constructed subprocess env", func(t *testing.T) {
		if prev, ok := os.LookupEnv("CLAUDE_CONFIG_DIR"); ok {
			os.Unsetenv("CLAUDE_CONFIG_DIR")
			t.Cleanup(func() { os.Setenv("CLAUDE_CONFIG_DIR", prev) })
		}
		_, _, _, err := InvokeClaude(context.Background(), stage, issue, nil, false, workDir, InvokeOptions{})
		if err != nil {
			t.Fatalf("InvokeClaude: %v", err)
		}
		data, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatalf("reading env file: %v", err)
		}
		env := string(data)
		if strings.Contains(env, "CLAUDE_CONFIG_DIR") {
			t.Errorf("expected CLAUDE_CONFIG_DIR to be entirely absent, got:\n%s", env)
		}
	})
}
