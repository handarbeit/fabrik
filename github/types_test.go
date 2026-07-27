package github

import "testing"

// TestIsBotLogin covers the existing suffix/prefix/literal patterns plus the
// #1141 addition of the bare "coderabbitai" literal — CodeRabbit's API login
// carries a "[bot]" suffix (already matched), but its @-mention surface is
// the bare form, which the suffix rule does not catch on its own.
func TestIsBotLogin(t *testing.T) {
	tests := []struct {
		name  string
		login string
		want  bool
	}{
		{"bot suffix bracket form", "coderabbitai[bot]", true},
		{"bare coderabbitai (mention surface)", "coderabbitai", true},
		{"bare coderabbitai mixed case", "CodeRabbitAI", true},
		{"dash-bot suffix", "some-data-bot", true},
		{"copilot prefix", "copilot-pull-request-reviewer", true},
		{"dependabot literal", "dependabot", true},
		{"gemini-code-assist literal", "gemini-code-assist", true},
		{"generic bot bracket suffix", "renovate[bot]", true},
		{"human login", "arbeithand", false},
		{"human login resembling a bot vendor", "coderabbit-fan", false},
		{"empty login", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBotLogin(tt.login); got != tt.want {
				t.Errorf("IsBotLogin(%q) = %v, want %v", tt.login, got, tt.want)
			}
		})
	}
}
