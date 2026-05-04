package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ActivePaneComponent manages the in-progress jobs pane.
type ActivePaneComponent struct {
	active         map[string]*activeJob
	activeNumToKey map[int]string
	blocked        map[string]*blockedIssue
	activeIdx      int
	focused        bool
	spinnerFrames  []string
	spinnerIdx     int
	now            time.Time
	defaultRepo    string // "owner/repo" fallback for single-repo projects where activeJob.Repo is empty
}

func (a ActivePaneComponent) Update(msg tea.Msg) (Component, tea.Cmd) {
	switch ev := msg.(type) {
	case TickEvent:
		a.now = ev.At
		a.spinnerIdx = (a.spinnerIdx + 1) % len(a.spinnerFrames)

	case JobStartedEvent:
		key := activeJobKey(ev.Repo, ev.IssueNumber)
		a.active[key] = &activeJob{
			IssueNumber: ev.IssueNumber,
			Repo:        ev.Repo,
			Title:       ev.Title,
			StageName:   ev.StageName,
			IsComment:   ev.IsComment,
			StartedAt:   ev.StartedAt,
		}
		a.activeNumToKey[ev.IssueNumber] = key
		delete(a.blocked, key)

	case IssueBlockedEvent:
		key := activeJobKey(ev.Repo, ev.IssueNumber)
		a.blocked[key] = &blockedIssue{
			IssueNumber: ev.IssueNumber,
			Repo:        ev.Repo,
			Title:       ev.Title,
			StageName:   ev.StageName,
			WaitingFor:  ev.WaitingFor,
		}

	case JobCompletedEvent:
		key := activeJobKey(ev.Repo, ev.IssueNumber)
		delete(a.active, key)
		if a.activeNumToKey[ev.IssueNumber] == key {
			delete(a.activeNumToKey, ev.IssueNumber)
		}
		if a.activeIdx >= len(a.active) && a.activeIdx > 0 {
			a.activeIdx = len(a.active) - 1
		}

	case StageChangedEvent:
		key := activeJobKey(ev.Repo, ev.Number)
		if job, ok := a.active[key]; ok {
			job.StageName = ev.NewStage
		}

	case TurnProgressEvent:
		if key, known := a.activeNumToKey[ev.IssueNumber]; known {
			if job, ok := a.active[key]; ok {
				job.TurnsUsed = ev.TurnsUsed
				job.MaxTurns = ev.MaxTurns
			}
		}

	case LogEvent:
		if ev.IssueNumber != 0 {
			if key, known := a.activeNumToKey[ev.IssueNumber]; known {
				if job, ok := a.active[key]; ok {
					job.LastTag = ev.Tag
					job.LastLine = strings.TrimRight(ev.Message, "\n")
				}
			}
		}

	case tea.KeyMsg:
		if !a.focused {
			return a, nil
		}
		switch ev.String() {
		case "up", "k":
			if a.activeIdx > 0 {
				a.activeIdx--
			}
		case "down", "j":
			keys := a.sortedActiveKeys()
			if a.activeIdx < len(keys)-1 {
				a.activeIdx++
			}
		case "l":
			keys := a.sortedActiveKeys()
			if a.activeIdx < len(keys) {
				if job, ok := a.active[keys[a.activeIdx]]; ok {
					return a, openWatchInlineCmd(job.IssueNumber, job.Repo)
				}
			}
		}
	}
	return a, nil
}

