package ciskip

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// skippablePaths are paths that, alone, must let CI skip the Go suites.
var skippablePaths = []string{
	"release-notes/v0.0.85.md",
	"notes/plan.md",
	"specs/2254-x/spec.md",
	"docs/stage-lifecycle.md",
	"docs/positioning.md",
	"docs/llms-full.txt",
	"adrs/1857-no-per-pr-test-selection.md",
	"README.md",
	"CONTRIBUTING.md",
	".claude/rules/golang.md",
	"scripts/cut-release.sh",
	"scripts/lib/killgroup.sh",
	"scripts/check-adr-references.sh",
	"scripts/e2e/run.sh",
}

// neverSkipPaths are paths that, alone, must force the full suite.
var neverSkipPaths = []string{
	"main.go",
	"engine/engine.go",
	"engine/engine_test.go",
	"docs/x/gen.go", // a .go file hidden under a skippable-looking directory
	"go.mod",
	"go.sum",
	"tools/x/go.mod",
	"plugin/fabrik/skills/x/SKILL.md",
	"stages/examples/plan.yaml",
	"engine/testdata/fixture.json",
	"docs/testdata/x.md", // testdata under a skippable directory
	"tests/e2e/registry/registry.json",
	".claude-plugin/marketplace.json",
	".github/workflows/ci.yml",
	"scripts/ci/classify-diff.sh",
	"scripts/ci/go-skip-paths.txt",
	"scripts/ci/worker-shaped-test.sh",
	"scripts/lib/parallel.sh",
	"CLAUDE.md",
	"docs/state-machine.md",
	"docs/USER_GUIDE.md",
	"adrs/1966-b-fabrik-mcp-channel-events.md",
	"tests/ciskip/classify_test.go",
}

// unknownPaths match neither list: fail safe means the full suite runs.
var unknownPaths = []string{
	"Makefile",
	"LICENSE",
	"newdir/file.txt",
	"streamfilter/notes.txt",
}

// unparseablePaths cannot be normalised and must force the full suite.
var unparseablePaths = []string{
	"",
	"/etc/passwd",
	"./docs/x.md",
	"docs/../plugin/x.md",
	"docs//x.md",
	"docs/./x.md",
	"docs\\x.md",
	"..",
}

func TestClassifier_SkippablePathsSkip(t *testing.T) {
	t.Parallel()
	for _, p := range skippablePaths {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			if r := classifyPaths(t, nil, p); !r.Skip {
				t.Errorf("%q should be skippable, got run-everything:\n%s", p, r.Stdout)
			}
		})
	}
}

func TestClassifier_NeverSkipPathsRunEverything(t *testing.T) {
	t.Parallel()
	for _, p := range neverSkipPaths {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			if r := classifyPaths(t, nil, p); r.Skip {
				t.Errorf("%q must force the full suite, got skip:\n%s", p, r.Stdout)
			}
		})
	}
}

func TestClassifier_UnknownAndUnparseablePathsRunEverything(t *testing.T) {
	t.Parallel()
	for _, p := range append(append([]string{}, unknownPaths...), unparseablePaths...) {
		t.Run(fmt.Sprintf("%q", p), func(t *testing.T) {
			t.Parallel()
			if r := classifyPaths(t, nil, p); r.Skip {
				t.Errorf("%q must fail safe to the full suite, got skip:\n%s", p, r.Stdout)
			}
		})
	}
}

func TestClassifier_MixedListRunsEverything(t *testing.T) {
	t.Parallel()
	r := classifyPaths(t, nil, "release-notes/v1.md", "specs/x/spec.md", "engine/engine.go")
	if r.Skip {
		t.Errorf("one Go-input path in a list of skippable ones must run everything:\n%s", r.Stdout)
	}
	if !r.CutRelease || !r.Killgroup || !r.ADRRefs {
		t.Errorf("a full run must select every shell check, got %+v", r)
	}
	if !strings.Contains(r.Stdout, "engine/engine.go") {
		t.Errorf("the log must name the offending path:\n%s", r.Stdout)
	}
}

