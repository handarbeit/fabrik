package engine

import (
	"fmt"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// describeNonPassing renders non-passing check runs (cancelled, stale, empty or
// unrecognised conclusion) as "name (conclusion)", using the real conclusion so
// a cancellation is never described as a failure (#2296 FR-012). An empty
// conclusion on a completed run reads "no conclusion".
func describeNonPassing(runs []gh.CheckRun) string {
	if len(runs) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(runs))
	for _, cr := range runs {
		parts = append(parts, fmt.Sprintf("%s (%s)", cr.Name, nonPassingConclusion(cr)))
	}
	return strings.Join(parts, ", ")
}

// nonPassingConclusion is the conclusion to show for a non-passing run.
func nonPassingConclusion(cr gh.CheckRun) string {
	if cr.Conclusion == "" {
		return "no conclusion"
	}
	return cr.Conclusion
}

// nonPassingNote is the one-line explanation shown beside a non-passing run in
// a worker-facing context: the check produced no result, which is not a test
// failure, and a fresh run is what is needed.
const nonPassingNote = "no result was produced; this is not a test failure — a fresh run is needed (push, or re-run the check)"
