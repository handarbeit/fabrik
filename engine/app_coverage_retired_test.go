package engine

import (
	"bytes"
	"testing"

	"github.com/handarbeit/fabrik/warnings"
)

// A hookdeck_coverage entry persisted by an earlier version must not outlive the
// removed check (ADR 2477): nothing re-evaluates it, so startup clears it, and
// leaves every other warning type alone.
func TestClearRetiredHookdeckWarnings_ClearsOnlyThatType(t *testing.T) {
	isolateWarnings(t)
	var buf bytes.Buffer
	if err := warnings.Reconcile(&buf, retiredHookdeckCoverageWarningType, []warnings.Condition{{
		Subject: "repos", Title: "stale", Detail: "GitHub App installation does not cover 1 managed repo(s)", LogLine: "stale",
	}}); err != nil {
		t.Fatalf("seed hookdeck_coverage: %v", err)
	}
	if err := warnings.Reconcile(&buf, "direct_coverage", []warnings.Condition{{
		Subject: "repos", Title: "live", Detail: "d", LogLine: "live",
	}}); err != nil {
		t.Fatalf("seed direct_coverage: %v", err)
	}
	if !warningKeys(t)["hookdeck_coverage:repos"] {
		t.Fatalf("seed failed; keys = %v", warningKeys(t))
	}

	clearRetiredHookdeckWarnings(&buf)

	keys := warningKeys(t)
	if keys["hookdeck_coverage:repos"] {
		t.Errorf("hookdeck_coverage:repos survived the startup sweep")
	}
	if !keys["direct_coverage:repos"] {
		t.Errorf("direct_coverage:repos was removed; the sweep must only touch the retired type (keys = %v)", keys)
	}
}
