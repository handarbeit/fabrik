package pruefer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	coverageBeginRE = regexp.MustCompile(`(?m)^PRUEFER_COVERAGE_BEGIN\r?$\n?`)
	// coverageLineRE matches a model-written line that imitates the fixed
	// status line; appendCoverage defuses it so it can never be mistaken for the
	// one Go appends.
	coverageLineRE = regexp.MustCompile(`(?m)^([ \t]*)(PRUEFER_COVERAGE:)`)
)

// coverageFailOpenForTest flips the fail-closed default so a test can show the
// missing-report assertions are not vacuous. Never set outside tests.
var coverageFailOpenForTest bool

// extractCoverage finds the model's coverage report: the final
// PRUEFER_COVERAGE_BEGIN/END block, and only when nothing but whitespace
// follows it (the prompt puts it last). It returns the reported entry IDs,
// whether a well-formed report was found, and text with that block removed.
// Anything earlier in the output is left alone, so a summary or finding that
// quotes the markers (this repo's own contract, for one) is neither deleted nor
// mistaken for the report. A tail block that is not a JSON object with an
// "opened" array is removed but counts as absent.
func extractCoverage(text string) (opened []string, ok bool, rest string) {
	matches := coverageBlockRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return nil, false, text
	}
	m := matches[len(matches)-1]
	if strings.TrimSpace(text[m[1]:]) != "" {
		return nil, false, text
	}
	blockStart, body := m[0], text[m[2]:m[3]]
	// A quoted BEGIN with no END of its own earlier in the output would make the
	// lazy match start there; the report is what follows the last BEGIN line.
	if locs := coverageBeginRE.FindAllStringIndex(body, -1); len(locs) > 0 {
		last := locs[len(locs)-1]
		blockStart, body = m[2]+last[0], body[last[1]:]
	}
	rest = text[:blockStart]
	var parsed struct {
		Opened *[]string `json:"opened"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &parsed); err != nil || parsed.Opened == nil {
		return nil, false, rest
	}
	return *parsed.Opened, true, rest
}

// Coverage is the computed result for one review.
type Coverage struct {
	Complete     bool
	Reason       string   // why partial; empty when complete
	Unread       []string // original paths, sorted and unique
	UnreadCount  int      // entries not opened
	TotalEntries int
	// TruncatedUnread is how many of the unread entries were named by a shell
	// command whose result the CLI cut to a `<persisted-output>` preview (#2362).
	// Diagnostic only: it appears in the log line and never changes Complete.
	TruncatedUnread int
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
	// UnobservedSubagents says a Task/Agent call ran whose traffic the transcript
	// does not show. Their reads cannot be verified, so the model's report is
	// then intersected with the transcript anyway: entries only an invisible
	// subagent could have read count as unread.
	UnobservedSubagents bool
	// Truncated is the set of entry IDs named by a shell command whose result
	// came back as a preview (see analyzeTranscript).
	Truncated map[string]bool
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
			if known[id] && (!(in.TranscriptAuthoritative || in.UnobservedSubagents) || in.Transcript[id]) {
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
			if in.Truncated[e.ID] {
				cov.TruncatedUnread++
			}
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
		if in.UnobservedSubagents {
			cov.Reason += " (subagent reads are not visible in the transcript and are not counted)"
		}
	case len(in.TrimmedPaths) > 0:
		cov.Reason = fmt.Sprintf("%d file(s) dropped to fit max_diff_bytes were never reviewed", len(in.TrimmedPaths))
	}
	cov.Complete = cov.Reason == ""
	return cov
}

// appendCoverage appends the fixed machine-readable coverage line (and, for a
// partial review, the unread paths) to body. The line is added to every review,
// complete or partial, so its absence only ever means a review from before this
// contract.
func appendCoverage(body string, cov Coverage) string {
	// A model line that starts with the marker is kept (it may be a quotation)
	// but moved off the start of the line, so `^PRUEFER_COVERAGE:` matches only
	// the line below.
	body = strings.TrimRight(coverageLineRE.ReplaceAllString(body, "> $2"), "\n")
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
// transcriptPath, when non-empty, is the saved transcript a partial line names (#2303).
func logCoverage(prNumber int, cov Coverage, authoritative bool, transcriptPath string) {
	if cov.Complete {
		logf(prNumber, "review", "coverage: complete (%d entries; transcript authoritative=%t)\n", cov.TotalEntries, authoritative)
		return
	}
	saved := ""
	if transcriptPath != "" {
		saved = "; transcript saved: " + transcriptPath
	}
	truncated := ""
	if cov.TruncatedUnread > 0 {
		truncated = fmt.Sprintf("; %d of the unread entries were in shell results cut to a preview", cov.TruncatedUnread)
	}
	logf(prNumber, "warn", "coverage: partial — %s (%d unread path(s)%s; transcript authoritative=%t)%s\n", cov.Reason, len(cov.Unread), truncated, authoritative, saved)
}

// streamEvent is the subset of a stream-json line deriveOpened reads.
type streamEvent struct {
	Type            string  `json:"type"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Message         struct {
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
			Input     struct {
				FilePath string `json:"file_path"`
				Offset   int    `json:"offset"`
				Limit    int    `json:"limit"`
				Command  string `json:"command"`
			} `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// toolResultText flattens a tool_result's content (a string, or an array of
// text blocks) to plain text.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		b.WriteString(bl.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// readLineRE matches the "<line number><tab>" prefix Read puts on every line
// (pinned by testdata/read_result_real.jsonl, captured from the installed CLI).
// The arrow form of `cat -n`-style renderers is accepted too, so a change of
// separator cannot silently turn every Read into "unread".
var readLineRE = regexp.MustCompile(`^\s*(\d+)(?:\t|→)`)

// shellMeta marks a command whose cat output may be truncated, redirected or
// combined, so it is not counted as opening a file.
const shellMeta = "|;&<>$`\"'\\(){}*?!#~\n"

// deriveOpened reads the stream-json transcript and returns the staged entry
// IDs the transcript proves were read in full, plus whether it is
// authoritative. Read truncates its result silently (a 32 KiB chunk came back
// cut at ~22 KB with no notice and no error in the live probe), so a successful
// Read is not proof: an entry counts only when the numbered lines Read returned
// cover 1..Lines, across however many Read calls (subagents' included) it took.
// An entry with a line over the Read line-width limit (wide) is never credited
// to Read, which cuts such lines mid-line; `cat` credits an entry only when its
// output contains the entry's whole text.
//
// The transcript is authoritative only when it shows all subagent traffic: no
// Task/Agent call at all, or every such call has events tagged with its
// parent_tool_use_id. Otherwise a subagent could have read files Go cannot see.
func deriveOpened(stream []byte, s *StagedDiff) (opened map[string]bool, authoritative bool) {
	f := analyzeTranscript(stream, s)
	return f.opened, f.authoritative
}

// transcriptFacts is what analyzeTranscript reads from a stream-json transcript.
type transcriptFacts struct {
	opened        map[string]bool
	authoritative bool
	// truncated is the set of staged entry IDs named by a Bash command whose
	// result the CLI replaced with a `<persisted-output>` preview (#2362). It is
	// diagnostic only: it never credits and never un-credits an entry.
	truncated map[string]bool
}

// persistedOutputPrefix starts a Bash result the CLI replaced with a preview
// because the output was too large to return whole.
const persistedOutputPrefix = "<persisted-output>"

// isPersistedOutput reports whether a tool_result's text is a preview notice. It
// is structural: the text must start with the marker, so a result that merely
// quotes it (a diff of this very code, say) is not one.
func isPersistedOutput(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), persistedOutputPrefix)
}

