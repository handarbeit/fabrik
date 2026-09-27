package github

import "testing"

func TestRestShapedLogin(t *testing.T) {
	for _, tc := range []struct{ login, typename, want string }{
		{"handarbeit-pruefer", "Bot", "handarbeit-pruefer[bot]"},
		{"fabrik-bed", "Bot", "fabrik-bed[bot]"},
		{"fabrik-bed[bot]", "Bot", "fabrik-bed[bot]"}, // already REST-shaped
		{"Fabrik-Bed[BOT]", "Bot", "Fabrik-Bed[BOT]"}, // suffix check is case-insensitive
		{"verveguy", "User", "verveguy"},
		{"arbeithand", "", "arbeithand"}, // older response without __typename
		{"", "Bot", ""},
	} {
		if got := restShapedLogin(tc.login, tc.typename); got != tc.want {
			t.Errorf("restShapedLogin(%q, %q) = %q, want %q", tc.login, tc.typename, got, tc.want)
		}
	}
}

// TestToComment_BotAuthorIsRESTShaped: a comment authored by a GitHub App
// comes back from GraphQL as the bare slug. Before normalization,
// IsBotLogin("handarbeit-pruefer") was false, so the App's comments counted
// as human, and under App auth Fabrik's own comments ("fabrik-bed") never
// matched its selfLogin() ("fabrik-bed[bot]").
func TestToComment_BotAuthorIsRESTShaped(t *testing.T) {
	var bot commentNodeData
	bot.Author = &struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	}{Typename: "Bot", Login: "fabrik-bed"}
	c := toComment(bot, 0)
	if c.Author != "fabrik-bed[bot]" {
		t.Fatalf("Author = %q, want fabrik-bed[bot]", c.Author)
	}
	if !IsBotLogin(c.Author) {
		t.Errorf("IsBotLogin(%q) = false, want true", c.Author)
	}

	var human commentNodeData
	human.Author = &struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	}{Typename: "User", Login: "arbeithand"}
	if got := toComment(human, 0).Author; got != "arbeithand" || IsBotLogin(got) {
		t.Errorf("human author = %q (bot=%v), want arbeithand, not a bot", got, IsBotLogin(got))
	}
}
