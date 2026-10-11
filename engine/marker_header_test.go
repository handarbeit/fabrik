package engine

import (
	"strings"
	"testing"
)

func TestMatchHeader(t *testing.T) {
	tests := []struct {
		name, line, key string
		wantVal         string
		wantOK          bool
	}{
		{"canonical", "TITLE: Add X", "TITLE", "Add X", true},
		{"lower", "title: Add X", "TITLE", "Add X", true},
		{"mixed", "Title: Add X", "TITLE", "Add X", true},
		{"padded", " TITLE : Add X", "TITLE", "Add X", true},
		{"tabs", "\tTITLE\t:\tAdd X\t", "TITLE", "Add X", true},
		{"crlf", "TITLE: Add X\r", "TITLE", "Add X", true},
		{"colon in value", "title: fix: handle nil", "TITLE", "fix: handle nil", true},
		{"depends lower", "depends_on: 3", "DEPENDS_ON", "3", true},
		{"empty value", "TITLE:", "TITLE", "", true},
		{"prose header rejected", "Summary: I did X", "TITLE", "", false},
		{"no colon", "TITLE Add X", "TITLE", "", false},
		{"prefix key rejected", "TITLES: x", "TITLE", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := matchHeader(tc.line, tc.key)
			if v != tc.wantVal || ok != tc.wantOK {
				t.Fatalf("matchHeader(%q, %q) = (%q, %v), want (%q, %v)", tc.line, tc.key, v, ok, tc.wantVal, tc.wantOK)
			}
		})
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	line, idx := firstNonEmptyLine("\n  \n  hello  \nworld")
	if line != "hello" || idx != 2 {
		t.Fatalf("got (%q, %d)", line, idx)
	}
	if line, idx := firstNonEmptyLine(" \n\t\n"); line != "" || idx != -1 {
		t.Fatalf("empty content: got (%q, %d)", line, idx)
	}
}

func TestQuoteFoundLine(t *testing.T) {
	if got := quoteFoundLine(""); got != "" {
		t.Fatalf("empty: %q", got)
	}
	got := quoteFoundLine("a `b`\nc\x07d @user")
	if strings.ContainsAny(got, "`\n\x07") {
		t.Fatalf("not sanitised: %q", got)
	}
	if strings.Contains(got, "@user") {
		t.Fatalf("mention not defused: %q", got)
	}
	long := quoteFoundLine(strings.Repeat("x", 5000))
	if len(long) > 400 {
		t.Fatalf("not truncated: %d", len(long))
	}
	if strings.Contains(long, "\n") {
		t.Fatalf("newline in truncated: %q", long)
	}
}
