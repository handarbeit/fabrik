package watch

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/handarbeit/fabrik/streamfilter"
)

// stageTab represents a single tab in the stage tab bar.
type stageTab struct {
	Label   string // stage name parsed from log filename
	LogPath string // path to the newest log file for this label
	IsLive  bool   // true if this is the globally newest log file
}

// stageNameFromFilename extracts the stage label from a log filename.
// Format: <safeLabel>-<yyyyMMdd-HHmmss>-<nanos>.log
// Returns everything before the first 8-digit all-numeric date segment.
func stageNameFromFilename(name string) string {
	base := strings.TrimSuffix(name, ".log")
	parts := strings.Split(base, "-")
	var label []string
	for _, p := range parts {
		if len(p) == 8 && isAllDigits(p) {
			break
		}
		label = append(label, p)
	}
	return strings.Join(label, "-")
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// logTimestampSuffix returns the timestamp portion of a log filename:
// <yyyyMMdd>-<HHmmss>-<nanos>.log — everything from the first 8-digit
// all-numeric segment onward. Because timestamps are zero-padded with fixed
// widths, lexicographic comparison of this suffix gives correct chronological
// ordering regardless of the stage label prefix. Falls back to name unchanged
// if no 8-digit segment is found (e.g. malformed filenames).
func logTimestampSuffix(name string) string {
	base := strings.TrimSuffix(name, ".log")
	parts := strings.Split(base, "-")
	for i, p := range parts {
		if len(p) == 8 && isAllDigits(p) {
			return strings.Join(parts[i:], "-") + ".log"
		}
	}
	return name
}

// buildStageTabs scans logDir, groups .log files by stage label, picks the
// newest log per label, and sorts by pipeline order (stageOrder map). Logs
// whose label matches "<ParentStage>-comment-review" are grouped under the
// parent stage tab. Unknown stages are appended after known stages in
// chronological order.
//
// IsLive selection: when activeStage is non-empty and a tab with that label
// exists, it is marked IsLive (label-driven; GitHub state takes precedence
// over log file timestamps). When activeStage is empty or names a stage with
// no tab, falls back to the globally newest log file.
//
// stageOrder maps stage name → pipeline order value (from Stage.Order).
// When stageOrder is nil or empty, falls back to chronological ordering.
func buildStageTabs(logDir string, stageOrder map[string]int, activeStage string) []stageTab {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil
	}

	// newestPerLabel maps label -> newest filename (lexicographic = chronological)
	newestPerLabel := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		label := stageNameFromFilename(e.Name())
		if label == "" {
			continue
		}
		if e.Name() > newestPerLabel[label] {
			newestPerLabel[label] = e.Name()
		}
	}
	if len(newestPerLabel) == 0 {
		return nil
	}

	// Two-pass comment-review grouping:
	// Pass 1: collect direct (non-comment-review) labels.
	// Pass 2: for labels ending in "-comment-review", if the parent is a known
	//         stage, merge the newest file under the parent label.
	effectiveNewest := make(map[string]string) // effective label -> newest filename
	for label, filename := range newestPerLabel {
		if strings.HasSuffix(label, "-comment-review") {
			continue // handled in pass 2
		}
		effectiveNewest[label] = filename
	}
	for label, filename := range newestPerLabel {
		if !strings.HasSuffix(label, "-comment-review") {
			continue
		}
		parent := strings.TrimSuffix(label, "-comment-review")
		if _, known := stageOrder[parent]; known {
			// Merge under parent: keep whichever filename is newer.
			if logTimestampSuffix(filename) > logTimestampSuffix(effectiveNewest[parent]) {
				effectiveNewest[parent] = filename
			}
		} else {
			// Unknown parent — keep as its own tab.
			if existing, ok := effectiveNewest[label]; !ok || logTimestampSuffix(filename) > logTimestampSuffix(existing) {
				effectiveNewest[label] = filename
			}
		}
	}

	// Separate known-stage tabs from unknown-stage tabs.
	var knownTabs []stageTab
	var unknownTabs []stageTab
	for label, filename := range effectiveNewest {
		tab := stageTab{
			Label:   label,
			LogPath: filepath.Join(logDir, filename),
		}
		if _, ok := stageOrder[label]; ok {
			knownTabs = append(knownTabs, tab)
		} else {
			unknownTabs = append(unknownTabs, tab)
		}
	}

	// Sort known tabs by pipeline order (tie-break by label for stability).
	sort.Slice(knownTabs, func(i, j int) bool {
		oi, oj := stageOrder[knownTabs[i].Label], stageOrder[knownTabs[j].Label]
		if oi != oj {
			return oi < oj
		}
		return knownTabs[i].Label < knownTabs[j].Label
	})
	// Sort unknown tabs chronologically by newest filename.
	sort.Slice(unknownTabs, func(i, j int) bool {
		return filepath.Base(unknownTabs[i].LogPath) < filepath.Base(unknownTabs[j].LogPath)
	})

	tabs := append(knownTabs, unknownTabs...)

	// Determine which tab is IsLive.
	// Primary: when activeStage is non-empty, find the tab with that label.
	// Fallback: pick the tab whose LogPath has the globally newest timestamp suffix.
	if activeStage != "" {
		for i, t := range tabs {
			if t.Label == activeStage {
				tabs[i].IsLive = true
				return tabs
			}
		}
		// activeStage names a stage that has no tab yet — fall through to timestamp heuristic.
	}
	// Timestamp-based fallback: compare by timestamp suffix only (not full filename)
	// so that a stage label like "Specify" (alphabetically > "Research") cannot beat
	// a chronologically newer "Research" log.
	newestFile := ""
	newestSuffix := ""
	for _, t := range tabs {
		base := filepath.Base(t.LogPath)
		if suffix := logTimestampSuffix(base); suffix > newestSuffix {
			newestSuffix = suffix
			newestFile = base
		}
	}
	for i, t := range tabs {
		if filepath.Base(t.LogPath) == newestFile {
			tabs[i].IsLive = true
			break
		}
	}

	return tabs
}

