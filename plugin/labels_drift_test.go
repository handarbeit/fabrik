package plugin

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestLabelsMdCoversStateMachineLabels guards against LABELS.md (and the 12
// per-stage SKILL.md hot-subset sections) silently falling behind
// docs/state-machine.md's §1.4 label table — the canonical, exhaustive
// source LABELS.md is condensed from (ADR-1339, #1339 Requirement 6).
//
// This is a NAME-PRESENCE check, not a semantic-equivalence check: it
// verifies every label identifier in §1.4 is mentioned somewhere in the
// shipped plugin docs, not that its documented behavior still matches.
// A label whose semantics changed in state-machine.md without a name
// change will not be caught here. See ADR-1339 for why a byte-exact
// regenerate-and-diff (the docs-drift.yml pattern) doesn't transfer to a
// ~40:1 curated condensation.
//
// docs/state-machine.md is read from the source tree (it is fabrik source,
// never embedded). LABELS.md and the SKILL.md files are read from
// plugin.FabrikPlugin (the embed), not the source tree, so this test also
// fails if the //go:embed directive for LABELS.md is ever dropped.
func TestLabelsMdCoversStateMachineLabels(t *testing.T) {
	labels, err := extractStateMachineLabels("../docs/state-machine.md")
	if err != nil {
		t.Fatalf("extracting labels from docs/state-machine.md: %v", err)
	}
	if len(labels) < 20 {
		t.Fatalf("expected at least 20 labels extracted from state-machine.md §1.4, got %d: %v", len(labels), labels)
	}

	var corpus strings.Builder
	labelsMd, err := FabrikPlugin.ReadFile("fabrik-workflows/LABELS.md")
	if err != nil {
		t.Fatalf("reading LABELS.md from embed: %v", err)
	}
	corpus.Write(labelsMd)

	skillDirs, err := FabrikPlugin.ReadDir("fabrik-workflows/skills")
	if err != nil {
		t.Fatalf("reading skills dir from embed: %v", err)
	}
	for _, d := range skillDirs {
		if !d.IsDir() {
			continue
		}
		content, err := FabrikPlugin.ReadFile("fabrik-workflows/skills/" + d.Name() + "/SKILL.md")
		if err != nil {
			t.Fatalf("reading %s/SKILL.md from embed: %v", d.Name(), err)
		}
		corpus.Write(content)
	}

	corpusText := corpus.String()
	var missing []string
	for _, label := range labels {
		if !labelMatcher(label).MatchString(corpusText) {
			missing = append(missing, label)
		}
	}
	if len(missing) > 0 {
		t.Errorf("labels present in docs/state-machine.md §1.4 but absent from LABELS.md + all SKILL.md files: %v\n"+
			"Add these to LABELS.md (and, if hot for a stage, that stage's SKILL.md) or confirm they were intentionally renamed/removed.", missing)
	}
}

var (
	labelCellRe        = regexp.MustCompile("^\\|\\s*`([^`]+)`")
	labelPlaceholderRe = regexp.MustCompile(`<[^>]+>`)
)

// labelMatcher builds a regexp that matches a label identifier as it may
// appear in consumer docs, tolerating a different placeholder spelling for
// any <placeholder> segment (state-machine.md uses <user>/<X>, LABELS.md and
// the SKILL.md files use <user>/<name>/<mode> — the segment name itself
// isn't the contract, its position in the label is). A label with no
// placeholder matches as a literal substring.
func labelMatcher(label string) *regexp.Regexp {
	parts := labelPlaceholderRe.Split(label, -1)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = regexp.QuoteMeta(p)
	}
	pattern := strings.Join(quoted, `<?[A-Za-z0-9_.-]+>?`)
	return regexp.MustCompile(pattern)
}

// extractStateMachineLabels parses the §1.4 "Label Semantics Reference" table
// in docs/state-machine.md and returns each row's raw label identifier
// (placeholders like <user> or <X> left intact — labelMatcher handles them).
func extractStateMachineLabels(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	inSection := false
	var labels []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "### 1.4 Label Semantics Reference") {
			inSection = true
			continue
		}
		if !inSection {
			continue
		}
		if strings.TrimSpace(line) == "---" {
			break
		}
		m := labelCellRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		labels = append(labels, m[1])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return labels, nil
}
