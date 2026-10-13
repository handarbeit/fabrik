package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// bannerText returns the confirm banner as plain text with whitespace
// normalized. A model with no width is rendered at 80 columns.
func bannerText(m Model) string {
	if m.width == 0 {
		m.width = 80
	}
	return strings.Join(strings.Fields(ansi.Strip(m.viewConfirmBanner())), " ")
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func press(t *testing.T, m Model, k string) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(keyMsg(k))
	return next.(Model), cmd
}

func tickAt(t *testing.T, m Model, at time.Time) Model {
	t.Helper()
	next, _ := m.Update(TickEvent{At: at})
	return next.(Model)
}

// cmdQuits reports whether cmd produces tea.QuitMsg.
func cmdQuits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

type confirmCase struct {
	name string
	// arm puts the model into the armed state and returns the prompt that must
	// then be fully visible.
	arm func(t *testing.T, m *Model) string
}

func confirmCases() []confirmCase {
	return []confirmCase{
		{"upgrade", func(t *testing.T, m *Model) string {
			m.armConfirm(confirmKindUpgrade)
			return fmt.Sprintf("Upgrade %d plugin file(s)? Active invocations pick up changes on next run. [y/N]", m.header.skillsStaleCount)
		}},
		{"reconcile", func(t *testing.T, m *Model) string {
			m.armConfirm(confirmKindReconcile)
			return reconcileStatusMsg(m.header.skillsStaleCount)
		}},
		{"stop", func(t *testing.T, m *Model) string {
			m.armConfirm(confirmKindStop)
			m.pendingStopRequest = &StopRequest{IssueNumber: 4242, StageName: "Implement"}
			return "Stop #4242 and pause? [y/N]"
		}},
		{"overwrite", func(t *testing.T, m *Model) string {
			m.armConfirm(confirmKindOverwrite)
			return "This will discard your customizations. Type 'OVERWRITE' to confirm. Esc cancels."
		}},
		{"quit", func(t *testing.T, m *Model) string {
			addActiveJob(m, 7, "", "Implement")
			m.armConfirm(confirmKindQuit)
			return "Quit Fabrik? 1 job(s) still in progress — they will be interrupted. [q] Quit anyway [n/Esc] Cancel"
		}},
		{"quit-reconcile", func(t *testing.T, m *Model) string {
			addActiveJob(m, 7, "", "Implement")
			m.armConfirm(confirmKindQuit)
			m.quitReconcile = true
			return "Quit Fabrik to start reconciliation? 1 job(s) still in progress — they will be interrupted. [q] Quit anyway [n/Esc] Cancel"
		}},
		{"clear", func(t *testing.T, m *Model) string {
			m.history.history = []HistoryEntry{{IssueNumber: 1, StageName: "Research"}}
			m.armConfirm(confirmKindClear)
			return "Clear all history? [C]onfirm / [n]o"
		}},
	}
}

type badgeCase struct {
	name   string
	stale  int
	custom bool
}

var badgeCases = []badgeCase{
	{"none", 0, false},
	{"stale", 3, false},
	{"custom", 0, true},
	{"custom+stale", 3, true},
}

// Acceptance: every confirm × badge combination shows its full prompt, answer
// keys included, at 80 and 60 columns (and 40), never wider than the terminal
// and without pushing the view past the terminal height.
func TestConfirmBanner_FullyVisible_Matrix(t *testing.T) {
	for _, bc := range badgeCases {
		for _, cc := range confirmCases() {
			for _, w := range []int{80, 60, 40} {
				t.Run(fmt.Sprintf("%s/%s/%d", bc.name, cc.name, w), func(t *testing.T) {
					redirectHistory(t)
					m := New(30, ProjectInfo{}, "", nil, nil, bc.stale, bc.custom)
					const h = 40
					next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
					m = next.(Model)
					want := cc.arm(t, &m)
					next, _ = m.Update(TickEvent{At: m.header.now}) // settle layout + re-render path
					m = next.(Model)
					m.updateLayout(false)

					view := m.View()
					plain := strings.Join(strings.Fields(ansi.Strip(view)), " ")
					want = strings.Join(strings.Fields(want), " ")
					if !strings.Contains(plain, want) {
						t.Fatalf("full prompt %q not visible in view:\n%s", want, ansi.Strip(view))
					}
					lines := strings.Split(view, "\n")
					if len(lines) > h {
						t.Errorf("view is %d lines, terminal is %d", len(lines), h)
					}
					for i, l := range lines {
						if lw := lipgloss.Width(l); lw > w {
							t.Errorf("line %d is %d wide at terminal width %d: %q", i, lw, w, ansi.Strip(l))
						}
					}
				})
			}
		}
	}
}

