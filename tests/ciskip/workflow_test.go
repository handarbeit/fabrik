package ciskip

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkflowShape pins the properties of .github/workflows/ci.yml that make
// the skip safe (FR-003, FR-010, SC-006): required-check names unchanged, no
// workflow-level path filter, and every Go step gated on the classifier.
func TestWorkflowShape(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(b)

	for _, name := range []string{"name: Test and vet", "name: Test in a worker-shaped environment"} {
		if !strings.Contains(ci, "    "+name+"\n") {
			t.Errorf("required-check job name %q must not change", name)
		}
	}
	if regexp.MustCompile(`(?m)^\s*paths(-ignore)?:`).MatchString(ci) {
		t.Error("ci.yml must not use a workflow-level paths:/paths-ignore: filter; a filtered workflow leaves required checks pending")
	}
	if got := strings.Count(ci, "id: classify"); got != 2 {
		t.Errorf("both Go jobs need a classify step, found %d", got)
	}
	if got := strings.Count(ci, "fetch-depth: 2"); got != 2 {
		t.Errorf("both Go jobs need fetch-depth: 2 for the merge commit's base parent, found %d", got)
	}

	// Every step other than checkout and classify carries exactly the condition
	// it must have. A Go step gated on a shell-check output (or the reverse), or
	// a new step nobody classified, fails here instead of silently running or
	// skipping the wrong thing.
	const goGate = "if: steps.classify.outputs.skip != 'true'"
	want := map[string]string{
		"Set up Go":                     goGate,
		"gofmt":                         goGate,
		"go vet":                        goGate,
		"Test hygiene lint":             goGate,
		"go test":                       goGate,
		"compile e2e suite (not run)":   goGate,
		"go test (worker-shaped env)":   goGate,
		"killgroup helper test (#1957)": "if: steps.classify.outputs.killgroup != 'false'",
		"cut-release flag/gate regression test (#1454)": "if: steps.classify.outputs.cut_release != 'false'",
		"ADR reference integrity":                       "if: steps.classify.outputs.adr_refs != 'false'",
	}
	seen := map[string]bool{}
	steps := strings.Split(ci, "\n      - name: ")
	for _, st := range steps[1:] {
		name, body, _ := strings.Cut(st, "\n")
		if name == "Checkout" || strings.HasPrefix(name, "Classify the diff") {
			continue
		}
		cond, ok := want[name]
		if !ok {
			t.Errorf("step %q has no expected classifier condition; add it to this table", name)
			continue
		}
		seen[name] = true
		// The step's own `if:` line (indented 8), not a mention in a comment.
		if !regexp.MustCompile(`(?m)^        ` + regexp.QuoteMeta(cond) + `$`).MatchString(body) {
			t.Errorf("step %q must be gated by exactly %q", name, cond)
		}
	}
	// "Set up Go" and "go test" appear in both jobs; the rest once.
	for name := range want {
		if !seen[name] {
			t.Errorf("expected step %q is missing from ci.yml", name)
		}
	}
}
