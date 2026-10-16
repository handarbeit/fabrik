package pruefer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/skillref"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// hostilePlugin declares hooks and MCP servers in every place Claude Code reads
// them from. The sentinel PreToolUse/EVIL-MCP strings must appear nowhere in a
// stripped copy.
func hostilePlugin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ops")
	writeTree(t, dir, map[string]string{
		".claude-plugin/plugin.json": `{"name":"ops","version":"1.2.3","description":"d","hooks":{"PreToolUse":[{"command":"EVIL-HOOK"}]},"mcpServers":{"x":{"command":"EVIL-MCP"}},"surprise":{"k":"v"},"skills":"./extra"}`,
		"hooks/hooks.json":           `{"PreToolUse":[{"command":"EVIL-HOOK"}]}`,
		".mcp.json":                  `{"mcpServers":{"x":{"command":"EVIL-MCP"}}}`,
		"scripts/run.sh":             "#!/bin/sh\necho EVIL-SCRIPT\n",
		"skills/review/SKILL.md":     "---\nname: review\ndescription: Reviews\nhooks:\n  PreToolUse:\n    - command: EVIL-HOOK\nmcpServers:\n  x:\n    command: EVIL-MCP\n---\n\nBody text.\n",
		"skills/review/hooks.json":   `{"PreToolUse":[{"command":"EVIL-HOOK"}]}`,
		"skills/review/notes.md":     "plain notes\n",
		"extra/more/SKILL.md":        "---\nname: more\ndescription: More\n---\nmore\n",
		"agents/a.md":                "---\nname: a\n\"mcp-servers\": {x: {command: EVIL-MCP}}\n---\nagent\n",
		"commands/c.md":              "no frontmatter, mentions hooks in prose\n",
		"skills/broken/SKILL.md":     "---\nname: [unclosed\nhooks: EVIL-HOOK\n---\nbody\n",
		"skills/unclosed/SKILL.md":   "---\nname: u\nhooks: EVIL-HOOK\nno closing fence\n",
	})
	if err := os.Symlink("/etc", filepath.Join(dir, "skills", "link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStripPluginHooks_RemovesEveryHookAndMCPDeclaration(t *testing.T) {
	src := hostilePlugin(t)
	dst := filepath.Join(t.TempDir(), "copy", "ops")
	rep, err := stripPluginHooks(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) == 0 {
		t.Fatal("nothing reported as removed")
	}
	var files []string
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("symlink copied: %s", p)
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dst, p)
			files = append(files, rel)
			data, _ := os.ReadFile(p)
			for _, bad := range []string{"EVIL-HOOK", "EVIL-MCP", "EVIL-SCRIPT"} {
				if strings.Contains(string(data), bad) {
					t.Errorf("%s still contains %s:\n%s", rel, bad, data)
				}
			}
			if fi, _ := d.Info(); fi.Mode().Perm()&0o111 != 0 {
				t.Errorf("%s is executable in the copy", rel)
			}
		}
		return nil
	})
	for _, absent := range []string{"hooks", ".mcp.json", "scripts", "skills/link", "skills/broken/SKILL.md", "skills/unclosed/SKILL.md", "skills/review/hooks.json"} {
		if _, err := os.Lstat(filepath.Join(dst, absent)); err == nil {
			t.Errorf("%s was copied (files: %v)", absent, files)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(dst, ".claude-plugin", "plugin.json")); strings.Contains(string(raw), "surprise") || strings.Contains(string(raw), "hooks") || strings.Contains(string(raw), "mcpServers") {
		t.Errorf("manifest not cut down:\n%s", raw)
	}

	// What the plugin is for survives: name, version, every skill, the
	// frontmatter's other keys and the body.
	p := skillref.Discover(dst)
	if p.Name != "ops" || p.Version != "1.2.3" {
		t.Errorf("identity = %s %s", p.Name, p.Version)
	}
	if got := strings.Join(p.SkillNames(), ","); got != "more,review" {
		t.Errorf("skills = %q, want more,review", got)
	}
	md, _ := os.ReadFile(filepath.Join(dst, "skills", "review", "SKILL.md"))
	for _, want := range []string{"name: review", "description: Reviews", "Body text."} {
		if !strings.Contains(string(md), want) {
			t.Errorf("SKILL.md lost %q:\n%s", want, md)
		}
	}
	if c, _ := os.ReadFile(filepath.Join(dst, "commands", "c.md")); string(c) != "no frontmatter, mentions hooks in prose\n" {
		t.Errorf("a file without frontmatter changed: %q", c)
	}
	// The source is never modified.
	if raw, _ := os.ReadFile(filepath.Join(src, ".mcp.json")); !strings.Contains(string(raw), "EVIL-MCP") {
		t.Error("the operator's own plugin was modified")
	}
}

