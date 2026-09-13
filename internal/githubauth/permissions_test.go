package githubauth

import (
	"fmt"
	"strings"
	"testing"
)

func TestPermissionOrdinal_Ranking(t *testing.T) {
	cases := []struct {
		level string
		want  int
	}{
		{"", 0},
		{"none", 0},
		{"bogus", 0},
		{"read", 1},
		{"write", 2},
		{"admin", 3},
	}
	for _, c := range cases {
		if got := permissionOrdinal(c.level); got != c.want {
			t.Errorf("permissionOrdinal(%q) = %d, want %d", c.level, got, c.want)
		}
	}
	// Cumulative ordering must actually hold, not just each individual value:
	if !(permissionOrdinal("admin") > permissionOrdinal("write") && permissionOrdinal("write") > permissionOrdinal("read") && permissionOrdinal("read") > permissionOrdinal("none")) {
		t.Fatal("permission levels are not strictly ordered none < read < write < admin")
	}
}

func TestCheckGrantedPermissions_MissingPermission(t *testing.T) {
	required := map[string]string{"issues": "write"}
	granted := map[string]string{"contents": "read"}
	shortfalls := checkGrantedPermissions(granted, required)
	if len(shortfalls) != 1 {
		t.Fatalf("expected 1 shortfall, got %d: %+v", len(shortfalls), shortfalls)
	}
	if shortfalls[0].Permission != "issues" || shortfalls[0].Required != "write" || shortfalls[0].Granted != "" {
		t.Errorf("shortfall = %+v", shortfalls[0])
	}
}

func TestCheckGrantedPermissions_InsufficientLevel(t *testing.T) {
	required := map[string]string{"issues": "write"}
	granted := map[string]string{"issues": "read"}
	shortfalls := checkGrantedPermissions(granted, required)
	if len(shortfalls) != 1 {
		t.Fatalf("expected 1 shortfall, got %d: %+v", len(shortfalls), shortfalls)
	}
	if shortfalls[0].Granted != "read" {
		t.Errorf("Granted = %q, want %q", shortfalls[0].Granted, "read")
	}
}

// TestCheckGrantedPermissions_HigherLevelSatisfiesRequirement is the
// ordinal-comparison regression case Research/Plan flagged explicitly: a
// scope granted at a higher level than required (e.g. "admin" granted,
// "write" required) must NOT be reported as a shortfall — a plain
// string-equality comparison would false-positive here.
func TestCheckGrantedPermissions_HigherLevelSatisfiesRequirement(t *testing.T) {
	required := map[string]string{"issues": "write"}
	granted := map[string]string{"issues": "admin"}
	shortfalls := checkGrantedPermissions(granted, required)
	if len(shortfalls) != 0 {
		t.Fatalf("expected no shortfalls when granted (admin) exceeds required (write), got %+v", shortfalls)
	}
}

func TestCheckGrantedPermissions_ExactMatchIsNotAShortfall(t *testing.T) {
	required := map[string]string{"issues": "write", "contents": "read"}
	granted := map[string]string{"issues": "write", "contents": "read"}
	if shortfalls := checkGrantedPermissions(granted, required); len(shortfalls) != 0 {
		t.Fatalf("expected no shortfalls for an exact match, got %+v", shortfalls)
	}
}

// TestCheckGrantedPermissions_DeterministicOrder guards the sort: multiple
// shortfalls must come back in a stable, alphabetical-by-permission order so
// log output (and any test asserting on it) doesn't flap between runs from Go's
// unordered map iteration.
func TestCheckGrantedPermissions_DeterministicOrder(t *testing.T) {
	required := map[string]string{
		"pull_requests": "write",
		"issues":        "write",
		"contents":      "write",
	}
	granted := map[string]string{}
	for i := 0; i < 20; i++ {
		shortfalls := checkGrantedPermissions(granted, required)
		if len(shortfalls) != 3 {
			t.Fatalf("expected 3 shortfalls, got %d", len(shortfalls))
		}
		if shortfalls[0].Permission != "contents" || shortfalls[1].Permission != "issues" || shortfalls[2].Permission != "pull_requests" {
			t.Fatalf("shortfalls not in alphabetical order: %+v", shortfalls)
		}
	}
}

func TestLogPermissionShortfalls_NamesEverything(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	shortfalls := []RequiredPermissionShortfall{
		{Permission: "issues", Required: "write", Granted: "read"},
		{Permission: "contents", Required: "read", Granted: ""},
	}
	logPermissionShortfalls(42, "handarbeit", shortfalls, logf)
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %v", len(lines), lines)
	}
	if !containsAll(t, lines[0], "42", "handarbeit", "issues", "read", "write") {
		t.Errorf("line 0 missing expected content: %q", lines[0])
	}
	if !containsAll(t, lines[1], "42", "handarbeit", "contents", "none", "read") {
		t.Errorf("line 1 missing expected content (granted \"none\" for an absent permission): %q", lines[1])
	}
}

func containsAll(t *testing.T, s string, substrs ...string) bool {
	t.Helper()
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
