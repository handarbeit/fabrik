package stages

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillKeys are the stage keys whose value is a skill reference.
var SkillKeys = []string{"skill", "comment_skill", "ci_fix_skill", "rebase_skill"}

// SkillRefChange records one rewritten skill reference.
type SkillRefChange struct {
	Key  string
	Old  string
	New  string
	Line int // 1-based
}

// skillRefLine matches a column-0 skill-valued key with a scalar value:
//
//	1 key, 2 spacing after the colon, 3 opening quote, 4 value, 5 closing quote,
//	6 trailing (spaces and an optional comment), 7 an optional CR.
//
// In an unquoted value a '#' only starts a comment after whitespace (YAML), so
// `skill: foo#bar` is the scalar "foo#bar" and `skill: foo #bar` is "foo" plus
// a comment; the lazy value group stops at the first whitespace-then-# (or the
// end of the line), never at a bare '#'.
//
// Only column-0 keys can match: a block scalar's content (a `prompt: |`) is
// always indented, so text inside it that looks like `skill: x` never does.
var skillRefLine = regexp.MustCompile(`^(skill|comment_skill|ci_fix_skill|rebase_skill):([ \t]+)(?:(")([^"\r\n]*)(")|(')([^'\r\n]*)(')|([^\s#"'\[\]{}&*!|>%@` + "`" + `][^\r\n]*?))([ \t]+#[^\r\n]*|[ \t]*)(\r?)$`)

// RewriteSkillRefs rewrites the skill-valued top-level keys of a stage file
// and changes nothing else (#2139, FR-001): it edits matching lines in place,
// so comments, quoting, indentation and line endings elsewhere are preserved
// byte for byte, which a yaml.Node round trip (see applyMissingKeys) is not.
//
// resolve receives each key and its current reference; it returns the
// replacement and true to rewrite it, or false to leave it as is. A returned
// value equal to the old one is also left alone.
//
// Before returning, the original and the result are both decoded and compared:
// the only difference allowed is the rewritten values. Any other difference
// (a construct the line matcher misjudged) returns an error and no output, so a
// stage file is never left corrupted.
func RewriteSkillRefs(src []byte, resolve func(key, ref string) (string, bool)) ([]byte, []SkillRefChange, error) {
	var orig map[string]any
	if err := yaml.Unmarshal(src, &orig); err != nil {
		return nil, nil, fmt.Errorf("parsing stage file: %w", err)
	}

	lines := strings.SplitAfter(string(src), "\n")
	var changes []SkillRefChange
	for i, line := range lines {
		m := skillRefLine.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
		if m == nil {
			continue
		}
		key := m[1]
		var ref, quote string
		switch {
		case m[3] != "":
			ref, quote = m[4], `"`
		case m[6] != "":
			ref, quote = m[7], `'`
		default:
			ref = strings.TrimRight(m[9], " \t")
		}
		if ref == "" {
			continue
		}
		newRef, ok := resolve(key, ref)
		if !ok || newRef == ref {
			continue
		}
		nl := ""
		if strings.HasSuffix(line, "\n") {
			nl = "\n"
		}
		lines[i] = key + ":" + m[2] + quote + newRef + quote + m[10] + m[11] + nl
		changes = append(changes, SkillRefChange{Key: key, Old: ref, New: newRef, Line: i + 1})
	}
	if len(changes) == 0 {
		return src, nil, nil
	}

	out := []byte(strings.Join(lines, ""))
	var got map[string]any
	if err := yaml.Unmarshal(out, &got); err != nil {
		return nil, nil, fmt.Errorf("rewritten stage file does not parse: %w", err)
	}
	want := make(map[string]any, len(orig))
	for k, v := range orig {
		want[k] = v
	}
	for _, c := range changes {
		if cur, _ := want[c.Key].(string); cur != c.Old {
			return nil, nil, fmt.Errorf("line %d: %s was matched as %q but parses as %v", c.Line, c.Key, c.Old, want[c.Key])
		}
		want[c.Key] = c.New
	}
	if !reflect.DeepEqual(want, got) {
		return nil, nil, fmt.Errorf("skill reference rewrite would change more than the skill values; leaving the file untouched")
	}
	return out, changes, nil
}

// ListSkillRefs returns the (key, reference) pairs of a stage file's
// skill-valued top-level keys, in key order of SkillKeys, using the decoded
// document rather than the line matcher.
func ListSkillRefs(src []byte) (map[string]string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parsing stage file: %w", err)
	}
	refs := map[string]string{}
	for _, k := range SkillKeys {
		if s, ok := doc[k].(string); ok && s != "" {
			refs[k] = s
		}
	}
	return refs, nil
}
