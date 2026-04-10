package watch

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTabClickIdx_NoTabs verifies -1 is returned when there are no tabs.
func TestTabClickIdx_NoTabs(t *testing.T) {
	m := WatchModel{}
	if got := m.tabClickIdx(0); got != -1 {
		t.Errorf("tabClickIdx with no tabs = %d, want -1", got)
	}
}

// TestTabClickIdx_SingleTab verifies clicking within the tab's X range returns 0.
func TestTabClickIdx_SingleTab(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{{Label: "Research", IsLive: false}},
	}
	// Tab renders as "[ Research ]" styled with dimStyle. Width should be > 0.
	// Click at X=0 (start of tab) should hit the tab.
	if got := m.tabClickIdx(0); got != 0 {
		t.Errorf("tabClickIdx(0) with single tab = %d, want 0", got)
	}
}

// TestTabClickIdx_BeyondAllTabs verifies -1 is returned when clicking past the
// last tab's X boundary.
func TestTabClickIdx_BeyondAllTabs(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{{Label: "Research", IsLive: false}},
	}
	// Click far to the right: should return -1.
	if got := m.tabClickIdx(10000); got != -1 {
		t.Errorf("tabClickIdx(10000) = %d, want -1", got)
	}
}

// TestTabClickIdx_SecondTab verifies clicking in the second tab's X range returns 1.
func TestTabClickIdx_SecondTab(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{
			{Label: "Research", IsLive: false},
			{Label: "Plan", IsLive: false},
		},
	}
	// First tab: "[ Research ]" → measure its width, then skip the separator.
	// We use a large X to ensure we're past the first tab.
	// Find where the second tab starts by measuring the first.
	firstRendered := dimStyle.Render("[ Research ]")
	firstW := len([]rune(firstRendered)) // rough width — lipgloss.Width more accurate but this is a test
	// Use tabClickIdx to probe: click 1 past first tab + 1 separator.
	// Instead, rely on tabClickIdx for the actual width computation.
	//
	// A simpler approach: click somewhere and verify we get the right tab or -1.
	// We know first tab starts at X=0. Test that X=0 gives tab 0.
	if got := m.tabClickIdx(0); got != 0 {
		t.Errorf("tabClickIdx(0) = %d, want 0 (first tab)", got)
	}
	_ = firstRendered
	_ = firstW

	// Find boundary: walk X until we get 1 (second tab).
	found := -1
	for x := 0; x < 80; x++ {
		if m.tabClickIdx(x) == 1 {
			found = x
			break
		}
	}
	if found == -1 {
		t.Error("could not find X that hits the second tab")
	}
}

// TestTabClickIdx_LiveTab verifies live tab (with ● prefix) is clickable.
func TestTabClickIdx_LiveTab(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{
			{Label: "Implement", IsLive: true},
		},
		selectedTabIdx: 0,
	}
	// Live tab renders as "[ ● Implement ]". Should still be clickable at X=0.
	if got := m.tabClickIdx(0); got != 0 {
		t.Errorf("tabClickIdx(0) with live tab = %d, want 0", got)
	}
}

// TestUpdate_MouseMsg_TabClick verifies a left click at the tab bar Y switches
// the selected tab.
func TestUpdate_MouseMsg_TabClick(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{
			{Label: "Research", IsLive: false},
			{Label: "Plan", IsLive: false},
		},
		selectedTabIdx: 0,
	}
	m.width = 80
	m.height = 24

	// With no labels: tabBarY = 2.
	tabBarY := 2

	// Find X that hits the second tab.
	secondTabX := -1
	for x := 0; x < 80; x++ {
		if m.tabClickIdx(x) == 1 {
			secondTabX = x
			break
		}
	}
	if secondTabX == -1 {
		t.Fatal("could not find X position for second tab")
	}

	click := tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      secondTabX,
		Y:      tabBarY,
	}
	next, _ := m.Update(click)
	nm := next.(WatchModel)
	if nm.selectedTabIdx != 1 {
		t.Errorf("selectedTabIdx = %d, want 1 after clicking second tab", nm.selectedTabIdx)
	}
}

// TestUpdate_MouseMsg_TabClick_WithLabels verifies the tab bar Y offset is
// correctly computed when labels are present (tabBarY = 3 instead of 2).
func TestUpdate_MouseMsg_TabClick_WithLabels(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{
			{Label: "Research", IsLive: false},
			{Label: "Plan", IsLive: false},
		},
		selectedTabIdx: 0,
	}
	m.width = 80
	m.height = 24
	m.github.labels = []string{"stage:Research:complete"}

	// With labels: tabBarY = 3.
	tabBarY := 3

	// Find X that hits the second tab.
	secondTabX := -1
	for x := 0; x < 80; x++ {
		if m.tabClickIdx(x) == 1 {
			secondTabX = x
			break
		}
	}
	if secondTabX == -1 {
		t.Fatal("could not find X position for second tab")
	}

	click := tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      secondTabX,
		Y:      tabBarY,
	}
	next, _ := m.Update(click)
	nm := next.(WatchModel)
	if nm.selectedTabIdx != 1 {
		t.Errorf("selectedTabIdx = %d, want 1 after clicking second tab with labels", nm.selectedTabIdx)
	}
}

// TestUpdate_MouseMsg_WrongY_NoTabSwitch verifies that a left click at the wrong
// Y coordinate does not switch the selected tab.
func TestUpdate_MouseMsg_WrongY_NoTabSwitch(t *testing.T) {
	m := WatchModel{
		stageTabs: []stageTab{
			{Label: "Research", IsLive: false},
			{Label: "Plan", IsLive: false},
		},
		selectedTabIdx: 0,
	}
	m.width = 80
	m.height = 24

	// Find X that hits the second tab.
	secondTabX := -1
	for x := 0; x < 80; x++ {
		if m.tabClickIdx(x) == 1 {
			secondTabX = x
			break
		}
	}
	if secondTabX == -1 {
		t.Fatal("could not find X position for second tab")
	}

	// Click at Y=5 (not the tab bar).
	click := tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      secondTabX,
		Y:      5,
	}
	next, _ := m.Update(click)
	nm := next.(WatchModel)
	if nm.selectedTabIdx != 0 {
		t.Errorf("selectedTabIdx = %d, want 0 (unchanged) after clicking wrong Y", nm.selectedTabIdx)
	}
}

// TestUpdate_MouseMsg_NoTabs_NoOp verifies that a left click when there are no
// tabs does not panic and returns without changing state.
func TestUpdate_MouseMsg_NoTabs_NoOp(t *testing.T) {
	m := WatchModel{}
	m.width = 80
	m.height = 24

	click := tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		X:      5,
		Y:      2,
	}
	next, _ := m.Update(click)
	_ = next.(WatchModel) // should not panic
}
