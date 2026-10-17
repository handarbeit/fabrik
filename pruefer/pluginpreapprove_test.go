package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var preapproveGrant = []string{"Bash(cat *)", "Bash(shellcheck *)", "Task"}

func TestSplitToolEntries(t *testing.T) {
	for in, want := range map[string][]string{
		"Bash(git log:*), Read":              {"Bash(git log:*)", "Read"},
		"Bash(git log:*) Read":               {"Bash(git log:*)", "Read"},
		"Read,Grep ,  Glob":                  {"Read", "Grep", "Glob"},
		"Bash(a b, c d)":                     {"Bash(a b, c d)"},
		"":                                   nil,
		"Bash(go test:*)":                    {"Bash(go test:*)"},
		"Bash( go   test:* )":                {"Bash( go test:* )"},
		"Read\nGrep":                         {"Read", "Grep"},
		"Bash(shellcheck *),Bash(go test:*)": {"Bash(shellcheck *)", "Bash(go test:*)"},
	} {
		if got := splitToolEntries(in); !slices.Equal(got, want) {
			t.Errorf("splitToolEntries(%q) = %q, want %q", in, got, want)
		}
	}
}

// R3 and R1 for an outside author: no allowed-tools key survives in any skill,
// agent or command, whatever its spelling or shape, and the removal is reported.
func TestStripPluginHooks_RemovesToolPreapprovalsFromEveryFrontmatter(t *testing.T) {
	src := filepath.Join(t.TempDir(), "pre")
	writeTree(t, src, map[string]string{
		".claude-plugin/plugin.json": `{"name":"pre","version":"1.0.0"}`,
		"skills/a/SKILL.md":          "---\nname: a\ndescription: d\nallowed-tools: Bash(go test:*), Read\n---\nbody a\n",
		"skills/b/SKILL.md":          "---\nname: b\ndescription: d\nallowed_tools:\n  - Bash(go test:*)\n---\nbody b\n",
		"agents/c.md":                "---\nname: c\nallowedTools: \"Bash(go test:*)\"\n---\nbody c\n",
		"commands/d.md":              "---\ndescription: d\nallowed-tools: Bash(go test:*)\n---\nbody d\n",
	})
	dst := filepath.Join(t.TempDir(), "copy", "pre")
	rep, err := stripPluginHooks(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, rel := range []string{"skills/a/SKILL.md", "skills/b/SKILL.md", "agents/c.md", "commands/d.md"} {
		data, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("%s not copied: %v", rel, err)
		}
		if strings.Contains(strings.ToLower(string(data)), "allowed") || strings.Contains(string(data), "go test") {
			t.Errorf("%s still pre-approves a tool:\n%s", rel, data)
		}
		if !strings.Contains(string(data), "body ") {
			t.Errorf("%s lost its body:\n%s", rel, data)
		}
	}
	for _, r := range rep.Removed {
		if strings.Contains(r, "frontmatter key") {
			seen++
		}
	}
	if seen != 4 {
		t.Errorf("Removed = %q, want one report per allowed-tools key (4)", rep.Removed)
	}
}

// An outside author's review reports the plugin as stripped when the only thing
// removed was a tool pre-approval.
func TestStripOperatorPlugins_CountsToolPreapprovalRemovalAsStripped(t *testing.T) {
	src := filepath.Join(t.TempDir(), "pre")
	writeTree(t, src, map[string]string{
		"skills/a/SKILL.md": "---\nname: a\ndescription: d\nallowed-tools: Bash(go test:*)\n---\nx\n",
	})
	_, rep := stripOperatorPlugins(t.TempDir(), []string{src}, 1)
	if !slices.Equal(rep.Stripped, []string{"pre"}) {
		t.Errorf("report = %+v, want pre stripped", rep)
	}
}

func preapprovePlugin(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ops")
	files[".claude-plugin/plugin.json"] = `{"name":"ops","version":"1.0.0"}`
	writeTree(t, dir, files)
	return dir
}

