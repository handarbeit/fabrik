// Package closingrefs finds the issues a pull request would close when it merges, using the
// closing-keyword grammar GitHub applies to a PR description and to commit messages
// (#2576, ADR 2576).
//
// GitHub matches word adjacency, not grammar: "this PR does not close #12" still closes
// issue 12. The matcher therefore looks only at a keyword followed by a target and ignores
// the sentence around them. It deliberately errs towards matching, because a missed match
// reopens the incident the check exists to prevent while an extra match costs one reworded
// sentence.
//
// The package is pure: it takes text and returns structured references. It performs no
// network access and imports nothing from the engine (deps_test.go pins this).
package closingrefs

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Commit is one commit message on the PR.
type Commit struct {
	SHA     string
	Message string
}

// Location says where a closing phrase was found.
type Location struct {
	// Where is "PR body" or "commit <sha>".
	Where string
	// Phrase is the keyword and its target as written, e.g. "Closes #12".
	Phrase string
}

// Ref is one issue the PR would close, with every place that says so.
type Ref struct {
	// Repo is the lower-cased "owner/repo" of the target issue.
	Repo   string
	Number int
	// Locations are in order of first appearance, body first, then commits in order.
	Locations []Location
}

// String renders "owner/repo#N".
func (r Ref) String() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// Options tunes the matcher.
type Options struct {
	// ExemptCode ignores keywords inside fenced code blocks and inline code spans. Off by
	// default: whether GitHub ignores them is unverified (testdata/README.md), and the safe
	// answer for a guard is to match.
	ExemptCode bool
}

const (
	keyword = `(?i:close[sd]?|fix(?:e[sd])?|resolve[sd]?)`
	// separator between keyword and target: whitespace (incl. newlines), a colon, Markdown
	// emphasis or strikethrough, and the opening of a link or autolink.
	separator = `[\s:*_~\[\]<]*`
	ident     = `[A-Za-z0-9_.-]+`
	target    = `(?:https?://github\.com/(` + ident + `)/(` + ident + `)/issues/(\d+)` +
		`|(` + ident + `)/(` + ident + `)#(\d+)` +
		`|(?i:GH-)(\d+)` +
		`|#(\d+))`
)

// The leading group stands in for a look-behind (RE2 has none): the keyword must not be the
// tail of a longer word, but an underscore (italics) or asterisk before it is fine.
var closingRE = regexp.MustCompile(`(^|[^A-Za-z0-9])(` + keyword + `)` + separator + target)

// Text is one piece of PR text to scan, labelled with where it came from.
type Text struct {
	// Where is shown in messages, e.g. "PR title".
	Where string
	Body  string
}

// Find returns every issue that body and commits would close, deduplicated by repository and
// number. selfRepo ("owner/repo") resolves a bare #N.
func Find(body string, commits []Commit, selfRepo string, opts Options) []Ref {
	return FindTexts(selfRepo, opts, joinTexts([]Text{{Where: "PR body", Body: body}}, commits)...)
}

func joinTexts(texts []Text, commits []Commit) []Text {
	for _, c := range commits {
		texts = append(texts, Text{Where: "commit " + c.SHA, Body: c.Message})
	}
	return texts
}

// FindTexts is Find over arbitrary labelled texts, scanned in order. selfRepo resolves a bare
// #N or GH-N.
func FindTexts(selfRepo string, opts Options, texts ...Text) []Ref {
	self := strings.ToLower(selfRepo)
	var order []string
	byKey := map[string]*Ref{}

	scan := func(text, where string) {
		if opts.ExemptCode {
			text = blankCode(text)
		}
		for _, m := range closingRE.FindAllStringSubmatch(text, -1) {
			repo, num, tgt := resolveTarget(m, self)
			if num <= 0 {
				continue
			}
			key := fmt.Sprintf("%s#%d", repo, num)
			r, ok := byKey[key]
			if !ok {
				r = &Ref{Repo: repo, Number: num}
				byKey[key] = r
				order = append(order, key)
			}
			loc := Location{Where: where, Phrase: m[2] + " " + tgt}
			if !hasLocation(r.Locations, loc) {
				r.Locations = append(r.Locations, loc)
			}
		}
	}

	for _, t := range texts {
		scan(t.Body, t.Where)
	}

	out := make([]Ref, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

func hasLocation(ls []Location, l Location) bool {
	for _, x := range ls {
		if x == l {
			return true
		}
	}
	return false
}

// resolveTarget reads the target groups of a closingRE match (groups 3..10).
func resolveTarget(m []string, self string) (repo string, num int, text string) {
	switch {
	case m[3] != "": // issue URL
		n, _ := strconv.Atoi(m[5])
		return strings.ToLower(m[3] + "/" + m[4]), n, "#" + m[5] + " (" + m[3] + "/" + m[4] + ")"
	case m[6] != "": // owner/repo#N
		n, _ := strconv.Atoi(m[8])
		return strings.ToLower(m[6] + "/" + m[7]), n, m[6] + "/" + m[7] + "#" + m[8]
	case m[9] != "": // GH-N
		n, _ := strconv.Atoi(m[9])
		return self, n, "GH-" + m[9]
	case m[10] != "": // bare #N
		n, _ := strconv.Atoi(m[10])
		return self, n, "#" + m[10]
	}
	return "", 0, ""
}

var inlineCodeRE = regexp.MustCompile("(`+)[^`]*?(?:`+)")

// blankCode replaces fenced code blocks and inline code spans with spaces (newlines kept),
// so a later scan cannot match inside them. Fences follow CommonMark: up to three spaces of
// indentation, three or more backticks or tildes, closed by a fence of the same character at
// least as long; an unclosed fence runs to the end of the text.
func blankCode(text string) string {
	lines := strings.Split(text, "\n")
	var fenceChar byte
	fenceLen := 0
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if fenceChar == 0 {
			if indent <= 3 {
				if ch, n := fenceRun(trimmed); n >= 3 && !(ch == '`' && strings.Contains(trimmed[n:], "`")) {
					fenceChar, fenceLen = ch, n
					lines[i] = strings.Repeat(" ", len(line))
					continue
				}
			}
			lines[i] = inlineCodeRE.ReplaceAllStringFunc(line, func(s string) string {
				return strings.Repeat(" ", len(s))
			})
			continue
		}
		if ch, n := fenceRun(trimmed); indent <= 3 && ch == fenceChar && n >= fenceLen &&
			strings.TrimSpace(trimmed[n:]) == "" {
			fenceChar, fenceLen = 0, 0
		}
		lines[i] = strings.Repeat(" ", len(line))
	}
	return strings.Join(lines, "\n")
}

// fenceRun returns the fence character and run length at the start of s (0, 0 if none).
func fenceRun(s string) (byte, int) {
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return 0, 0
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	return s[0], n
}

// Sorted returns refs ordered by repository then number (stable output for messages).
func Sorted(refs []Ref) []Ref {
	out := append([]Ref(nil), refs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out
}
