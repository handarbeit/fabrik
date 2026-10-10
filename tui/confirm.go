package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// confirmTimeout is how long an armed confirm waits for an answer before it
// cancels itself (#2092). Measured on TickEvent.At, so the effective timeout is
// confirmTimeout minus up to one tick.
const confirmTimeout = 10 * time.Second

// confirmKind identifies which confirm, if any, is armed.
type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmKindQuit
	confirmKindStop
	confirmKindUpgrade
	confirmKindReconcile
	confirmKindOverwrite
	confirmKindClear
)

// armedKind returns the armed confirm. The bool flags stay the source of truth
// (armConfirm keeps them mutually exclusive); the priority order only matters
// for state set directly.
func (m Model) armedKind() confirmKind {
	switch {
	case m.confirmReconcile:
		return confirmKindReconcile
	case m.confirmOverwrite:
		return confirmKindOverwrite
	case m.confirmStop && m.pendingStopRequest != nil:
		return confirmKindStop
	case m.confirmUpgrade:
		return confirmKindUpgrade
	case m.history.ConfirmClear():
		return confirmKindClear
	case m.confirmQuit:
		return confirmKindQuit
	}
	return confirmNone
}

// disarmConfirms clears every confirm flag and its attached state.
func (m *Model) disarmConfirms() {
	m.confirmQuit = false
	m.quitReconcile = false
	m.confirmStop = false
	m.pendingStopRequest = nil
	m.confirmUpgrade = false
	m.confirmReconcile = false
	m.confirmOverwrite = false
	m.overwriteTyped = ""
	m.history.SetConfirmClear(false)
}

// armConfirm arms exactly one confirm, replacing any other.
func (m *Model) armConfirm(k confirmKind) {
	m.disarmConfirms()
	switch k {
	case confirmKindQuit:
		m.confirmQuit = true
	case confirmKindStop:
		m.confirmStop = true
	case confirmKindUpgrade:
		m.confirmUpgrade = true
	case confirmKindReconcile:
		m.confirmReconcile = true
	case confirmKindOverwrite:
		m.confirmOverwrite = true
	case confirmKindClear:
		m.history.SetConfirmClear(true)
	}
}

// confirmPrompt returns the full prompt for the armed confirm, or "". It is the
// only place prompt text is built.
func (m Model) confirmPrompt() string {
	switch m.armedKind() {
	case confirmKindQuit:
		if m.quitReconcile {
			return fmt.Sprintf("Quit Fabrik to start reconciliation? %d job(s) still in progress — they will be interrupted. [q] Quit anyway [n/Esc] Cancel", m.active.ActiveCount())
		}
		return fmt.Sprintf("Quit Fabrik? %d job(s) still in progress — they will be interrupted. [q] Quit anyway [n/Esc] Cancel", m.active.ActiveCount())
	case confirmKindStop:
		return fmt.Sprintf("Stop #%d and pause? [y/N]", m.pendingStopRequest.IssueNumber)
	case confirmKindUpgrade:
		return fmt.Sprintf("Upgrade %d plugin file(s)? Active invocations pick up changes on next run. [y/N]", m.header.skillsStaleCount)
	case confirmKindReconcile:
		return reconcileStatusMsg(m.header.skillsStaleCount)
	case confirmKindOverwrite:
		p := "This will discard your customizations. Type 'OVERWRITE' to confirm. Esc cancels."
		if m.overwriteTyped != "" {
			p += " Typed: " + m.overwriteTyped
		}
		return p
	case confirmKindClear:
		return "Clear all history? [C]onfirm / [n]o"
	}
	return ""
}