// renderLogFile reads the log file at path, renders each NDJSON line via
// streamfilter.RenderLine, and returns the joined result ready for viewport display.
func renderLogFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := append(scanner.Bytes(), '\n')
		if rendered := streamfilter.RenderLine(line); rendered != "" {
			b.WriteString(rendered)
		}
	}
	return b.String()
}

// StartLogFollower launches two goroutines:
//  1. A file-follow goroutine that reads the newest .log file in logDir in
//     real time, calling send(LogLineMsg) for each rendered line.
//  2. A directory-poll goroutine (2s interval) that detects when a new .log
//     file appears (stage transition) and calls send(NewLogFileMsg).
//
// getActiveStage is called by the follow goroutine to determine which stage's
// log to follow; "" means fall back to the globally newest log file.
// Both goroutines run until the done channel is closed.
func StartLogFollower(logDir string, send func(tea.Msg), done <-chan struct{}, getActiveStage func() string) {
	go followLatestLog(logDir, getActiveStage, send, done)
	go pollForNewLogFile(logDir, send, done)
}

// newestLogFile returns the path of the most recently modified .log file in
// logDir, or "" if none exist.
func newestLogFile(logDir string) string {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return ""
	}
	// Collect .log files
	var logs []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e)
		}
	}
	if len(logs) == 0 {
		return ""
	}
	// Sort by timestamp suffix only (not full filename) so that a stage label
	// prefix cannot distort chronological ordering. Within a single label the
	// sort is still correct, and across labels the timestamp alone determines
	// which file is newest.
	sort.Slice(logs, func(i, j int) bool {
		return logTimestampSuffix(logs[i].Name()) < logTimestampSuffix(logs[j].Name())
	})
	return filepath.Join(logDir, logs[len(logs)-1].Name())
}

