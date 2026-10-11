package engine

import (
	"strings"
	"unicode"
)

// matchHeader reports whether line is a "KEY: value" header line for the given
// key, and returns the trimmed value. The key comparison is case-insensitive
// and tolerant of whitespace around the key and before the colon (" TITLE :"),
// and a trailing "\r" or tabs count as whitespace. The split is at the FIRST
// colon only, so a value may itself contain colons ("TITLE: fix: handle nil").
//
// The caller names the key it expects — rather than accepting any "word:" — so
// a prose first line such as "Summary: I did X" is never mistaken for a header
// of a different marker. Shared by FABRIK_PR_CREATE and FABRIK_SPAWN_CHILD
// (ADR 2091); the grammar is otherwise unchanged.
func matchHeader(line, key string) (value string, ok bool) {
	line = strings.TrimSpace(line)
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(line[:idx]), key) {
		return "", false
	}
	return strings.TrimSpace(line[idx+1:]), true
}

// firstNonEmptyLine returns the first line of content that is non-empty after
// trimming whitespace (the trimmed text), and its index in
// strings.Split(content, "\n"). Returns ("", -1) when there is none.
func firstNonEmptyLine(content string) (line string, idx int) {
	for i, l := range strings.Split(content, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t, i
		}
	}
	return "", -1
}

// quoteFoundLine renders a line of untrusted model output for quoting inside an
// inline code span in a comment or log line. It maps control characters other
// than CR/LF to a space, defuses @mentions with a zero-width space (so quoting
// a line never pings an account), and delegates newline collapsing, backtick
// replacement and truncation to sanitizeToolsDeniedCommand — the established
// convention for quoting untrusted text inline. Returns "" for an empty line.
func quoteFoundLine(line string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r != '\n' && r != '\r' && unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line)
	cleaned = strings.ReplaceAll(cleaned, "@", "@​")
	return sanitizeToolsDeniedCommand(cleaned)
}