func (a ActivePaneComponent) View(width int) string {
	focusIndicator := " "
	if a.focused {
		focusIndicator = "▸"
	}
	title := activeStyle.Render(fmt.Sprintf("%s In Progress (%d)", focusIndicator, len(a.active)+len(a.blocked)))

	maxWidth := max(width-6, 20)

	var lines []string
	keys := a.sortedActiveKeys()

	spinner := a.spinnerFrames[a.spinnerIdx]
	for idx, key := range keys {
		job := a.active[key]
		elapsed := fmtDuration(a.now.Sub(job.StartedAt))
		tag := ""
		if job.LastTag != "" {
			tag = dimStyle.Render(fmt.Sprintf("[%s]", job.LastTag))
		}
		msg := ""
		if job.LastLine != "" {
			msg = job.LastLine
		}
		titleStr := ""
		if job.Title != "" {
			titleStr = dimStyle.Render(job.Title) + " "
		}
		stageDisplay := job.StageName
		if job.IsComment {
			stageDisplay += " 💬"
		}
		stagePad := 12 - lipgloss.Width(stageDisplay)
		if stagePad > 0 {
			stageDisplay += strings.Repeat(" ", stagePad)
		}
		essential := fmt.Sprintf("#%-5d %s %s %s  ", job.IssueNumber, stageDisplay, spinner, elapsed)
		remaining := maxWidth - lipgloss.Width(essential)
		badge := turnBadge(job.TurnsUsed, job.MaxTurns, remaining)
		if badge != "" {
			badge += " "
		}
		line := essential + badge + titleStr + tag + " " + msg
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		if a.focused && idx == a.activeIdx {
			line = selectedStyle.Render(line)
		}
		repo := job.Repo
		if repo == "" {
			repo = a.defaultRepo
		}
		line = injectIssueLink(line, repo, job.IssueNumber)
		lines = append(lines, line)
	}

	blockedKeys := make([]string, 0, len(a.blocked))
	for k := range a.blocked {
		blockedKeys = append(blockedKeys, k)
	}
	sort.Strings(blockedKeys)
	for _, key := range blockedKeys {
		b := a.blocked[key]
		waiting := strings.Join(b.WaitingFor, ", ")
		titleStr := ""
		if b.Title != "" {
			titleStr = dimStyle.Render(b.Title) + " "
		}
		stagePad := 12 - lipgloss.Width(b.StageName)
		stageDisplay := b.StageName
		if stagePad > 0 {
			stageDisplay += strings.Repeat(" ", stagePad)
		}
		line := fmt.Sprintf("🔒#%-4d %s waiting for: %s  %s",
			b.IssueNumber, stageDisplay, waiting, titleStr)
		if runes := []rune(line); len(runes) > maxWidth {
			line = string(runes[:maxWidth-1]) + "…"
		}
		repo := b.Repo
		if repo == "" {
			repo = a.defaultRepo
		}
		line = injectIssueLink(line, repo, b.IssueNumber)
		lines = append(lines, line)
	}

	totalLines := len(lines)
	maxLines := a.Height() - 3
	if len(lines) > maxLines && maxLines > 0 {
		start := a.activeIdx - maxLines/2
		if start < 0 {
			start = 0
		}
		if start+maxLines > len(lines) {
			start = max(len(lines)-maxLines, 0)
		}
		if start > 0 || start+maxLines < totalLines {
			maxLines--
		}
		lines = lines[start : start+maxLines]
		if start > 0 || start+maxLines < totalLines {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … %d more", totalLines-maxLines)))
		}
	}

	hint := ""
	if a.focused && len(a.active) > 0 {
		hintPlain := "  [l] watch  [enter] details  [tab] history"
		hintMax := max(maxWidth-lipgloss.Width(title), 0)
		hintRunes := []rune(hintPlain)
		for len(hintRunes) > 0 && lipgloss.Width(dimStyle.Render(string(hintRunes)+"…")) > hintMax {
			hintRunes = hintRunes[:len(hintRunes)-1]
		}
		if len(hintRunes) == len([]rune(hintPlain)) {
			hint = dimStyle.Render(hintPlain)
		} else if len(hintRunes) > 0 {
			hint = dimStyle.Render(string(hintRunes) + "…")
		}
	}
	content := title + hint + "\n" + strings.Join(lines, "\n")
	return borderStyle.Width(width - 4).Render(content)
}

func (a ActivePaneComponent) Height() int {
	// In Progress always gets the rows it needs — one row per active/blocked
	// item plus title and borders. History is sacrificed to make room.
	// No cap: if the terminal can't fit everything, the history viewport
	// truncates first and, in the extreme, the View() "… N more" fallback
	// inside Active handles anything that still can't fit.
	n := len(a.active) + len(a.blocked)
	return max(n+1, 2) + 2
}

// turnBadge returns a turn counter badge string that fits within available rune columns.
// Returns full "[N/M turns]", compact "[N/M]", or "" depending on available space.
// When maxTurns == 0 (unlimited), returns "[N turns]" / "[N]" / "".
func turnBadge(turnsUsed, maxTurns, available int) string {
	if turnsUsed <= 0 || available <= 0 {
		return ""
	}
	var full, compact string
	if maxTurns > 0 {
		full = fmt.Sprintf("[%d/%d turns]", turnsUsed, maxTurns)
		compact = fmt.Sprintf("[%d/%d]", turnsUsed, maxTurns)
	} else {
		full = fmt.Sprintf("[%d turns]", turnsUsed)
		compact = fmt.Sprintf("[%d]", turnsUsed)
	}
	switch {
	case len([]rune(full)) <= available:
		return full
	case len([]rune(compact)) <= available:
		return compact
	default:
		return ""
	}
}

// sortedActiveKeys returns job keys from the active map in sorted order.
func (a ActivePaneComponent) sortedActiveKeys() []string {
	keys := make([]string, 0, len(a.active))
	for k := range a.active {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SelectedJob returns the currently selected active job, or nil if none.
func (a ActivePaneComponent) SelectedJob() *activeJob {
	keys := a.sortedActiveKeys()
	if a.activeIdx < len(keys) {
		return a.active[keys[a.activeIdx]]
	}
	return nil
}

// SetFocused updates the focused state.
func (a *ActivePaneComponent) SetFocused(f bool) {
	a.focused = f
}

// ActiveCount returns len(active).
func (a ActivePaneComponent) ActiveCount() int {
	return len(a.active)
}

// TotalCount returns len(active) + len(blocked).
func (a ActivePaneComponent) TotalCount() int {
	return len(a.active) + len(a.blocked)
}
