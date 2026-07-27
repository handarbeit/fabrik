package engine

import (
	"strings"
	"testing"
)

// TestNeutralizeBotMentions is the regression guard for #1141: Fabrik's own
// generated output re-mentioning a bot login (e.g. "@coderabbitai") notified
// the bot regardless of the surrounding prose saying "no reply posted",
// producing a self-sustaining reply loop (#933).
func TestNeutralizeBotMentions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "bare bot mention gets wrapped",
			input: "**@coderabbitai**: No action taken, no reply posted.",
			want:  "**`@coderabbitai`**: No action taken, no reply posted.",
		},
		{
			name:  "already backtick-wrapped mention is left alone (idempotent)",
			input: "See `@coderabbitai` for details.",
			want:  "See `@coderabbitai` for details.",
		},
		{
			name:  "double-pass is idempotent",
			input: "**@coderabbitai**: No action taken.",
			want:  "**`@coderabbitai`**: No action taken.",
		},
		{
			name:  "human mention is untouched",
			input: "Thanks @arbeithand for the review.",
			want:  "Thanks @arbeithand for the review.",
		},
		{
			name:  "email-like text is untouched",
			input: "Notifications go to support@dependabot.io for this vendor.",
			want:  "Notifications go to support@dependabot.io for this vendor.",
		},
		{
			name:  "multiple bot mentions in one string",
			input: "@coderabbitai acknowledged; cc @dependabot for the bump.",
			want:  "`@coderabbitai` acknowledged; cc `@dependabot` for the bump.",
		},
		{
			name:  "gemini-code-assist bare mention",
			input: "@gemini-code-assist please re-review.",
			want:  "`@gemini-code-assist` please re-review.",
		},
		{
			name:  "mention at start of string",
			input: "@coderabbitai, acknowledged. No action taken.",
			want:  "`@coderabbitai`, acknowledged. No action taken.",
		},
		{
			name:  "no mentions at all",
			input: "Nothing to see here.",
			want:  "Nothing to see here.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := neutralizeBotMentions(tt.input)
			if got != tt.want {
				t.Errorf("neutralizeBotMentions(%q) = %q, want %q", tt.input, got, tt.want)
			}
			// Idempotency: running it again must be a no-op.
			if again := neutralizeBotMentions(got); again != got {
				t.Errorf("neutralizeBotMentions is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// TestNeutralizeBotMentionsRendersNoLiveMention asserts the property the
// issue's Definition of Done actually cares about: the rendered text must
// never contain an unwrapped "@<bot-login>" substring that GitHub would
// treat as a live mention.
func TestNeutralizeBotMentionsRendersNoLiveMention(t *testing.T) {
	input := "🏭 **Fabrik — stage: Validate (comment review)**\n\nNo change. No action, no reply.\n\n" +
		"## Comment Processing Summary\n\n**@coderabbitai**: No action taken, no reply posted — same non-blocking loop, no change in state."
	got := neutralizeBotMentions(input)
	if strings.Contains(got, "@coderabbitai") && !strings.Contains(got, "`@coderabbitai`") {
		t.Fatalf("output still contains a live mention: %q", got)
	}
	if !strings.Contains(got, "`@coderabbitai`") {
		t.Fatalf("expected neutralized mention in output, got: %q", got)
	}
}