// The banner sits below the header; the header itself no longer carries it, so
// badges cannot truncate it.
func TestConfirmBanner_NotInHeaderStatus(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 3, true)
	m.width, m.height = 60, 30
	m.armConfirm(confirmKindReconcile)
	if m.header.statusMsg != "" {
		t.Errorf("header status should stay empty, got %q", m.header.statusMsg)
	}
}

// Acceptance: an unrelated key cancels each armed confirm, visibly, and is
// consumed (it does nothing else).
func TestConfirm_UnrelatedKeyCancels(t *testing.T) {
	for _, cc := range confirmCases() {
		for _, key := range []string{"x", "tab", "?", "w", "u", "s"} {
			t.Run(cc.name+"/"+key, func(t *testing.T) {
				redirectHistory(t)
				m := New(30, ProjectInfo{}, "", nil, nil, 2, true)
				next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
				m = next.(Model)
				cc.arm(t, &m)
				focus, help := m.focusPane, m.helpPanel

				nm, cmd := press(t, m, key)
				if nm.armedKind() != confirmNone {
					t.Fatalf("confirm still armed after %q", key)
				}
				if b := bannerText(nm); b != "" {
					t.Errorf("prompt still displayed after cancel: %q", b)
				}
				if !strings.Contains(nm.header.statusMsg, "cancelled") {
					t.Errorf("cancel should be acknowledged, statusMsg = %q", nm.header.statusMsg)
				}
				if cmd != nil {
					t.Errorf("a cancelling key must do nothing else, got cmd")
				}
				if nm.focusPane != focus || nm.helpPanel != help {
					t.Errorf("cancelling key %q must be consumed (focus/help changed)", key)
				}
				if nm.confirmUpgrade || nm.confirmReconcile || nm.confirmOverwrite || nm.confirmStop || nm.confirmQuit || nm.history.ConfirmClear() {
					t.Error("a confirm flag survived the cancel")
				}
			})
		}
	}
}

// ctrl+c stays a force quit even with a confirm armed.
func TestConfirm_CtrlCStillQuits(t *testing.T) {
	for _, cc := range confirmCases() {
		t.Run(cc.name, func(t *testing.T) {
			redirectHistory(t)
			m := New(30, ProjectInfo{}, "", nil, nil, 2, true)
			cc.arm(t, &m)
			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
			_ = next
			if !cmdQuits(cmd) {
				t.Error("ctrl+c should quit")
			}
		})
	}
}

// Acceptance: the timeout cancels each armed confirm and dismisses the prompt.
func TestConfirm_TimeoutCancels(t *testing.T) {
	for _, cc := range confirmCases() {
		t.Run(cc.name, func(t *testing.T) {
			redirectHistory(t)
			m := New(30, ProjectInfo{}, "", nil, nil, 2, true)
			next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
			m = next.(Model)
			cc.arm(t, &m)
			t0 := time.Now()
			m = tickAt(t, m, t0) // establishes the clock
			m.confirmArmedAt, m.confirmSigVal = t0, m.confirmSignature()

			m = tickAt(t, m, t0.Add(confirmTimeout-time.Second))
			if m.armedKind() == confirmNone {
				t.Fatal("confirm cancelled before the timeout")
			}
			if bannerText(m) == "" {
				t.Fatal("prompt vanished before the timeout")
			}
			m = tickAt(t, m, t0.Add(confirmTimeout))
			if m.armedKind() != confirmNone {
				t.Fatal("confirm still armed after the timeout")
			}
			if b := bannerText(m); b != "" {
				t.Errorf("prompt still displayed after timeout: %q", b)
			}
			if !strings.Contains(m.header.statusMsg, "timed out") {
				t.Errorf("timeout should be acknowledged, statusMsg = %q", m.header.statusMsg)
			}
			// The next tick clears the acknowledgement and must not revive anything.
			m = tickAt(t, m, t0.Add(confirmTimeout+time.Second))
			if m.armedKind() != confirmNone || m.header.statusMsg != "" {
				t.Errorf("state after next tick: armed=%v statusMsg=%q", m.armedKind(), m.header.statusMsg)
			}
		})
	}
}

