package pruefer

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func i64ptr(n int64) *int64 { return &n }
func sptr(s string) *string { return &s }

// --- applyRepoNarrowing: excluded_paths/excluded_labels/excluded_authors (union, never removes) ---

func TestApplyRepoNarrowing_ExcludedListsUnion(t *testing.T) {
	operator := Config{
		ExcludedPaths:   []string{"vendor/**"},
		ExcludedLabels:  []string{"skip-review"},
		ExcludedAuthors: []string{"dependabot[bot]"},
	}
	repo := yamlRepoConfig{
		ExcludedPaths:   []string{"generated/**", "vendor/**"}, // dupe should not double-add
		ExcludedLabels:  []string{"wip"},
		ExcludedAuthors: []string{"renovate[bot]"},
	}

	effective, warnings := applyRepoNarrowing(operator, repo)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	wantPaths := []string{"vendor/**", "generated/**"}
	if !equalStrings(effective.ExcludedPaths, wantPaths) {
		t.Errorf("ExcludedPaths = %v, want %v", effective.ExcludedPaths, wantPaths)
	}
	wantLabels := []string{"skip-review", "wip"}
	if !equalStrings(effective.ExcludedLabels, wantLabels) {
		t.Errorf("ExcludedLabels = %v, want %v", effective.ExcludedLabels, wantLabels)
	}
	wantAuthors := []string{"dependabot[bot]", "renovate[bot]"}
	if !equalStrings(effective.ExcludedAuthors, wantAuthors) {
		t.Errorf("ExcludedAuthors = %v, want %v", effective.ExcludedAuthors, wantAuthors)
	}
}

func TestApplyRepoNarrowing_NoRepoConfig_ByteIdenticalToOperator(t *testing.T) {
	operator := Config{
		ExcludedPaths:           []string{"vendor/**"},
		ExcludedLabels:          []string{"skip-review"},
		ExcludedAuthors:         []string{"dependabot[bot]"},
		MaxDiffBytes:            100_000,
		RequestChangesThreshold: SeverityHigh,
	}
	effective, warnings := applyRepoNarrowing(operator, yamlRepoConfig{})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for an absent repo config", warnings)
	}
	if !reflect.DeepEqual(effective, operator) {
		t.Errorf("effective = %+v, want byte-identical to operator %+v", effective, operator)
	}
}

// --- applyRepoNarrowing: max_diff_bytes ---

func TestApplyRepoNarrowing_MaxDiffBytes(t *testing.T) {
	tests := []struct {
		name          string
		operatorVal   int64
		repoVal       *int64
		wantEffective int64
		wantWidenWarn bool
	}{
		{"repo lowers operator's cap", 100_000, i64ptr(50_000), 50_000, false},
		{"repo requests equal value (no-op, allowed)", 100_000, i64ptr(100_000), 100_000, false},
		{"repo raises operator's cap (widen, rejected)", 100_000, i64ptr(200_000), 100_000, true},
		{"repo uncaps while operator has a cap (widen, rejected)", 100_000, i64ptr(0), 100_000, true},
		{"operator uncapped, repo sets a real cap (narrows from infinity)", 0, i64ptr(50_000), 50_000, false},
		{"operator uncapped, repo also uncapped (no-op)", 0, i64ptr(0), 0, false},
		{"repo doesn't set the field", 100_000, nil, 100_000, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			operator := Config{MaxDiffBytes: tc.operatorVal}
			repo := yamlRepoConfig{MaxDiffBytes: tc.repoVal}
			effective, warnings := applyRepoNarrowing(operator, repo)
			if effective.MaxDiffBytes != tc.wantEffective {
				t.Errorf("MaxDiffBytes = %d, want %d", effective.MaxDiffBytes, tc.wantEffective)
			}
			gotWarn := hasWarningForField(warnings, "max_diff_bytes")
			if gotWarn != tc.wantWidenWarn {
				t.Errorf("warning for max_diff_bytes = %v, want %v (warnings: %v)", gotWarn, tc.wantWidenWarn, warnings)
			}
		})
	}
}

// --- applyRepoNarrowing: request_changes_threshold — the highest-stakes direction ---

