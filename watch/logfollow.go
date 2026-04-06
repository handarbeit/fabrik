package watch

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/verveguy/fabrik/streamfilter"
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

// buildStageTabs scans logDir, groups .log files by stage label, picks the
// newest log per label, and sorts by pipeline order (stageOrder map). Logs
// whose label matches "<ParentStage>-comment-review" are grouped under the
// parent stage tab. Unknown stages are appended after known stages in
// chronological order. The globally newest log file is marked IsLive.
// Returns an empty slice if logDir is unreadable or empty.
//
// stageOrder maps stage name → pipeline order value (from Stage.Order).
// When stageOrder is nil or empty, falls back to chronological ordering.
func buildStageTabs(logDir string, stageOrder map[string]int) []stageTab {
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
			if filename > effectiveNewest[parent] {
				effectiveNewest[parent] = filename
			}
		} else {
			// Unknown parent — keep as its own tab.
			if existing, ok := effectiveNewest[label]; !ok || filename > existing {
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

	// Find the globally newest log file across all tabs and mark it as IsLive.
	newestFile := ""
	for _, t := range tabs {
		if base := filepath.Base(t.LogPath); base > newestFile {
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
// Both goroutines run until the done channel is closed.
func StartLogFollower(logDir string, send func(tea.Msg), done <-chan struct{}) {
	go followLatestLog(logDir, send, done)
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
	// Sort by name (which is <label>-<timestamp>-<nanos>.log); lexicographic
	// order gives us chronological order since timestamps are zero-padded.
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Name() < logs[j].Name()
	})
	return filepath.Join(logDir, logs[len(logs)-1].Name())
}

// followLatestLog finds the newest .log file in logDir, then follows it in
// real time (like tail -F). It blocks until done is closed. When a
// NewLogFileMsg is needed (stage transition), pollForNewLogFile handles that
// separately — this goroutine simply re-opens the new file path when
// instructed via a reload.
func followLatestLog(logDir string, send func(tea.Msg), done <-chan struct{}) {
	// Wait for the log directory and a .log file to exist.
	var currentPath string
	for {
		if currentPath == "" {
			currentPath = newestLogFile(logDir)
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

	followFile(currentPath, logDir, send, done)
}

// followFile reads from path, rendering each NDJSON line via streamfilter.
// It loops — switching to newer .log files as they appear — until done is closed.
// Using a loop instead of recursion avoids goroutine stack growth across stage transitions.
func followFile(path, logDir string, send func(tea.Msg), done <-chan struct{}) {
	for {
		f, err := os.Open(path)
		if err != nil {
			// File disappeared; wait briefly and try the next newest.
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			if next := newestLogFile(logDir); next != "" && next != path {
				path = next
			}
			continue
		}

		reader := bufio.NewReader(f)
		switchFile := false
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
				// EOF: check for a newer log file (stage transition).
				if next := newestLogFile(logDir); next != "" && next != path {
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