// bestLogFileForStage returns the path of the most recent .log file in logDir
// whose stage label is stageName or stageName+"-comment-review". When stageName
// is empty or no matching file exists, falls back to newestLogFile.
func bestLogFileForStage(logDir, stageName string) string {
	if stageName == "" {
		return newestLogFile(logDir)
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return newestLogFile(logDir)
	}
	var best string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		label := stageNameFromFilename(e.Name())
		if label != stageName && label != stageName+"-comment-review" {
			continue
		}
		if logTimestampSuffix(e.Name()) > logTimestampSuffix(best) {
			best = e.Name()
		}
	}
	if best == "" {
		return newestLogFile(logDir)
	}
	return filepath.Join(logDir, best)
}

// followLatestLog finds the best .log file in logDir for the active stage (or
// globally newest when stage is unknown), then follows it in real time (like
// tail -F). It blocks until done is closed.
func followLatestLog(logDir string, getActiveStage func() string, send func(tea.Msg), done <-chan struct{}) {
	// Wait for the log directory and a .log file to exist.
	var currentPath string
	for {
		if currentPath == "" {
			currentPath = bestLogFileForStage(logDir, getActiveStage())
		}
		if currentPath != "" {
			break
		}
		select {
		case <-done:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}

	followFile(currentPath, logDir, getActiveStage, send, done)
}

// followFile reads from path, rendering each NDJSON line via streamfilter.
// It loops — switching to the best log file for the active stage as files
// appear — until done is closed. Using a loop instead of recursion avoids
// goroutine stack growth across stage transitions.
func followFile(path, logDir string, getActiveStage func() string, send func(tea.Msg), done <-chan struct{}) {
	for {
		f, err := os.Open(path)
		if err != nil {
			// File disappeared; wait briefly and try the best file for the active stage.
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			if next := bestLogFileForStage(logDir, getActiveStage()); next != "" && next != path {
				path = next
			}
			continue
		}

		reader := bufio.NewReader(f)
		switchFile := false
		turnCount := 0
	readLoop:
		for {
			select {
			case <-done:
				f.Close()
				return
			default:
			}

			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if isUserTurn(line) {
					turnCount++
					send(TurnCountMsg{TurnsUsed: turnCount})
				}
				if rendered := streamfilter.RenderLine(line); rendered != "" {
					send(LogLineMsg{Text: rendered})
				}
			}
			if err != nil {
				if err != io.EOF {
					// Unexpected read error; stop following this file.
					f.Close()
					return
				}
				// EOF: check for the best log file for the active stage (stage transition).
				if next := bestLogFileForStage(logDir, getActiveStage()); next != "" && next != path {
					path = next
					switchFile = true
					break readLoop
				}
				// Still at EOF; sleep and retry.
				select {
				case <-done:
					f.Close()
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		}

		f.Close()
		if !switchFile {
			return
		}
	}
}

// isUserTurn returns true if line is a JSON object with type == "user".
func isUserTurn(line []byte) bool {
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &envelope) == nil && envelope.Type == "user"
}

// pollForNewLogFile polls logDir every 2 seconds. When it observes a .log
// file that wasn't there on the previous poll, it sends NewLogFileMsg.
func pollForNewLogFile(logDir string, send func(tea.Msg), done <-chan struct{}) {
	known := make(map[string]struct{})
	// Seed with files that already exist so we don't spam on startup.
	if entries, err := os.ReadDir(logDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
				known[e.Name()] = struct{}{}
			}
		}
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			entries, err := os.ReadDir(logDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
					continue
				}
				if _, seen := known[e.Name()]; !seen {
					known[e.Name()] = struct{}{}
					send(NewLogFileMsg{Path: filepath.Join(logDir, e.Name())})
				}
			}
		}
	}
}
