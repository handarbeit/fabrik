package stages

import (
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/plugin"
)

func qualifyAll(key, ref string) (string, bool) {
	if _, _, q := plugin.Split(ref); q {
		return "", false
	}
	return plugin.Qualify(plugin.BasePluginName, ref), true
}

func TestRewriteSkillRefs_AllFourKeys(t *testing.T) {
	src := "name: Plan\nskill: fabrik-plan\ncomment_skill: fabrik-plan-comment\nci_fix_skill: ci\nrebase_skill: rb\nmodel: sonnet\n"
	out, changes, err := RewriteSkillRefs([]byte(src), qualifyAll)
	if err != nil {
		t.Fatal(err)
	}
	want := "name: Plan\nskill: fabrik-stages:fabrik-plan\ncomment_skill: fabrik-stages:fabrik-plan-comment\nci_fix_skill: fabrik-stages:ci\nrebase_skill: fabrik-stages:rb\nmodel: sonnet\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if len(changes) != 4 || changes[0].Key != "skill" || changes[0].Old != "fabrik-plan" || changes[0].Line != 2 {
		t.Errorf("changes = %+v", changes)
	}
}

func TestRewriteSkillRefs_PreservesQuotesCommentsAndCRLF(t *testing.T) {
	src := "name: X\r\nskill: \"fabrik-plan\"   # the plan skill\r\ncomment_skill: 'fabrik-plan-comment'\r\nmodel:   sonnet\r\n"
	out, _, err := RewriteSkillRefs([]byte(src), qualifyAll)
	if err != nil {
		t.Fatal(err)
	}
	want := "name: X\r\nskill: \"fabrik-stages:fabrik-plan\"   # the plan skill\r\ncomment_skill: 'fabrik-stages:fabrik-plan-comment'\r\nmodel:   sonnet\r\n"
	if string(out) != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestRewriteSkillRefs_NoTrailingNewlineAndTrailingComment(t *testing.T) {
	out, _, err := RewriteSkillRefs([]byte("name: X\nskill: fabrik-plan # c"), qualifyAll)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "name: X\nskill: fabrik-stages:fabrik-plan # c" {
		t.Errorf("got %q", out)
	}
}

func TestRewriteSkillRefs_LeavesQualifiedAndUnresolvedAlone(t *testing.T) {
	src := "name: X\nskill: fabrik-stages:fabrik-plan\ncomment_skill: custom\n"
	out, changes, err := RewriteSkillRefs([]byte(src), func(key, ref string) (string, bool) {
		if ref == "custom" {
			return "", false
		}
		return qualifyAll(key, ref)
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != src || len(changes) != 0 {
		t.Errorf("file changed: %q %+v", out, changes)
	}
}

func TestRewriteSkillRefs_IgnoresSkillTextInsideBlockScalarsAndNesting(t *testing.T) {
	src := "name: X\nprompt: |\n  Use this:\n  skill: not-a-ref\nskill: fabrik-plan\nallowed_tools:\n  - Read\nextra:\n  skill: nested\n"
	out, changes, err := RewriteSkillRefs([]byte(src), qualifyAll)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(src, "\nskill: fabrik-plan\n", "\nskill: fabrik-stages:fabrik-plan\n", 1)
	if string(out) != want || len(changes) != 1 {
		t.Errorf("got:\n%s\nchanges %+v", out, changes)
	}
}

func TestRewriteSkillRefs_RejectsInvalidYAML(t *testing.T) {
	if _, _, err := RewriteSkillRefs([]byte("skill: [unclosed\n"), qualifyAll); err == nil {
		t.Error("expected parse error")
	}
}

func TestRewriteSkillRefs_UsesBasePluginNameConstant(t *testing.T) {
	out, _, err := RewriteSkillRefs([]byte("skill: s\n"), qualifyAll)
	if err != nil {
		t.Fatal(err)
	}
	if want := "skill: " + plugin.BasePluginName + plugin.NamespaceSep + "s\n"; string(out) != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestRewriteSkillRefs_EmptyValueIgnored(t *testing.T) {
	src := "name: X\nskill:\nmodel: a\n"
	out, changes, err := RewriteSkillRefs([]byte(src), qualifyAll)
	if err != nil || string(out) != src || len(changes) != 0 {
		t.Errorf("%q %+v %v", out, changes, err)
	}
}

func TestListSkillRefs(t *testing.T) {
	refs, err := ListSkillRefs([]byte("skill: a\ncomment_skill: p:b\nmodel: m\n"))
	if err != nil || refs["skill"] != "a" || refs["comment_skill"] != "p:b" || len(refs) != 2 {
		t.Errorf("%v %v", refs, err)
	}
}

// In an unquoted scalar '#' only starts a comment after whitespace, so a value
// containing a bare '#' is rewritten whole (not split into value and comment),
// and a real trailing comment is still preserved.
func TestRewriteSkillRefs_HashInsideUnquotedValue(t *testing.T) {
	src := "skill: foo#bar\ncomment_skill: baz#q   # a comment\nci_fix_skill: plain # note\n"
	out, changes, err := RewriteSkillRefs([]byte(src), qualifyAll)
	if err != nil {
		t.Fatal(err)
	}
	want := "skill: fabrik-stages:foo#bar\ncomment_skill: fabrik-stages:baz#q   # a comment\nci_fix_skill: fabrik-stages:plain # note\n"
	if string(out) != want {
		t.Errorf("got %q want %q", out, want)
	}
	if len(changes) != 3 || changes[0].Old != "foo#bar" || changes[1].Old != "baz#q" || changes[2].Old != "plain" {
		t.Errorf("changes = %+v", changes)
	}
}
