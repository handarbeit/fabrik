package wirescrub

import (
	"strings"
	"testing"
)

// TestFindings_SeededSecretsDetected is #1453 AC6's demonstration: a
// deliberately seeded fixture containing a token-shaped, email-shaped, and
// otherwise secret-shaped string must be caught by Findings. Each case is
// independent so a regression in one pattern doesn't hide behind another.
func TestFindings_SeededSecretsDetected(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    string // substring expected in the Findings description
	}{
		{
			name:    "github classic PAT",
			fixture: `{"token":"ghp_abcdefghijklmnopqrstuvwxyz0123456789AB"}`,
			want:    "github classic token",
		},
		{
			name:    "github oauth token",
			fixture: `{"token":"gho_abcdefghijklmnopqrstuvwxyz0123456789AB"}`,
			want:    "github classic token",
		},
		{
			name:    "github fine-grained PAT",
			fixture: `{"token":"github_pat_11ABCDEFGHIJKLMNOPQRSTU_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop"}`,
			want:    "github fine-grained PAT",
		},
		{
			name:    "generic bearer token",
			fixture: `Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789`,
			want:    "bearer token",
		},
		{
			name:    "email address",
			fixture: `{"author":{"login":"jdoe","email":"jane.doe@handarbeit.io"}}`,
			want:    "email address",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := Findings([]byte(tc.fixture))
			if len(findings) == 0 {
				t.Fatalf("Findings found nothing in seeded fixture %q, expected a %s match", tc.fixture, tc.want)
			}
			joined := strings.Join(findings, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("Findings %v did not mention expected pattern %q", findings, tc.want)
			}
		})
	}
}

// TestFindings_MaskedPreviewNeverContainsFullSecret guards the "never the
// full matched text" invariant Findings documents: if this regresses, a
// Findings call from a failing test could itself leak the secret it
// detected into a CI log.
func TestFindings_MaskedPreviewNeverContainsFullSecret(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789AB"
	findings := Findings([]byte(`{"token":"` + secret + `"}`))
	if len(findings) == 0 {
		t.Fatal("expected a finding for the seeded token")
	}
	for _, f := range findings {
		if strings.Contains(f, secret) {
			t.Fatalf("finding %q contains the full secret verbatim", f)
		}
	}
}

// TestFindings_CleanTextProducesNoFindings ensures Findings doesn't
// over-match on ordinary fixture content — a scrubbing check that fires on
// everything is as useless as one that fires on nothing.
func TestFindings_CleanTextProducesNoFindings(t *testing.T) {
	clean := `{"data":{"repository":{"pullRequest":{"id":"PR_kwDOABC123","number":42,"title":"Fix the thing"}}}}`
	if findings := Findings([]byte(clean)); len(findings) != 0 {
		t.Fatalf("expected no findings in clean fixture, got %v", findings)
	}
}

// TestRedact_RemovesSecretsButKeepsShape proves Redact actually strips the
// sensitive value while leaving a placeholder that still names what was
// removed, and that redacted output no longer trips Findings itself —
// otherwise a "scrubbed" fixture could still fail the committed-fixture
// scrub check in github/.
func TestRedact_RemovesSecretsButKeepsShape(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789AB"
	email := "jane.doe@handarbeit.io"
	fixture := `{"token":"` + secret + `","email":"` + email + `"}`

	redacted := Redact([]byte(fixture))

	if strings.Contains(string(redacted), secret) {
		t.Fatalf("redacted output still contains the raw token: %s", redacted)
	}
	if strings.Contains(string(redacted), email) {
		t.Fatalf("redacted output still contains the raw email: %s", redacted)
	}
	if !strings.Contains(string(redacted), "[SCRUBBED:") {
		t.Fatalf("redacted output has no placeholder marker: %s", redacted)
	}
	if findings := Findings(redacted); len(findings) != 0 {
		t.Fatalf("redacted output still trips Findings: %v", findings)
	}
}