func TestClassifier_EmptyDiffRunsEverything(t *testing.T) {
	t.Parallel()
	r := classifyPaths(t, nil)
	if r.Skip {
		t.Errorf("an empty diff is not docs-only:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "empty diff") {
		t.Errorf("the log must state the reason:\n%s", r.Stdout)
	}
}

func TestClassifier_ShellCheckSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                    string
		paths                   []string
		cutRelease, kg, adrRefs bool
	}{
		{"cut-release only", []string{"scripts/cut-release.sh"}, true, false, false},
		{"gate test only", []string{"scripts/cut_release_gate_test.sh"}, true, false, false},
		{"scripts/lib", []string{"scripts/lib/killgroup.sh"}, true, true, false},
		{"adrs", []string{"adrs/1857-no-per-pr-test-selection.md"}, false, false, true},
		{"docs", []string{"docs/positioning.md"}, false, false, true},
		{"adr checker", []string{"scripts/check-adr-references.sh"}, false, false, true},
		{"release notes", []string{"release-notes/v1.md"}, false, false, false},
		{"top-level md", []string{"README.md"}, false, false, false},
		{"specs", []string{"specs/x/spec.md"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := classifyPaths(t, nil, tc.paths...)
			if !r.Skip {
				t.Fatalf("expected skip:\n%s", r.Stdout)
			}
			if r.CutRelease != tc.cutRelease || r.Killgroup != tc.kg || r.ADRRefs != tc.adrRefs {
				t.Errorf("cut_release/killgroup/adr_refs = %v/%v/%v, want %v/%v/%v",
					r.CutRelease, r.Killgroup, r.ADRRefs, tc.cutRelease, tc.kg, tc.adrRefs)
			}
		})
	}
}

func TestClassifier_SkipReportsAndSummary(t *testing.T) {
	t.Parallel()
	r := classifyPaths(t, nil, "release-notes/v1.md", "specs/x/spec.md")
	if !r.Skip {
		t.Fatalf("expected skip:\n%s", r.Stdout)
	}
	for _, want := range []string{"release-notes/v1.md", "specs/x/spec.md", "skippable rule"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("log must contain %q:\n%s", want, r.Stdout)
		}
	}
	if !strings.Contains(r.Summary, "Go tests skipped: no Go build inputs changed") {
		t.Errorf("job summary must carry the skip message, got %q", r.Summary)
	}
	if full := classifyPaths(t, nil, "engine/engine.go"); full.Summary != "" {
		t.Errorf("a full run must not write the skip summary, got %q", full.Summary)
	}
}

// TestClassifier_NeutralisedGuardIsDetected is the non-vacuity test (FR-012):
// with the never-skip rules switched off, the never-skip paths that sit under a
// skippable directory classify as skip, so the assertions in
// TestClassifier_NeverSkipPathsRunEverything would fail without the guard.
func TestClassifier_NeutralisedGuardIsDetected(t *testing.T) {
	t.Parallel()
	guarded := []string{
		"docs/x/gen.go", "docs/testdata/x.md", "docs/state-machine.md", "docs/USER_GUIDE.md",
		"adrs/1966-b-fabrik-mcp-channel-events.md", "scripts/ci/classify-diff.sh",
		"scripts/lib/parallel.sh", "CLAUDE.md",
	}
	for _, p := range guarded {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			if r := classifyPaths(t, nil, p); r.Skip {
				t.Fatalf("with the guard on, %q must run everything", p)
			}
			neutral := classifyPaths(t, []string{"CLASSIFY_DISABLE_NEVER_SKIP=1"}, p)
			// CLAUDE.md is a top-level *.md, the rest sit under skippable dirs.
			if !neutral.Skip {
				t.Errorf("with the guard disabled %q should have been skipped — the never-skip rule is not what protects it:\n%s", p, neutral.Stdout)
			}
		})
	}
}

