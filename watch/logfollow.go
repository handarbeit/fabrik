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