// confirmSignature identifies the armed confirm and its detail. When it changes
// the timeout clock restarts; observing it (rather than stamping at every
// arming site) also covers confirms the history pane arms itself.
func (m Model) confirmSignature() string {
	k := m.armedKind()
	if k == confirmNone {
		return ""
	}
	stop := ""
	if m.pendingStopRequest != nil {
		stop = fmt.Sprintf("%s#%d", m.pendingStopRequest.Repo, m.pendingStopRequest.IssueNumber)
	}
	return fmt.Sprintf("%d|%s|%t|%s", k, stop, m.quitReconcile, m.overwriteTyped)
}

// syncConfirm restamps the timeout clock when the armed confirm changed.
func (m *Model) syncConfirm() {
	sig := m.confirmSignature()
	if sig != m.confirmSigVal {
		m.confirmSigVal = sig
		m.confirmArmedAt = m.header.now
	}
}

// expireConfirm cancels the armed confirm once it has waited confirmTimeout.
// It reports whether it cancelled one.
func (m *Model) expireConfirm(now time.Time) bool {
	if m.armedKind() == confirmNone || m.confirmArmedAt.IsZero() {
		return false
	}
	if now.Sub(m.confirmArmedAt) < confirmTimeout {
		return false
	}
	m.disarmConfirms()
	m.header.SetStatusMsg("confirmation timed out — cancelled")
	return true
}

// isConfirmAnswer reports whether key is part of answering the armed confirm.
// Any other key cancels it. ctrl+c is never an answer and never cancels: it
// stays a force quit and is handled before this is consulted.
func (m Model) isConfirmAnswer(key string) bool {
	switch m.armedKind() {
	case confirmKindStop, confirmKindUpgrade:
		switch key {
		case "y", "Y", "n", "N", "esc":
			return true
		}
	case confirmKindReconcile:
		switch key {
		case "1", "2", "3", "n", "N", "esc":
			return true
		}
	case confirmKindQuit:
		switch key {
		case "q", "n", "N", "esc":
			return true
		}
	case confirmKindClear:
		switch key {
		case "C", "n", "N", "esc":
			return true
		}
	case confirmKindOverwrite:
		switch key {
		case "esc", "backspace", "ctrl+h", "delete":
			return true
		}
		if len([]rune(key)) == 1 {
			return strings.HasPrefix(overwriteConfirmWord, m.overwriteTyped+key)
		}
	}
	return false
}

// wrapPrompt word-wraps text to at most width cells per line. Words are never
// split unless a single word is wider than width.
func wrapPrompt(text string, width int) []string {
	width = max(width, 1)
	var lines []string
	cur := ""
	flush := func() {
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
	}
	for _, word := range strings.Fields(text) {
		for lipgloss.Width(word) > width {
			// Hard-break an overlong word.
			flush()
			runes := []rune(word)
			n := 0
			for n < len(runes) && lipgloss.Width(string(runes[:n+1])) <= width {
				n++
			}
			if n == 0 {
				n = 1
			}
			lines = append(lines, string(runes[:n]))
			word = string(runes[n:])
		}
		if word == "" {
			continue
		}
		switch {
		case cur == "":
			cur = word
		case lipgloss.Width(cur)+1+lipgloss.Width(word) <= width:
			cur += " " + word
		default:
			flush()
			cur = word
		}
	}
	flush()
	return lines
}

// confirmBannerLines returns the wrapped prompt lines for the current width.
func (m Model) confirmBannerLines() []string {
	if m.width == 0 {
		return nil
	}
	p := m.confirmPrompt()
	if p == "" {
		return nil
	}
	return wrapPrompt(p, m.width-2)
}

// confirmBannerHeight is the number of lines the banner occupies; 0 when no
// confirm is armed. Model.View and updateLayout both use it.
func (m Model) confirmBannerHeight() int {
	return len(m.confirmBannerLines())
}

// viewConfirmBanner renders the armed confirm's prompt in full.
func (m Model) viewConfirmBanner() string {
	lines := m.confirmBannerLines()
	if len(lines) == 0 {
		return ""
	}
	for i, l := range lines {
		lines[i] = " " + failStyle.Render(l)
	}
	return strings.Join(lines, "\n")
}
