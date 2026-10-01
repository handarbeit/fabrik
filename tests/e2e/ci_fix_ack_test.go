//go:build e2e

package e2e

import "testing"

// TestParseMinRunID covers the nonce-resolution parse: a decimal ID, and the
// "no run yet" shapes (empty / jq's null) that must be retryable errors, not 0.
func TestParseMinRunID(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"123456789\n", 123456789, false},
		{"null\n", 0, true},
		{"", 0, true},
		{"abc", 0, true},
	} {
		got, err := parseMinRunID(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseMinRunID(%q) = %d, %v; want %d, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestPromptMissing proves the prompt assertion is not satisfiable by a bare
// retry: both the failing check's name and the exact ack line must be present,
// and a different nonce does not satisfy it.
func TestPromptMissing(t *testing.T) {
	line := `[#7 ci-fix-reinvoke] prompt (900 bytes): "- **ci-fix-sentinel**: failure [NEW REGRESSION]\n  annotation: create CI_FIX_ACK containing the line ack:42"`
	if m := promptMissing(line, 42); len(m) != 0 {
		t.Errorf("complete prompt reported missing %v", m)
	}
	if m := promptMissing(line, 43); len(m) != 1 || m[0] != "ack:43" {
		t.Errorf("wrong nonce: missing = %v, want [ack:43]", m)
	}
	bare := `[#7 ci-fix-reinvoke] prompt (200 bytes): "- **build**: failure [NEW REGRESSION]"`
	if m := promptMissing(bare, 42); len(m) != 2 {
		t.Errorf("a prompt with no failure context reported missing %v, want both", m)
	}
}