func TestApplyRepoNarrowing_RequestChangesThreshold(t *testing.T) {
	tests := []struct {
		name          string
		operatorVal   Severity
		repoVal       *string
		wantEffective Severity
		wantRejected  bool
	}{
		{"repo narrows high->low (stricter, allowed)", SeverityHigh, sptr("low"), SeverityLow, false},
		{"repo sets same tier (no-op, allowed)", SeverityHigh, sptr("high"), SeverityHigh, false},
		{"repo widens low->high (rejected)", SeverityLow, sptr("high"), SeverityLow, true},
		{"repo widens high->critical (rejected)", SeverityHigh, sptr("critical"), SeverityHigh, true},
		{"repo turns on when operator was off (allowed)", "", sptr("high"), SeverityHigh, false},
		{"repo unsets an operator-configured threshold back to off (rejected)", SeverityHigh, sptr(""), SeverityHigh, true},
		{"operator off, repo also off (no-op, allowed)", "", sptr(""), "", false},
		{"repo sets an invalid tier (rejected, ignored)", SeverityHigh, sptr("nonsense"), SeverityHigh, true},
		{"repo doesn't set the field", SeverityHigh, nil, SeverityHigh, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			operator := Config{RequestChangesThreshold: tc.operatorVal}
			repo := yamlRepoConfig{RequestChangesThreshold: tc.repoVal}
			effective, warnings := applyRepoNarrowing(operator, repo)
			if effective.RequestChangesThreshold != tc.wantEffective {
				t.Errorf("RequestChangesThreshold = %q, want %q", effective.RequestChangesThreshold, tc.wantEffective)
			}
			gotWarn := hasWarningForField(warnings, "request_changes_threshold")
			if gotWarn != tc.wantRejected {
				t.Errorf("warning for request_changes_threshold = %v, want %v (warnings: %v)", gotWarn, tc.wantRejected, warnings)
			}
		})
	}
}

// --- applyRepoNarrowing: operator-scoped fields are never touched ---

func TestApplyRepoNarrowing_NeverTouchesOperatorScopedFields(t *testing.T) {
	operator := Config{
		Model:           "sonnet",
		Effort:          "high",
		ConcurrencyCap:  3,
		WatchedRepos:    []string{"owner/repo"},
		MaxDerivedRepos: 200,
		AppID:           12345,
	}
	// yamlRepoConfig has no fields corresponding to any of these — this test
	// documents (and would fail to compile if that ever changed without
	// deliberate intent) that there is no way to even construct a
	// yamlRepoConfig that could affect them.
	effective, _ := applyRepoNarrowing(operator, yamlRepoConfig{
		ExcludedPaths: []string{"docs/**"},
	})
	if effective.Model != operator.Model || effective.Effort != operator.Effort ||
		effective.ConcurrencyCap != operator.ConcurrencyCap || effective.AppID != operator.AppID ||
		effective.MaxDerivedRepos != operator.MaxDerivedRepos {
		t.Errorf("operator-scoped fields changed: got %+v, want unchanged from %+v", effective, operator)
	}
	if !equalStrings(effective.WatchedRepos, operator.WatchedRepos) {
		t.Errorf("WatchedRepos changed: got %v, want %v", effective.WatchedRepos, operator.WatchedRepos)
	}
}

// --- fetchRepoConfig ---

func newFakeContentsClient(data []byte, err error) *fakeReviewer {
	r := newFakeReviewer()
	r.repoConfigData = data
	r.repoConfigErr = err
	return r
}

func TestFetchRepoConfig_NotFound(t *testing.T) {
	client := newFakeContentsClient(nil, nil) // default: gh.ErrNotFound
	rc, prov := fetchRepoConfig(client, "owner", "repo", "main")
	if prov.Found {
		t.Errorf("prov.Found = true, want false")
	}
	if prov.Invalid {
		t.Errorf("prov.Invalid = true, want false for an absent file")
	}
	if !reflect.DeepEqual(rc, yamlRepoConfig{}) {
		t.Errorf("rc = %+v, want zero value", rc)
	}
}

func TestFetchRepoConfig_FetchError(t *testing.T) {
	client := newFakeContentsClient(nil, errors.New("network exploded"))
	rc, prov := fetchRepoConfig(client, "owner", "repo", "main")
	if !prov.Invalid {
		t.Errorf("prov.Invalid = false, want true for a genuine fetch error")
	}
	if !reflect.DeepEqual(rc, yamlRepoConfig{}) {
		t.Errorf("rc = %+v, want zero value", rc)
	}
}