// The timeout is stamped when the confirm is armed, through Update.
func TestConfirm_TimeoutStampedOnArm(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 2, false)
	m.width, m.height = 80, 30
	t0 := time.Now()
	m = tickAt(t, m, t0)
	m, _ = press(t, m, "u")
	if !m.confirmUpgrade {
		t.Fatal("u should arm the upgrade confirm")
	}
	if !m.confirmArmedAt.Equal(t0) {
		t.Fatalf("armedAt = %v, want %v", m.confirmArmedAt, t0)
	}
	m = tickAt(t, m, t0.Add(confirmTimeout+time.Second))
	if m.confirmUpgrade {
		t.Error("upgrade confirm should have timed out")
	}
	// A late tick after a cancel is a no-op and clears no unrelated message.
	m.header.SetStatusMsg("unrelated")
	if m.expireConfirm(t0.Add(time.Hour)) || m.header.statusMsg != "unrelated" {
		t.Error("expiry with nothing armed must not touch state")
	}
}

// A tick after the confirm was answered is a no-op.
func TestConfirm_TickAfterAnswerIsNoOp(t *testing.T) {
	stopCh := make(chan StopRequest, 1)
	m := New(30, ProjectInfo{}, "", nil, stopCh, 0, false)
	m.width, m.height = 80, 30
	addActiveJob(&m, 9, "", "Implement")
	t0 := time.Now()
	m = tickAt(t, m, t0)
	m, _ = press(t, m, "s")
	m, _ = press(t, m, "y")
	if m.header.statusMsg != "stopped #9 — paused" {
		t.Fatalf("statusMsg = %q", m.header.statusMsg)
	}
	next, _ := m.Update(TickEvent{At: t0.Add(time.Hour)})
	nm := next.(Model)
	if nm.header.statusMsg != "" {
		t.Errorf("tick should only clear the normal status, got %q", nm.header.statusMsg)
	}
}

// Re-arming or changing the detail restarts the clock.
func TestConfirm_RearmRestartsClock(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, true)
	m.width, m.height = 80, 30
	t0 := time.Now()
	m = tickAt(t, m, t0)
	m, _ = press(t, m, "u")
	m, _ = press(t, m, "2") // reconcile -> overwrite
	if !m.confirmOverwrite {
		t.Fatal("expected overwrite confirm")
	}
	m = tickAt(t, m, t0.Add(8*time.Second))
	m, _ = press(t, m, "O") // typing restarts the clock at t0+8s
	if !m.confirmOverwrite || m.overwriteTyped != "O" {
		t.Fatalf("typed %q armed=%v", m.overwriteTyped, m.confirmOverwrite)
	}
	m = tickAt(t, m, t0.Add(15*time.Second))
	if !m.confirmOverwrite {
		t.Error("typing should have restarted the timeout clock")
	}
	m = tickAt(t, m, t0.Add(19*time.Second))
	if m.confirmOverwrite || m.overwriteTyped != "" {
		t.Errorf("overwrite should have timed out and discarded input, typed=%q", m.overwriteTyped)
	}
}

