package pruefer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Coverage contract (#2190, ADR 2190). A review states which staged diff
// entries it opened; Go, not the model, decides what that means:
//
//   - the model reports opened entry IDs in a PRUEFER_COVERAGE_BEGIN/END block;
//   - when the stream-json transcript shows every subagent's traffic, Go
//     intersects the report with what the transcript proves was read, so the
//     model cannot claim `complete` for an entry it never opened;
//   - an entry nobody reports is unread, and a missing or unparseable report is
//     `partial`, never `complete` (this reverses ADR 1456's fail-open stance for
//     coverage only; the review is still submitted).
//
// The result is rendered as a fixed `PRUEFER_COVERAGE:` line in the review body.

// coveragePrefix starts the fixed, greppable line appended to every review body.
const coveragePrefix = "PRUEFER_COVERAGE:"

// maxCoverageListed caps the unread paths listed in the review body.
const maxCoverageListed = 50

var (
	coverageBlockRE = regexp.MustCompile(`(?ms)^PRUEFER_COVERAGE_BEGIN\r?$(.*?)^PRUEFER_COVERAGE_END\r?$\n?`)
	// coverageLineRE matches a model-written line that imitates the fixed
	// status line, so it can never be mistaken for the one Go appends.
	coverageLineRE = regexp.MustCompile(`(?m)^[ \t]*PRUEFER_COVERAGE:.*$\n?`)
)

// coverageFailOpenForTest flips the fail-closed default so a test can show the
// missing-report assertions are not vacuous. Never set outside tests.
var coverageFailOpenForTest bool

// extractCoverage finds the model's coverage report in text. It returns the
// reported entry IDs, whether a well-formed report was found, and text with
// every coverage block removed. A malformed block (not a JSON object with an
// "opened" array) counts as absent. The last well-formed block wins.
func extractCoverage(text string) (opened []string, ok bool, rest string) {
	matches := coverageBlockRE.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		var parsed struct {
			Opened *[]string `json:"opened"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &parsed); err != nil || parsed.Opened == nil {
			continue
		}
		opened, ok = *parsed.Opened, true
	}
	return opened, ok, coverageBlockRE.ReplaceAllString(text, "")
}

// Coverage is the computed result for one review.
type Coverage struct {
	Complete     bool
	Reason       string   // why partial; empty when complete
	Unread       []string // original paths, sorted and unique
	UnreadCount  int      // entries not opened
	TotalEntries int
}

// coverageInput is everything computeCoverage needs.
type coverageInput struct {
	Staged       *StagedDiff
	ChangedPaths []string // used only when nothing was staged
	TrimmedPaths []string // dropped to fit max_diff_bytes: never reviewed
	Reported     []string
	ReportOK     bool
	// Transcript is the set of entry IDs the stream proves were read in full,
	// and TranscriptAuthoritative says whether every subagent's traffic was
	// visible there. When it is not, the transcript is not used to downgrade.
	Transcript              map[string]bool
	TranscriptAuthoritative bool
}

func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// computeCoverage decides complete versus partial. It fails closed: anything it
// cannot establish is unread.
func computeCoverage(in coverageInput) Coverage {
	if in.Staged == nil {
		return Coverage{Reason: "no staged diff", Unread: sortedUnique(in.ChangedPaths)}
	}
	entries := in.Staged.Entries
	cov := Coverage{TotalEntries: len(entries)}

	known := make(map[string]bool, len(entries))
	for _, e := range entries {
		known[e.ID] = true
	}
	opened := make(map[string]bool, len(entries))
	switch {
	case in.ReportOK:
		for _, id := range in.Reported {
			id = strings.TrimSuffix(strings.TrimSpace(id), ".diff")
			if known[id] && (!in.TranscriptAuthoritative || in.Transcript[id]) {
				opened[id] = true
			}
		}
	case coverageFailOpenForTest:
		for id := range known {
			opened[id] = true
		}
	}

	var unread []string
	for _, e := range entries {
		if !opened[e.ID] {
			cov.UnreadCount++
			unread = append(unread, e.Path)
		}
	}
	unread = append(unread, in.TrimmedPaths...)
	cov.Unread = sortedUnique(unread)

	switch {
	case !in.ReportOK && !coverageFailOpenForTest:
		cov.Reason = "reviewer did not report coverage"
	case cov.UnreadCount > 0:
		cov.Reason = fmt.Sprintf("%d of %d diff entries not opened", cov.UnreadCount, cov.TotalEntries)
	case len(in.TrimmedPaths) > 0:
		cov.Reason = fmt.Sprintf("%d file(s) dropped to fit max_diff_bytes were never reviewed", len(in.TrimmedPaths))
	}
	cov.Complete = cov.Reason == ""
	return cov
}

// appendCoverage appends the fixed machine-readable coverage line (and, for a
// partial review, the unread paths) to body. The line is added to every review,
// complete or partial, so its absence only ever means a review from before this
// contract. A model-written imitation of the line is removed first.
func appendCoverage(body string, cov Coverage) string {
	body = strings.TrimRight(coverageLineRE.ReplaceAllString(body, ""), "\n")
	var b strings.Builder
	b.WriteString(body)
	if body != "" {
		b.WriteString("\n\n")
	}
	if cov.Complete {
		b.WriteString(coveragePrefix + " complete")
		return b.String()
	}
	b.WriteString(coveragePrefix + " partial — " + cov.Reason)
	listed := cov.Unread
	if len(listed) > maxCoverageListed {
		listed = listed[:maxCoverageListed]
	}
	for _, p := range listed {
		fmt.Fprintf(&b, "\n- `%s`", strings.ReplaceAll(sanitizeIndexText(p), "`", "'"))
	}
	if extra := len(cov.Unread) - len(listed); extra > 0 {
		fmt.Fprintf(&b, "\n- and %d more", extra)
	}
	return b.String()
}

// logCoverage logs the computed coverage at the call site that has the PR number.
func logCoverage(prNumber int, cov Coverage, authoritative bool) {
	if cov.Complete {
		logf(prNumber, "review", "coverage: complete (%d entries; transcript authoritative=%t)\n", cov.TotalEntries, authoritative)
		return
	}
	logf(prNumber, "warn", "coverage: partial — %s (%d unread path(s); transcript authoritative=%t)\n", cov.Reason, len(cov.Unread), authoritative)
}

// streamEvent is the subset of a stream-json line deriveOpened reads.
type streamEvent struct {
	Type            string  `json:"type"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		Content []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
			Input     struct {
				FilePath string `json:"file_path"`
				Offset   int    `json:"offset"`
				Limit    int    `json:"limit"`
				Command  string `json:"command"`
			} `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// shellMeta marks a command whose cat output may be truncated, redirected or
// combined, so it is not counted as opening a file.
const shellMeta = "|;&<>$`\"'\\(){}*?!#~\n"

// deriveOpened reads the stream-json transcript and returns the staged entry
// IDs a successful, untruncated read proves were opened, plus whether the
// transcript is authoritative. A Read counts when it starts at the top of the
// file and has no limit or one covering the whole entry; so does `cat <file>`
// with only plain staged paths. Every successful read anywhere in the
// transcript counts, subagents' included.
//
// The transcript is authoritative only when it shows all subagent traffic: no
// Task call at all, or every Task call has events tagged with its
// parent_tool_use_id. Otherwise a subagent could have read files Go cannot see,
// and the model's own report must stand.
func deriveOpened(stream []byte, s *StagedDiff) (opened map[string]bool, authoritative bool) {
	opened = map[string]bool{}
	if s == nil {
		return opened, false
	}
	byPath := make(map[string]StagedEntry, len(s.Entries))
	for _, e := range s.Entries {
		byPath[filepath.Clean(s.EntryPath(e))] = e
	}
	type pending struct{ ids []string }
	calls := map[string]pending{}
	taskIDs := map[string]bool{}
	taggedParents := map[string]bool{}

	for _, line := range bytes.Split(stream, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.ParentToolUseID != nil && *ev.ParentToolUseID != "" {
			taggedParents[*ev.ParentToolUseID] = true
		}
		for _, c := range ev.Message.Content {
			switch {
			case ev.Type == "assistant" && c.Type == "tool_use":
				switch c.Name {
				case "Task", "Agent":
					taskIDs[c.ID] = true
				case "Read":
					if e, ok := byPath[filepath.Clean(c.Input.FilePath)]; ok &&
						c.Input.Offset <= 1 && (c.Input.Limit == 0 || c.Input.Limit >= e.Lines) {
						calls[c.ID] = pending{ids: []string{e.ID}}
					}
				case "Bash":
					if ids := catEntryIDs(c.Input.Command, byPath); len(ids) > 0 {
						calls[c.ID] = pending{ids: ids}
					}
				}
			case ev.Type == "user" && c.Type == "tool_result":
				if p, ok := calls[c.ToolUseID]; ok && !c.IsError {
					for _, id := range p.ids {
						opened[id] = true
					}
				}
			}
		}
	}
	authoritative = true
	for id := range taskIDs {
		if !taggedParents[id] {
			authoritative = false
		}
	}
	return opened, authoritative
}

// catEntryIDs returns the entries a plain `cat <path>...` command opens, or nil
// when the command is anything else.
func catEntryIDs(command string, byPath map[string]StagedEntry) []string {
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, "cat ") || strings.ContainsAny(command, shellMeta) {
		return nil
	}
	var ids []string
	for _, arg := range strings.Fields(command)[1:] {
		e, ok := byPath[filepath.Clean(arg)]
		if !ok {
			return nil // reads something else too: not a clean open of staged files
		}
		ids = append(ids, e.ID)
	}
	return ids
}