// analyzeTranscript is deriveOpened plus the truncation attribution. See
// deriveOpened for the crediting rules, which it keeps; a preview result never
// credits an entry.
func analyzeTranscript(stream []byte, s *StagedDiff) transcriptFacts {
	opened := map[string]bool{}
	truncated := map[string]bool{}
	if s == nil {
		return transcriptFacts{opened: opened, truncated: truncated}
	}
	byPath := make(map[string]StagedEntry, len(s.Entries))
	for _, e := range s.Entries {
		byPath[filepath.Clean(s.EntryPath(e))] = e
	}
	type pending struct {
		read bool // a Read of ids[0]; otherwise a cat of ids
		ids  []string
	}
	calls := map[string]pending{}
	named := map[string][]string{} // Bash tool_use ID -> staged entries its command names
	lineSeen := map[string]map[int]bool{}
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
					if e, ok := byPath[filepath.Clean(c.Input.FilePath)]; ok && !e.Wide {
						calls[c.ID] = pending{read: true, ids: []string{e.ID}}
					}
				case "Bash":
					if ids := stagedEntryIDsNamed(c.Input.Command, s); len(ids) > 0 {
						named[c.ID] = ids
					}
					if ids := catEntryIDs(c.Input.Command, byPath); len(ids) > 0 {
						calls[c.ID] = pending{ids: ids}
					}
				}
			case ev.Type == "user" && c.Type == "tool_result":
				text := toolResultText(c.Content)
				preview := !c.IsError && isPersistedOutput(text)
				if preview {
					for _, id := range named[c.ToolUseID] {
						truncated[id] = true
					}
				}
				p, ok := calls[c.ToolUseID]
				if !ok || c.IsError || preview {
					continue
				}
				if p.read {
					id := p.ids[0]
					if lineSeen[id] == nil {
						lineSeen[id] = map[int]bool{}
					}
					for _, l := range strings.Split(text, "\n") {
						if m := readLineRE.FindStringSubmatch(l); m != nil {
							n, _ := strconv.Atoi(m[1])
							lineSeen[id][n] = true
						}
					}
					continue
				}
				for _, id := range p.ids {
					for _, e := range s.Entries {
						if e.ID == id && e.content != "" && strings.Contains(text, strings.TrimRight(e.content, "\n")) {
							opened[id] = true
						}
					}
				}
			}
		}
	}
	for _, e := range s.Entries {
		seen := lineSeen[e.ID]
		if len(seen) == 0 || e.Lines == 0 {
			continue
		}
		whole := true
		for n := 1; n <= e.Lines; n++ {
			if !seen[n] {
				whole = false
				break
			}
		}
		if whole {
			opened[e.ID] = true
		}
	}
	authoritative := true
	for id := range taskIDs {
		if !taggedParents[id] {
			authoritative = false
		}
	}
	return transcriptFacts{opened: opened, authoritative: authoritative, truncated: truncated}
}

// stagedEntryIDsNamed returns the staged entries a shell command names, however
// the command is shaped: an absolute entry path, or a bare <ID>.diff name (the
// `cd <dir>; cat 0001.diff ...` form catEntryIDs rejects). It only attributes a
// truncated result to entries; it never credits an open, so the strictness of
// catEntryIDs is untouched (#2362, R4).
func stagedEntryIDsNamed(command string, s *StagedDiff) []string {
	byPath := make(map[string]string, len(s.Entries))
	byFile := make(map[string]string, len(s.Entries))
	for _, e := range s.Entries {
		byPath[filepath.Clean(s.EntryPath(e))] = e.ID
		byFile[e.File] = e.ID
	}
	tokens := strings.FieldsFunc(command, func(r rune) bool {
		return strings.ContainsRune(" \t\r\n;&|()<>", r)
	})
	seen := map[string]bool{}
	var ids []string
	for _, tok := range tokens {
		tok = strings.Trim(tok, "\"'`")
		id, ok := byPath[filepath.Clean(tok)]
		if !ok {
			id, ok = byFile[tok]
		}
		if ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
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