// OVERWRITE: letters of the word keep it armed and are echoed; anything else cancels.
func TestConfirm_Overwrite_TypingRules(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, true)
	m.width, m.height = 80, 30
	m.armConfirm(confirmKindOverwrite)
	for _, k := range []string{"O", "V", "E"} {
		m, _ = press(t, m, k)
	}
	if !m.confirmOverwrite || m.overwriteTyped != "OVE" {
		t.Fatalf("typed %q armed=%v", m.overwriteTyped, m.confirmOverwrite)
	}
	if !strings.Contains(bannerText(m), "Typed: OVE") {
		t.Errorf("progress not echoed: %q", bannerText(m))
	}
	m, _ = press(t, m, "backspace")
	if m.overwriteTyped != "OV" {
		t.Errorf("backspace: typed %q", m.overwriteTyped)
	}
	m, cmd := press(t, m, "x") // breaks the prefix
	if m.confirmOverwrite || m.overwriteTyped != "" || cmd != nil {
		t.Errorf("non-matching key should cancel: armed=%v typed=%q", m.confirmOverwrite, m.overwriteTyped)
	}

	m.armConfirm(confirmKindOverwrite)
	m, _ = press(t, m, "o") // case-sensitive
	if m.confirmOverwrite {
		t.Error("lowercase o should cancel")
	}

	m.armConfirm(confirmKindOverwrite)
	for _, k := range strings.Split("OVERWRIT", "") {
		m, _ = press(t, m, k)
	}
	m, cmd = press(t, m, "E")
	if m.confirmOverwrite || cmd == nil {
		t.Errorf("full word should run the upgrade: armed=%v cmd=%v", m.confirmOverwrite, cmd != nil)
	}

	m.armConfirm(confirmKindOverwrite)
	m, _ = press(t, m, "esc")
	if m.confirmOverwrite {
		t.Error("esc should cancel")
	}
}

// Acceptance: 1 (and 2, 3) with no armed reconcile prompt does nothing.
func TestConfirm_DigitsDoNothingWithoutReconcile(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(m *Model)
	}{
		{"nothing armed", func(m *Model) {}},
		{"upgrade armed", func(m *Model) { m.armConfirm(confirmKindUpgrade) }},
		{"stop armed", func(m *Model) {
			m.armConfirm(confirmKindStop)
			m.pendingStopRequest = &StopRequest{IssueNumber: 1}
		}},
	} {
		for _, key := range []string{"1", "2", "3"} {
			t.Run(tc.name+"/"+key, func(t *testing.T) {
				m := New(30, ProjectInfo{}, "", nil, nil, 2, true)
				m.width, m.height = 80, 30
				addActiveJob(&m, 5, "", "Implement")
				tc.arm(&m)
				nm, cmd := press(t, m, key)
				if cmdQuits(cmd) || cmd != nil {
					t.Error("digit must not produce a command")
				}
				if nm.pendingReconcilePrompt != "" || nm.confirmOverwrite || nm.confirmQuit || nm.confirmReconcile {
					t.Errorf("digit changed state: %+v", nm)
				}
			})
		}
	}
}

// R3: [1] goes through the active-workers confirmation, like q.
func TestConfirm_Reconcile1_WithWorkers_ConfirmsQuit(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, true)
	m.width, m.height = 80, 30
	addActiveJob(&m, 5, "", "Implement")
	addActiveJob(&m, 6, "", "Review")
	m, _ = press(t, m, "u")
	m, cmd := press(t, m, "1")
	if cmd != nil {
		t.Fatal("[1] with workers active must not quit immediately")
	}
	if !m.confirmQuit || !m.quitReconcile || m.confirmReconcile {
		t.Fatalf("expected quit-for-reconcile confirm, got %+v", m)
	}
	if m.pendingReconcilePrompt != "" {
		t.Error("prompt must not be set before the quit is confirmed")
	}
	if !strings.Contains(bannerText(m), "2 job(s) still in progress") {
		t.Errorf("banner = %q", bannerText(m))
	}

	// n cancels and leaves the prompt empty.
	nm, cmd := press(t, m, "n")
	if cmd != nil || nm.armedKind() != confirmNone || nm.pendingReconcilePrompt != "" {
		t.Errorf("n should cancel cleanly: %+v", nm)
	}

	// q confirms: quits with the prompt set.
	qm, cmd := press(t, m, "q")
	if !cmdQuits(cmd) {
		t.Fatal("q should quit")
	}
	if qm.PendingReconcilePrompt() != reconcilePromptText {
		t.Errorf("pending prompt = %q", qm.PendingReconcilePrompt())
	}
}

