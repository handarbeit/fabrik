package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

// extractEmbeddedPlugin writes the embedded base plugin to a temp directory,
// as `fabrik init` would, and returns it.
func extractEmbeddedPlugin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugin")
	err := fs.WalkDir(plugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		dest := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := plugin.FabrikPlugin.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// loadDefaultStages loads the embedded default stage YAMLs (what a fresh
// `fabrik init` writes).
func loadDefaultStages(t *testing.T) []*stages.Stage {
	t.Helper()
	dir := t.TempDir()
	entries, err := fs.ReadDir(stages.DefaultStages, "examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := stages.DefaultStages.ReadFile("examples/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list, err := stages.LoadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func skillDirective(name string) string {
	return "Follow the instructions in the " + name + " skill exactly."
}

// SC-001: for every default stage the emitted prompt names the exact registered
// skill (<manifest name>:<skill directory>), as Claude Code lists it.
func TestSkillPrompt_DefaultStagesNameRegisteredSkill(t *testing.T) {
	pdir := extractEmbeddedPlugin(t)
	e := &Engine{skillResolver: newSkillResolver([]string{pdir})}
	item := gh.ProjectItem{Number: 1, Title: "t"}

	checked := 0
	for _, s := range loadDefaultStages(t) {
		if s.Skill == "" {
			continue
		}
		dir := strings.ToLower(s.Name)
		if _, err := os.Stat(filepath.Join(pdir, "skills", dir, "SKILL.md")); err != nil {
			t.Fatalf("stage %s: no embedded skill dir %s", s.Name, dir)
		}
		registered := plugin.BasePluginName + plugin.NamespaceSep + dir
		// The shipped defaults are written qualified, so a fresh `fabrik init`
		// starts without a deprecation notice.
		if s.Skill != registered || (s.CommentSkill != "" && s.CommentSkill != registered+"-comment") {
			t.Errorf("stage %s: shipped default skill=%q comment_skill=%q, want %q / %q", s.Name, s.Skill, s.CommentSkill, registered, registered+"-comment")
		}
		got, _, err := e.resolveStageSkills(s)
		if err != nil {
			t.Fatalf("stage %s: %v", s.Name, err)
		}
		if p := buildPrompt(got, item, nil, "main", ""); !strings.Contains(p, skillDirective(registered)) {
			t.Errorf("stage %s: prompt lacks %q", s.Name, skillDirective(registered))
		}
		if s.CommentSkill != "" {
			if p := buildCommentReviewPrompt(got, item, nil, "main"); !strings.Contains(p, skillDirective(registered+"-comment")) {
				t.Errorf("stage %s: comment prompt lacks %q", s.Name, skillDirective(registered+"-comment"))
			}
		}
		checked++
	}
	if checked < 6 {
		t.Fatalf("checked %d default stages with skills, want at least 6", checked)
	}
}

// SC-003: a configuration from the previous release (bare names) against the
// current embedded plugin — and against an old customised plugin whose layout
// only matches by directory name — resolves every stage to the same prompt as
// the qualified defaults.
func TestSkillPrompt_PreviousReleaseBareConfigRunsUnchanged(t *testing.T) {
	item := gh.ProjectItem{Number: 1, Title: "t"}

	// "Old customised plugin": only the directory names and a manifest, with
	// skill bodies rewritten, as a local edit would leave them.
	custom := extractEmbeddedPlugin(t)
	matches, err := filepath.Glob(filepath.Join(custom, "skills", "*", "SKILL.md"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("glob: %v %v", matches, err)
	}
	for _, m := range matches {
		if err := os.WriteFile(m, []byte("locally customised\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for name, pdir := range map[string]string{"embedded": extractEmbeddedPlugin(t), "customised": custom} {
		t.Run(name, func(t *testing.T) {
			e := &Engine{skillResolver: newSkillResolver([]string{pdir})}
			for _, s := range loadDefaultStages(t) {
				if s.Skill == "" {
					continue
				}
				_, bareSkill, _ := plugin.Split(s.Skill)
				_, bareComment, _ := plugin.Split(s.CommentSkill)
				bare := *s
				bare.Skill, bare.CommentSkill = bareSkill, bareComment

				fromBare, refs, err := e.resolveStageSkills(&bare)
				if err != nil {
					t.Fatalf("stage %s: %v", s.Name, err)
				}
				fromQualified, _, err := e.resolveStageSkills(s)
				if err != nil {
					t.Fatalf("stage %s: %v", s.Name, err)
				}
				if buildPrompt(fromBare, item, nil, "main", "") != buildPrompt(fromQualified, item, nil, "main", "") {
					t.Errorf("stage %s: bare config produced a different prompt", s.Name)
				}
				if buildCommentReviewPrompt(fromBare, item, nil, "main") != buildCommentReviewPrompt(fromQualified, item, nil, "main") {
					t.Errorf("stage %s: bare config produced a different comment prompt", s.Name)
				}
				for _, r := range refs {
					if !r.Deprecated {
						t.Errorf("stage %s: bare %s not flagged deprecated", s.Name, r.Key)
					}
				}
			}
		})
	}
}

// SC-002: a genuinely separate plugin (not a renamed copy of Fabrik's) loaded
// as an additional plugin. A qualified reference loads its skill, and a bare
// name only that plugin provides is qualified to it — which the pre-resolution
// engine, passing the bare name through, did not do.
func TestSkillPrompt_SeparatePlugin(t *testing.T) {
	root := t.TempDir()
	base := skillPlugin(t, root, "base", plugin.BasePluginName, "fabrik-review")
	org := skillPlugin(t, root, "org", "myorg", "review")
	e := &Engine{skillResolver: newSkillResolver([]string{base, org})}
	item := gh.ProjectItem{Number: 1, Title: "t"}

	for _, ref := range []string{"myorg:review", "review"} {
		got, _, err := e.resolveStageSkills(&stages.Stage{Name: "Review", Skill: ref})
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if p := buildPrompt(got, item, nil, "main", ""); !strings.Contains(p, skillDirective("myorg:review")) {
			t.Errorf("ref %q: prompt lacks myorg:review directive:\n%s", ref, p)
		}
	}
}
