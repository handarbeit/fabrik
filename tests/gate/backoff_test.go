package gate

import (
	"os"
	"strings"
	"testing"
)

// Ported from scripts/e2e/backoff_detection_test.sh (#1527/#1547).

func TestDetectRateLimitBackoff(t *testing.T) {
	log := t.TempDir() + "/fabrik.log"
	check := func(desc string, want bool) {
		t.Helper()
		if got := DetectRateLimitBackoff(log); got != want {
			t.Errorf("%s: got %v, want %v", desc, got, want)
		}
	}

	mustWrite(t, log, "2026-08-11T09:00:00Z [info] poll cycle complete\n2026-08-11T09:00:30Z [info] poll cycle complete\n")
	check("no backoff activity", false)

	mustWrite(t, log, "2026-08-11T09:00:00Z [warn] GraphQL rate limit is low (25% remaining) — consider reducing poll frequency\n")
	check("the per-poll 'is low' line alone is not the activation line", false)

	mustWrite(t, log, "2026-08-11T09:00:00Z [warn] GraphQL rate limit low (19% remaining) — activating rate-limit backoff\n")
	check("activation line present", true)

	// The #1547 field regression: the engine truncates fabrik.log at every
	// startup, so by the time a leg ends the file is SHORTER than it was when
	// the leg began. The activation line is logged only after that truncation.
	mustWrite(t, log, strings.Repeat("2026-08-11T08:00:00Z [info] poll cycle complete, nothing to do\n", 50))
	preRestartSize := int64(len(strings.Repeat("2026-08-11T08:00:00Z [info] poll cycle complete, nothing to do\n", 50)))
	mustWrite(t, log, "2026-08-11T09:15:16Z [warn] GraphQL rate limit low (19% remaining) — activating rate-limit backoff\n")
	check("activation line logged only after a mid-leg truncation (the field regression)", true)

	// Neutralisation twin: the historical byte-offset approach (`tail -c
	// +OFFSET`, with the offset taken before the restart) must NOT detect the
	// same fixture — proving the case above exercises the real regression and
	// that the whole-file scan is what makes it pass.
	data, _ := os.ReadFile(log)
	var legacy bool
	if preRestartSize < int64(len(data)) {
		legacy = strings.Contains(string(data[preRestartSize:]), backoffMarker)
	}
	if legacy {
		t.Error("the byte-offset approach unexpectedly detected the fixture; the field-regression case may not exercise the regression it claims to")
	}

	os.Remove(log)
	check("a missing log is 'no match', not an error", false)
}

func TestReaderContainsAcrossChunkBoundaries(t *testing.T) {
	// The marker straddles the 64 KiB read boundary.
	pad := strings.Repeat("x", 64*1024-5)
	if !readerContains(strings.NewReader(pad+backoffMarker+"\n"), []byte(backoffMarker)) {
		t.Error("a match straddling a chunk boundary was missed")
	}
	if readerContains(strings.NewReader(pad+"activating rate-limit"), []byte(backoffMarker)) {
		t.Error("a partial marker must not match")
	}
}
