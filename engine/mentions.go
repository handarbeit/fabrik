package engine

import (
	"regexp"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

// codeSpanRE matches inline Markdown code spans (single-backtick delimited,
// no embedded newline). Text inside a code span is never scanned for
// mentions — this is what makes neutralizeBotMentions naturally idempotent,
// since a mention already wrapped in backticks (by Claude itself, or by a
// prior neutralization pass) is skipped on any later pass.
var codeSpanRE = regexp.MustCompile("`[^`\n]*`")

// mentionRE matches a GitHub-style @mention: an "@" preceded by start-of-text
// or a non-login character, followed by a login-shaped run of characters.
// Mirrors GitHub's own mention boundary closely enough to avoid false
// positives inside things like email addresses — the "t" immediately before
// "@" in "support@dependabot.io" is a login character, so it blocks the match.
var mentionRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)`)

// neutralizeBotMentions rewrites live @-mentions of known bot logins
// (gh.IsBotLogin) into inline code spans — e.g. "@coderabbitai" becomes
// "`@coderabbitai`" — so GitHub does not deliver a notification to the bot
// when the text is posted as a comment or written into a PR body/section.
// The "@" is preserved (not stripped) so the reader still sees which account
// is being referenced; only the live-mention behavior is suppressed.
//
// Applied inside the canonical output formatters (engine/pr.go) and
// buildAwaitingInputComment (engine/item.go) — the single point where
// Claude's freeform stage/comment output is embedded into posted content —
// rather than left to prompt instructions, which are not enforceable (#1141:
// Fabrik's own "no reply posted" summary re-mentioning "@coderabbitai" kept
// notifying the bot into an auto-reply, which Fabrik then processed again).
func neutralizeBotMentions(text string) string {
	spans := codeSpanRE.FindAllStringIndex(text, -1)
	if len(spans) == 0 {
		return neutralizeMentionsOutsideCode(text)
	}

	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, span := range spans {
		b.WriteString(neutralizeMentionsOutsideCode(text[prev:span[0]]))
		b.WriteString(text[span[0]:span[1]])
		prev = span[1]
	}
	b.WriteString(neutralizeMentionsOutsideCode(text[prev:]))
	return b.String()
}

// neutralizeMentionsOutsideCode applies the bot-mention rewrite to a segment
// known to contain no Markdown code spans.
func neutralizeMentionsOutsideCode(segment string) string {
	return mentionRE.ReplaceAllStringFunc(segment, func(match string) string {
		// match is exactly the substring the pattern matched, so re-running
		// FindStringSubmatch against it in isolation reproduces the same
		// prefix/login split without needing byte offsets into segment.
		sub := mentionRE.FindStringSubmatch(match)
		prefix, login := sub[1], sub[2]
		if !gh.IsBotLogin(login) {
			return match
		}
		return prefix + "`@" + login + "`"
	})
}