func TestStripPluginHooks_PluginWithoutHooksReportsNothingRemoved(t *testing.T) {
	src := filepath.Join(t.TempDir(), "clean")
	writeTree(t, src, map[string]string{
		".claude-plugin/plugin.json": `{"name":"clean","version":"1"}`,
		"skills/s/SKILL.md":          "---\nname: s\ndescription: d\n---\nx\n",
	})
	dst := filepath.Join(t.TempDir(), "clean")
	rep, err := stripPluginHooks(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 0 {
		t.Errorf("Removed = %v, want none", rep.Removed)
	}
	if got := skillref.Discover(dst).SkillNames(); len(got) != 1 {
		t.Errorf("skills = %v", got)
	}
}

func TestStripPluginHooks_RootSingleSkillAndNoManifest(t *testing.T) {
	src := filepath.Join(t.TempDir(), "solo")
	writeTree(t, src, map[string]string{
		"SKILL.md":   "---\nname: solo\nhooks: {a: EVIL-HOOK}\n---\nx\n",
		"hooks/h.sh": "EVIL-HOOK",
	})
	dst := filepath.Join(t.TempDir(), "solo")
	rep, err := stripPluginHooks(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) < 2 {
		t.Errorf("Removed = %v, want the hooks dir and the frontmatter key", rep.Removed)
	}
	if p := skillref.Discover(dst); p.Name != "solo" || len(p.Skills) != 1 {
		t.Errorf("plugin = %+v", p)
	}
}

func TestStripPluginHooks_ManifestPathsCannotEscapeOrReachHooks(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "p")
	writeTree(t, src, map[string]string{
		".claude-plugin/plugin.json": `{"name":"p","skills":["../outside","hooks","./.claude-plugin","/etc"]}`,
		"hooks/x/SKILL.md":           "EVIL-HOOK",
	})
	writeTree(t, filepath.Join(base, "outside"), map[string]string{"y/SKILL.md": "OUTSIDE"})
	dst := filepath.Join(t.TempDir(), "p")
	if _, err := stripPluginHooks(src, dst); err != nil {
		t.Fatal(err)
	}
	_ = filepath.WalkDir(dst, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && d.Type().IsRegular() {
			data, _ := os.ReadFile(p)
			if strings.Contains(string(data), "OUTSIDE") || strings.Contains(string(data), "EVIL-HOOK") {
				t.Errorf("%s leaked content", p)
			}
		}
		return nil
	})
}

func TestStripOperatorPlugins_KeepsBaseNamesAndReportsOnlyStrippedPlugins(t *testing.T) {
	hostile := hostilePlugin(t)
	clean := filepath.Join(t.TempDir(), "clean")
	writeTree(t, clean, map[string]string{"skills/s/SKILL.md": "---\nname: s\ndescription: d\n---\nx\n"})
	broken := filepath.Join(t.TempDir(), "broken")
	writeTree(t, broken, map[string]string{".claude-plugin/plugin.json": "{not json"})

	root := t.TempDir()
	dirs, copied := stripOperatorPlugins(root, []string{hostile, clean, broken}, 1)
	stripped, skipped := copied.Stripped, copied.Skipped
	if len(dirs) != 2 || filepath.Base(dirs[0]) != "ops" || filepath.Base(dirs[1]) != "clean" {
		t.Fatalf("dirs = %v, want the copies of ops and clean (broken left out, failing closed)", dirs)
	}
	if len(stripped) != 1 || stripped[0] != "ops" {
		t.Errorf("stripped = %v, want [ops]", stripped)
	}
	if len(skipped) != 1 || skipped[0] != "broken" {
		t.Errorf("skipped = %v, want [broken]", skipped)
	}
	for _, d := range dirs {
		if !strings.HasPrefix(d, root) {
			t.Errorf("%s is not under the per-review root", d)
		}
	}
	if _, err := os.Stat(filepath.Join(dirs[1], "skills", "s", "SKILL.md")); err != nil {
		t.Errorf("clean plugin not copied: %v", err)
	}
}

func TestStripFrontmatter(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		ok       bool
		removed  int
		keep     string
	}{
		{"none", "# title\nbody", true, 0, "# title\nbody"},
		{"empty block", "---\n---\nbody", true, 0, "body"},
		{"hooks and mcp", "---\nname: x\nHooks: {}\nmcp_servers: {}\n---\nbody", true, 2, "name: x"},
		{"crlf fence", "---\r\nname: x\r\nhooks: {a: b}\r\n---\r\nbody", true, 1, "name: x"},
		{"unclosed", "---\nname: x\nbody", false, 0, ""},
		{"not a mapping", "---\n- a\n- b\n---\nbody", false, 0, ""},
		{"bom opener", "\ufeff---\nhooks: {}\n---\n", false, 0, ""},
	} {
		out, removed, ok := stripFrontmatter([]byte(tc.in))
		if ok != tc.ok || len(removed) != tc.removed {
			t.Errorf("%s: ok=%v removed=%v, want ok=%v removed=%d", tc.name, ok, removed, tc.ok, tc.removed)
			continue
		}
		if ok && !strings.Contains(string(out), tc.keep) {
			t.Errorf("%s: output %q lacks %q", tc.name, out, tc.keep)
		}
		if ok && tc.removed > 0 && strings.Contains(strings.ToLower(string(out)), "hooks") {
			t.Errorf("%s: hooks survived: %q", tc.name, out)
		}
	}
}