func TestClassifier_BadRulesFileFailsSafe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := filepath.Join(dir, "rules.txt")
	if err := os.WriteFile(bad, []byte("bogus docs/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := classifyPaths(t, []string{"CLASSIFY_RULES_FILE=" + bad}, "release-notes/v1.md")
	if r.Skip || !r.CutRelease || !r.Killgroup || !r.ADRRefs {
		t.Errorf("an unloadable rule table must run everything, got %+v", r)
	}
	missing := classifyPaths(t, []string{"CLASSIFY_RULES_FILE=" + filepath.Join(dir, "nope.txt")}, "release-notes/v1.md")
	if missing.Skip {
		t.Error("a missing rule table must run everything")
	}
}

// TestClassifier_OddFilenames checks NUL-delimited parsing of awkward names.
func TestClassifier_OddFilenames(t *testing.T) {
	t.Parallel()
	if r := classifyPaths(t, nil, "docs/with space.md", "docs/new\nline.md", "notes/q'uote.md"); !r.Skip {
		t.Errorf("odd but skippable names must classify as skip:\n%s", r.Stdout)
	}
	if r := classifyPaths(t, nil, "docs/a b/gen.go"); r.Skip {
		t.Errorf("a .go file with a space in its path must run everything:\n%s", r.Stdout)
	}
}

// --- real git -----------------------------------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mergeRepo builds a scratch repo whose HEAD is a two-parent merge of a PR
// branch (applying change) into a base, the shape actions/checkout gives a
// pull_request run. The base moves on after the PR forks so HEAD^1 != merge-base.
func mergeRepo(t *testing.T, change func(dir string)) string {
	t.Helper()
	skipIfNoGit(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "plugin/a.txt", "a\n")
	writeFile(t, dir, "docs/a.md", "a\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	git(t, dir, "checkout", "-q", "-b", "pr")
	change(dir)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "pr")
	git(t, dir, "checkout", "-q", "main")
	// The base advances with a Go change the PR did not make; it must not
	// appear in the classified diff (HEAD^1..HEAD is what the merge adds).
	writeFile(t, dir, "engine/base.go", "package engine\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base moves")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge pr", "pr")
	return dir
}

func TestClassifier_RealMergeCommit(t *testing.T) {
	t.Parallel()
	t.Run("docs-only PR skips", func(t *testing.T) {
		t.Parallel()
		dir := mergeRepo(t, func(d string) { writeFile(t, d, "release-notes/v1.md", "x\n") })
		if r := runScript(t, dir, nil, nil); !r.Skip {
			t.Errorf("docs-only merge should skip:\n%s", r.Stdout)
		}
	})
	t.Run("go change runs everything", func(t *testing.T) {
		t.Parallel()
		dir := mergeRepo(t, func(d string) { writeFile(t, d, "engine/x.go", "package engine\n") })
		if r := runScript(t, dir, nil, nil); r.Skip {
			t.Errorf("a .go change must run everything:\n%s", r.Stdout)
		}
	})
	t.Run("rename out of plugin runs everything", func(t *testing.T) {
		t.Parallel()
		dir := mergeRepo(t, func(d string) {
			if err := os.MkdirAll(filepath.Join(d, "notes"), 0o755); err != nil {
				t.Fatal(err)
			}
			git(t, d, "mv", "plugin/a.txt", "notes/a.txt")
		})
		r := runScript(t, dir, nil, nil)
		if r.Skip {
			t.Errorf("the old path under plugin/ must be classified:\n%s", r.Stdout)
		}
		if !strings.Contains(r.Stdout, "plugin/a.txt") {
			t.Errorf("both sides of a rename must be reported:\n%s", r.Stdout)
		}
	})
	t.Run("empty merge diff runs everything", func(t *testing.T) {
		t.Parallel()
		// The PR commit changes nothing, so the merge adds no paths.
		dir := mergeRepo(t, func(string) {})
		if r := runScript(t, dir, nil, nil); r.Skip {
			t.Errorf("an empty diff must not skip:\n%s", r.Stdout)
		}
	})
}

func TestClassifier_NonMergeHeadFailsSafe(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "docs/a.md", "a\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "one")
	writeFile(t, dir, "docs/b.md", "b\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "two")
	r := runScript(t, dir, nil, nil)
	if r.Skip || !r.CutRelease || !r.Killgroup || !r.ADRRefs {
		t.Errorf("a non-merge HEAD must run everything, got %+v", r)
	}
	if !strings.Contains(r.Stdout, "not a merge commit") {
		t.Errorf("the log must state the reason:\n%s", r.Stdout)
	}
}

func TestClassifier_NotARepoFailsSafe(t *testing.T) {
	t.Parallel()
	skipIfNoGit(t)
	if r := runScript(t, t.TempDir(), nil, nil); r.Skip {
		t.Errorf("no repository must run everything:\n%s", r.Stdout)
	}
}
