package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// pluginUpgradeResultMsg is returned by upgradePluginCmd when RefreshPlugin completes.
type pluginUpgradeResultMsg struct {
	Wrote int
	Err   error
}

// upgradePluginCmd returns a tea.Cmd that calls fabrikplugin.RefreshPlugin() and
// fabrikplugin.WriteInstalledVersion() in a background goroutine and delivers
// the result as a pluginUpgradeResultMsg. It does not block the TUI event loop.
// pluginDir defaults to ".fabrik/plugin" when empty, matching RefreshPlugin's convention.
func upgradePluginCmd(pluginDir string) tea.Cmd {
	if pluginDir == "" {
		pluginDir = ".fabrik/plugin"
	}
	return func() tea.Msg {
		n, err := fabrikplugin.RefreshPlugin()
		if err != nil {
			return pluginUpgradeResultMsg{Wrote: n, Err: err}
		}
		if werr := fabrikplugin.WriteInstalledVersion(pluginDir); werr != nil {
			return pluginUpgradeResultMsg{Wrote: n, Err: werr}
		}
		return pluginUpgradeResultMsg{Wrote: n, Err: nil}
	}
}

// tuiReadSessionID reads the Claude session ID for a given repo, issue and stage.
// The path logic mirrors engine.ReadSessionID — keep in sync if either changes.
func tuiReadSessionID(repo string, issueNumber int, stageName string) string {
	cwd, _ := os.Getwd()
	base := filepath.Base(stageName)
	if base == "" || base == "." || base == "/" || base == string(filepath.Separator) {
		base = "default"
	}
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	var sessDir string
	if repo != "" {
		repoPart := strings.ReplaceAll(repo, "/", "-")
		sessDir = filepath.Join(cwd, ".fabrik", "sessions", repoPart, issuePart)
	} else {
		sessDir = filepath.Join(cwd, ".fabrik", "sessions", issuePart)
	}
	data, err := os.ReadFile(filepath.Join(sessDir, base+".session"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// fmtDuration formats a duration as MM:SS.
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

// fmtRateLimitCountdown returns a human-readable string describing how long
// until the rate limit resets relative to now.
func fmtRateLimitCountdown(reset time.Time, now time.Time) string {
	remaining := reset.Sub(now)
	if remaining <= 0 {
		return "soon"
	}
	secs := int(remaining.Seconds())
	if secs >= 3600 {
		return fmt.Sprintf("%dh", secs/3600)
	}
	if secs >= 60 {
		return fmt.Sprintf("%dm", secs/60)
	}
	return fmt.Sprintf("%ds", secs)
}

// fmtBannerCountdown formats the rate-limit reset time for the alert banner.
// Returns "" when reset is zero (header was absent), "Resumes soon." when reset
// is in the past, or "Resumes in Nm (HH:MM local time)." for a future reset.
func fmtBannerCountdown(reset time.Time, now time.Time) string {
	if reset.IsZero() {
		return ""
	}
	remaining := reset.Sub(now)
	if remaining <= 0 {
		return "Resumes soon."
	}
	secs := int(remaining.Seconds())
	var countdown string
	if secs >= 3600 {
		countdown = fmt.Sprintf("%dh", secs/3600)
	} else if secs >= 60 {
		countdown = fmt.Sprintf("%dm", secs/60)
	} else {
		countdown = fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("Resumes in %s (%s local time).", countdown, reset.Local().Format("15:04"))
}

// openWatchInlineCmd returns a tea.Cmd that suspends the TUI and launches
// "fabrik watch <issueNumber>" in the current terminal via tea.ExecProcess.
// The TUI is restored automatically when the user exits watch with q.
// In multi-repo mode, --owner and --repo flags are passed explicitly so the
// child process watches the correct repository.
func openWatchInlineCmd(issueNumber int, repo string) tea.Cmd {
	fabrikBin, err := os.Executable()
	if err != nil {
		fabrikBin = "fabrik"
	}
	args := []string{"watch"}
	if repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) == 2 {
			args = append(args, "--owner", parts[0], "--repo", parts[1])
		}
	}
	args = append(args, strconv.Itoa(issueNumber))
	cmd := exec.Command(fabrikBin, args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return watchExitMsg{Err: err}
	})
}

// openResumeInlineCmd returns a tea.Cmd that suspends the TUI and launches an
// interactive Claude session in the issue's worktree via tea.ExecProcess.
// worktreePath must already be verified to exist by the caller.
// If a session file exists for the given stage, --resume <id> is passed;
// otherwise a fresh session starts.
func openResumeInlineCmd(pluginDir, repo string, issueNumber int, stageName, stageModel, worktreePath string) tea.Cmd {
	args := []string{}
	sessionID := tuiReadSessionID(repo, issueNumber, stageName)
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if stageModel != "" {
		args = append(args, "--model", stageModel)
	}
	if pluginDir != "" {
		args = append(args, "--plugin-dir", pluginDir)
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = worktreePath
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return claudeResumeFinishedMsg{Err: err}
	})
}

// openAbtopInlineCmd returns a tea.Cmd that suspends the TUI and launches abtop
// inline in the current terminal via tea.ExecProcess.
// The TUI is restored automatically when the user exits abtop.
// The caller must verify abtop is in PATH before calling this function.
func openAbtopInlineCmd() tea.Cmd {
	cmd := exec.Command("abtop")
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return abtopFinishedMsg{Err: err}
	})
}

// worktreePathForIssue returns the absolute path to the issue's git worktree.
// Path: <rootDir>/.fabrik/worktrees/<owner>-<repo>/issue-N
func worktreePathForIssue(rootDir, repo string, issueNumber int) string {
	issuePart := fmt.Sprintf("issue-%d", issueNumber)
	if repo != "" {
		repoPart := strings.ReplaceAll(repo, "/", "-")
		return filepath.Join(rootDir, ".fabrik", "worktrees", repoPart, issuePart)
	}
	return filepath.Join(rootDir, ".fabrik", "worktrees", issuePart)
}

// isActiveIssue reports whether the history entry's issue is currently being
// processed (present in the active jobs map). Works correctly in multi-repo mode.
func isActiveIssue(active map[string]*activeJob, h HistoryEntry) bool {
	key := activeJobKey(h.Repo, h.IssueNumber)
	_, ok := active[key]
	return ok
}