func TestFetchRepoConfig_Oversized(t *testing.T) {
	huge := []byte(strings.Repeat("a", DefaultMaxRepoConfigBytes+1))
	client := newFakeContentsClient(huge, nil)
	rc, prov := fetchRepoConfig(client, "owner", "repo", "main")
	if !prov.Invalid {
		t.Errorf("prov.Invalid = false, want true for an oversized file")
	}
	if !reflect.DeepEqual(rc, yamlRepoConfig{}) {
		t.Errorf("rc = %+v, want zero value", rc)
	}
}

func TestFetchRepoConfig_NonUTF8(t *testing.T) {
	invalid := []byte{0xff, 0xfe, 0xfd}
	client := newFakeContentsClient(invalid, nil)
	rc, prov := fetchRepoConfig(client, "owner", "repo", "main")
	if !prov.Invalid {
		t.Errorf("prov.Invalid = false, want true for non-UTF-8 content")
	}
	if !reflect.DeepEqual(rc, yamlRepoConfig{}) {
		t.Errorf("rc = %+v, want zero value", rc)
	}
}

func TestFetchRepoConfig_MalformedYAML(t *testing.T) {
	malformed := []byte("excluded_paths: [unterminated\n  - foo")
	client := newFakeContentsClient(malformed, nil)
	rc, prov := fetchRepoConfig(client, "owner", "repo", "main")
	if !prov.Invalid {
		t.Errorf("prov.Invalid = false, want true for malformed YAML")
	}
	if !reflect.DeepEqual(rc, yamlRepoConfig{}) {
		t.Errorf("rc = %+v, want zero value", rc)
	}
}

func TestFetchRepoConfig_RecognizedFieldsParsed(t *testing.T) {
	data := []byte("excluded_paths:\n  - vendor/**\nmax_diff_bytes: 1000\n")
	client := newFakeContentsClient(data, nil)
	rc, prov := fetchRepoConfig(client, "owner", "repo", "abc123")
	if !prov.Found || prov.Invalid {
		t.Fatalf("prov = %+v, want Found=true Invalid=false", prov)
	}
	if !equalStrings(rc.ExcludedPaths, []string{"vendor/**"}) {
		t.Errorf("ExcludedPaths = %v, want [vendor/**]", rc.ExcludedPaths)
	}
	if rc.MaxDiffBytes == nil || *rc.MaxDiffBytes != 1000 {
		t.Errorf("MaxDiffBytes = %v, want 1000", rc.MaxDiffBytes)
	}
	if prov.Ref != "abc123" {
		t.Errorf("prov.Ref = %q, want abc123", prov.Ref)
	}
}

func TestFetchRepoConfig_UnknownAndOperatorScopedKeysReported(t *testing.T) {
	data := []byte(`
excluded_paths:
  - vendor/**
model: opus
watched_repos:
  - evil/repo
max_derived_repos: 999999
totally_unknown_key: true
`)
	client := newFakeContentsClient(data, nil)
	rc, prov := fetchRepoConfig(client, "owner", "repo", "main")
	if !prov.Found || prov.Invalid {
		t.Fatalf("prov = %+v, want Found=true Invalid=false", prov)
	}
	// The recognized field still parses correctly despite the junk siblings.
	if !equalStrings(rc.ExcludedPaths, []string{"vendor/**"}) {
		t.Errorf("ExcludedPaths = %v, want [vendor/**]", rc.ExcludedPaths)
	}
	wantUnknown := []string{"max_derived_repos", "model", "totally_unknown_key", "watched_repos"}
	if !equalStrings(prov.UnknownKeys, wantUnknown) {
		t.Errorf("UnknownKeys = %v, want %v", prov.UnknownKeys, wantUnknown)
	}
}

// --- narrowingRank direction, isolated from severityRank ---

func TestNarrowingRank_OffIsMostLenient(t *testing.T) {
	if got := narrowingRank(""); got <= narrowingRank(SeverityCritical) {
		t.Errorf("narrowingRank(\"\") = %d, want strictly greater than narrowingRank(critical) = %d", got, narrowingRank(SeverityCritical))
	}
	// Confirm this is deliberately the opposite of severityRank's own
	// direction for "" (0, the least severe) — the whole reason this issue
	// introduces a separate function rather than reusing severityRank.
	if severityRank("") >= severityRank(SeverityLow) {
		t.Fatalf("severityRank(\"\") = %d, unexpectedly >= severityRank(low) = %d — test assumption broken", severityRank(""), severityRank(SeverityLow))
	}
}

// --- helpers ---

func hasWarningForField(warnings []narrowingWarning, field string) bool {
	for _, w := range warnings {
		if w.Field == field {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