func TestConfirm_Reconcile1_NoWorkers_QuitsLikeCtrlC(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, true)
	m.width, m.height = 80, 30
	m, _ = press(t, m, "u")
	m, cmd := press(t, m, "1")
	if !cmdQuits(cmd) {
		t.Fatal("[1] without workers should quit (as q and ctrl+c do)")
	}
	if m.PendingReconcilePrompt() != reconcilePromptText {
		t.Errorf("pending prompt = %q", m.PendingReconcilePrompt())
	}
}

// Plain q confirmation does not set the reconcile prompt.
func TestConfirm_PlainQuit_DoesNotSetReconcilePrompt(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 0, true)
	m.width, m.height = 80, 30
	addActiveJob(&m, 5, "", "Implement")
	m, _ = press(t, m, "q")
	m, cmd := press(t, m, "q")
	if !cmdQuits(cmd) || m.PendingReconcilePrompt() != "" {
		t.Errorf("quit=%v prompt=%q", cmdQuits(cmd), m.PendingReconcilePrompt())
	}
}

// Arming a second confirm replaces the first.
func TestConfirm_ArmReplacesPrevious(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 2, true)
	m.armConfirm(confirmKindUpgrade)
	m.armConfirm(confirmKindReconcile)
	if m.confirmUpgrade || !m.confirmReconcile {
		t.Errorf("upgrade=%v reconcile=%v", m.confirmUpgrade, m.confirmReconcile)
	}
	m.armConfirm(confirmKindClear)
	if m.confirmReconcile || !m.history.ConfirmClear() {
		t.Error("clear should replace reconcile")
	}
}

// A resize while armed keeps the prompt fully visible and the layout valid.
func TestConfirm_ResizeKeepsPromptVisible(t *testing.T) {
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, nil, 3, true)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(Model)
	m, _ = press(t, m, "u")
	want := strings.Join(strings.Fields(reconcileStatusMsg(3)), " ")
	for _, w := range []int{100, 60, 30, 90} {
		next, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		m = next.(Model)
		plain := strings.Join(strings.Fields(ansi.Strip(m.View())), " ")
		if !strings.Contains(plain, want) {
			t.Errorf("width %d: prompt not fully visible:\n%s", w, ansi.Strip(m.View()))
		}
		if got := len(strings.Split(m.View(), "\n")); got > 30 {
			t.Errorf("width %d: view is %d lines", w, got)
		}
	}
}

func TestWrapPrompt(t *testing.T) {
	text := "Quit now? [y/N] or [1]/[2]/[3] please"
	for w := 1; w <= 50; w++ {
		lines := wrapPrompt(text, w)
		for _, l := range lines {
			if lipgloss.Width(l) > w {
				t.Errorf("w=%d: line %q too wide", w, l)
			}
		}
		if w >= len("[1]/[2]/[3]") {
			joined := strings.Join(lines, " ")
			for _, tok := range strings.Fields(text) {
				if !strings.Contains(joined, tok) {
					t.Errorf("w=%d: token %q split or lost in %q", w, tok, lines)
				}
			}
		} else if strings.ReplaceAll(strings.Join(lines, ""), " ", "") != strings.ReplaceAll(text, " ", "") {
			t.Errorf("w=%d: content lost: %q", w, lines)
		}
	}
	if got := wrapPrompt("", 10); len(got) != 0 {
		t.Errorf("empty text: %q", got)
	}
}

func TestIsConfirmAnswer_Sets(t *testing.T) {
	m := New(30, ProjectInfo{}, "", nil, nil, 1, true)
	m.armConfirm(confirmKindReconcile)
	for _, k := range []string{"1", "2", "3", "n", "N", "esc"} {
		if !m.isConfirmAnswer(k) {
			t.Errorf("reconcile: %q should be an answer", k)
		}
	}
	for _, k := range []string{"y", "q", "u", "C"} {
		if m.isConfirmAnswer(k) {
			t.Errorf("reconcile: %q should not be an answer", k)
		}
	}
	m.armConfirm(confirmKindClear)
	if !m.isConfirmAnswer("C") || m.isConfirmAnswer("c") || m.isConfirmAnswer("y") {
		t.Error("clear answer set wrong")
	}
	m.armConfirm(confirmNone)
	if m.isConfirmAnswer("y") {
		t.Error("nothing armed: no answers")
	}
}
