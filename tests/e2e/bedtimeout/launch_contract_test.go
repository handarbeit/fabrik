package bedtimeout

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The e2e-tagged harness cannot be compiled by plain `go test`, so the half of
// the launch contract that lives there (tests/e2e/lifecycle.go's
// StartFabrikTestBed) is pinned by reading the source; the gate runner's half is
// pinned behaviourally by tests/gate's TestBedStartCmdContracts.
func TestHarnessLaunchInjectsSharedTimeouts(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "lifecycle.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "bedtimeout.Env()...") {
		t.Error("tests/e2e/lifecycle.go must append bedtimeout.Env() to the bed's environment (#2417)")
	}
	if got := strings.Count(string(src), "bedtimeout.Env()"); got != 1 {
		t.Errorf("bedtimeout.Env() must be injected exactly once in StartFabrikTestBed, found %d", got)
	}
}

// Tests read the value the bed was launched with. Parsing the bed's .env (the
// removed readEnvFileReviewWaitTimeout) or falling back to a literal default would
// let a test and its bed disagree.
func TestNoTestReadsTheTimeoutFromBedEnvFile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no harness sources found: %v", err)
	}
	bad := regexp.MustCompile(`readEnvFileValue\([^)]*"FABRIK_(REVIEW|CI)_WAIT_TIMEOUT"|readEnvFileReviewWaitTimeout`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments may mention the removed helper by name.
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if bad.MatchString(line) {
				t.Errorf("%s:%d reads an engine wait timeout from the bed's .env; use bedtimeout (#2417): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