// A trusted author's review keeps a pre-approval that is already in the grant
// and drops one that is not, without touching the operator's plugin on disk.
func TestSanitizeOperatorPreapprovals_TrustedAuthor(t *testing.T) {
	inGrant := preapprovePlugin(t, map[string]string{
		"skills/a/SKILL.md": "---\nname: a\ndescription: d\nallowed-tools: Bash(shellcheck *), Bash(cat *), Task\n---\nx\n",
		"skills/b/SKILL.md": "---\nname: b\ndescription: d\nallowed-tools:\n  - Bash(cat *)\n---\nx\n",
		"skills/c/SKILL.md": "---\nname: c\ndescription: d\n---\nno pre-approval\n",
	})
	outOfGrant := preapprovePlugin(t, map[string]string{
		"skills/a/SKILL.md": "---\nname: a\ndescription: d\nallowed-tools: Bash(shellcheck *), Bash(go test:*)\n---\nbody a\n",
		"agents/b.md":       "---\nname: b\nallowed_tools: [Read]\n---\nbody b\n",
		"skills/d/SKILL.md": "---\nname: d\ndescription: d\nallowed-tools: \"*\"\n---\nbody d\n",
		"skills/e/SKILL.md": "---\nname: e\nallowed-tools:\n  nested: map\n---\nbody e\n",
		"hooks/hooks.json":  `{"PreToolUse":[]}`,
		"scripts/run.sh":    "#!/bin/sh\necho ok\n",
	})
	if err := os.Chmod(filepath.Join(outOfGrant, "scripts", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("skills", filepath.Join(outOfGrant, "alias")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(outOfGrant, "skills/a/SKILL.md"))

	root := t.TempDir()
	out, skipped := sanitizeOperatorPreapprovals(root, []string{inGrant, outOfGrant}, preapproveGrant, 1)
	if len(skipped) != 0 || len(out) != 2 {
		t.Fatalf("out = %v skipped = %v", out, skipped)
	}
	if out[0] != inGrant {
		t.Errorf("a plugin pre-approving only granted tools must be used in place, got %s", out[0])
	}
	if !strings.HasPrefix(out[1], root) || filepath.Base(out[1]) != "ops" {
		t.Fatalf("out-of-grant plugin not replaced by a copy under the per-review root: %s", out[1])
	}
	for _, rel := range []string{"skills/a/SKILL.md", "agents/b.md", "skills/d/SKILL.md", "skills/e/SKILL.md"} {
		data, err := os.ReadFile(filepath.Join(out[1], rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if strings.Contains(strings.ToLower(string(data)), "allowed") || strings.Contains(string(data), "go test") {
			t.Errorf("%s still pre-approves tools:\n%s", rel, data)
		}
		if !strings.Contains(string(data), "body ") {
			t.Errorf("%s lost its body:\n%s", rel, data)
		}
	}
	// The trusted copy is otherwise the plugin as it is: hooks and scripts stay,
	// the executable bit and symlinks are kept.
	for _, rel := range []string{"hooks/hooks.json", "scripts/run.sh", ".claude-plugin/plugin.json"} {
		if _, err := os.Stat(filepath.Join(out[1], rel)); err != nil {
			t.Errorf("%s missing from the trusted copy: %v", rel, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(out[1], "scripts/run.sh")); err != nil || fi.Mode()&0o100 == 0 {
		t.Errorf("executable bit lost: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(out[1], "alias")); err != nil || target != "skills" {
		t.Errorf("symlink not preserved: %q %v", target, err)
	}
	if after, _ := os.ReadFile(filepath.Join(outOfGrant, "skills/a/SKILL.md")); string(after) != string(before) {
		t.Errorf("the operator's plugin on disk was modified")
	}
}

// Frontmatter this code cannot parse is left out of the copy when it mentions a
// pre-approval, and left alone when it does not.
func TestSanitizeOperatorPreapprovals_UnparseableFrontmatter(t *testing.T) {
	p := preapprovePlugin(t, map[string]string{
		"skills/bad/SKILL.md": "---\nname: [unclosed\nallowed-tools: Bash(go test:*)\n---\nbody\n",
		"README.md":           "---\nnot frontmatter, a rule line\n",
		"skills/ok/SKILL.md":  "---\nname: ok\ndescription: d\n---\nok\n",
	})
	out, _ := sanitizeOperatorPreapprovals(t.TempDir(), []string{p}, preapproveGrant, 1)
	if out[0] == p {
		t.Fatal("a plugin with unreadable pre-approving frontmatter was used in place")
	}
	if _, err := os.Stat(filepath.Join(out[0], "skills/bad/SKILL.md")); err == nil {
		t.Error("the unreadable file was copied")
	}
	for _, rel := range []string{"skills/ok/SKILL.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(out[0], rel)); err != nil {
			t.Errorf("%s should be kept: %v", rel, err)
		}
	}
	// With no pre-approval mentioned anywhere the same README does not force a copy.
	q := preapprovePlugin(t, map[string]string{"README.md": "---\nrule\n", "skills/ok/SKILL.md": "---\nname: ok\n---\nok\n"})
	if out, _ := sanitizeOperatorPreapprovals(t.TempDir(), []string{q}, preapproveGrant, 1); out[0] != q {
		t.Errorf("an unrelated unparseable markdown file forced a copy: %v", out)
	}
}

// End to end through Review: whatever the CLI is given never pre-approves a tool
// outside the grant the review runs with. A pre-approval inside the grant
// survives for a trusted author; an outside author's copy has none at all.
func TestReview_PluginToolPreapprovalsNeverWidenTheGrant(t *testing.T) {
	mixed := preapprovePlugin(t, map[string]string{
		"skills/s/SKILL.md": "---\nname: s\ndescription: d\nallowed-tools: Bash(shellcheck *), Bash(go test:*)\n---\nSKILLBODY\n",
	})
	granted := preapprovePlugin(t, map[string]string{
		"skills/s/SKILL.md": "---\nname: s\ndescription: d\nallowed-tools: Bash(shellcheck *)\n---\nSKILLBODY\n",
	})
	for name, tc := range map[string]struct {
		plugin         string
		grant          reviewGrant
		wantShellcheck bool
	}{
		"trusted, mixed entries: the key is dropped whole": {mixed, resolveReviewGrant(shellcheckGrant, nil, "alice", "MEMBER"), false},
		"trusted, tool granted: the pre-approval stays":    {granted, resolveReviewGrant(shellcheckGrant, nil, "alice", "MEMBER"), true},
		"trusted, tool not granted: dropped":               {granted, resolveReviewGrant(nil, nil, "alice", "MEMBER"), false},
		"outside author: never kept":                       {granted, resolveReviewGrant(shellcheckGrant, nil, "mallory", "NONE"), false},
		"outside author, mixed":                            {mixed, resolveReviewGrant(shellcheckGrant, nil, "mallory", "NONE"), false},
	} {
		t.Run(name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "skills.txt")
			writeFakeClaude(t, `prev=""
for a in "$@"; do
  if [ "$prev" = "--plugin-dir" ]; then cat "$a"/skills/*/SKILL.md >> "`+rec+`" 2>/dev/null; fi
  prev="$a"
done
echo '`+resultLine+`'
`)
			req := ReviewRequest{PRNumber: 1, WorkDir: t.TempDir(), OperatorPluginDirs: []string{tc.plugin}, Grant: tc.grant}
			if _, err := (&RealClaudeInvoker{}).Review(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(rec)
			got := string(data)
			if !strings.Contains(got, "SKILLBODY") {
				t.Fatalf("the operator skill was not loaded:\n%s", got)
			}
			if strings.Contains(got, "go test") {
				t.Errorf("the CLI was given a skill pre-approving go test:\n%s", got)
			}
			if has := strings.Contains(got, "shellcheck"); has != tc.wantShellcheck {
				t.Errorf("shellcheck pre-approval present = %v, want %v:\n%s", has, tc.wantShellcheck, got)
			}
		})
	}
}
