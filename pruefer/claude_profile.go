package pruefer

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/handarbeit/fabrik/internal/claudeprofile"
)

// PrueferClaudeConfigDirEnv is Pruefer's deliberate variable for the Claude
// profile its review subprocesses run under (#2294). It is the shell-level
// spelling of claude_config_dir / -claude-config-dir, and it is never passed to
// a review subprocess (reviewEnvNeverPassthrough refuses the PRUEFER_ prefix).
const PrueferClaudeConfigDirEnv = "PRUEFER_CLAUDE_CONFIG_DIR"

// claudeProfileNames are the layer spellings the shared resolver prints for
// Pruefer, so a startup line or disagreement warning names what the operator
// actually typed rather than Fabrik's own variable and config file.
var claudeProfileNames = claudeprofile.Names{
	Flag: "-claude-config-dir",
	Env:  PrueferClaudeConfigDirEnv,
	YAML: ".pruefer/config.yaml",
}

// profileSnapshot holds the two resolver inputs that are not derivable from the
// flags, the shell environment or the YAML file at reload time: the ambient
// CLAUDE_CONFIG_DIR as it was when the process started (frozen — a reload never
// re-reads it, because by then it would be the daemon's own inheritance rather
// than what the operator's shell provided), and the .env file read as data.
//
// Neither .env name is ever exported into os.Environ() (Execute loads .env
// through config.LoadDotenvWithholding), and the resolved winner is never
// os.Setenv'd: the self-upgrade re-exec passes os.Environ() on, so an exported
// value would come back looking inherited and its provenance would be lost.
var profileSnapshot struct {
	mu       sync.Mutex
	captured bool
	ambient  string
	dotenv   map[string]string
}

// captureProfileSnapshot records the start-of-process inputs. Execute calls it
// once, before any .env is loaded.
func captureProfileSnapshot(ambient string, dotenv map[string]string) {
	profileSnapshot.mu.Lock()
	defer profileSnapshot.mu.Unlock()
	profileSnapshot.captured = true
	profileSnapshot.ambient = ambient
	profileSnapshot.dotenv = dotenv
}

// swapProfileDotenv replaces the .env data on reload and returns the previous
// data, so a reload that then fails can put it back. The ambient value stays
// frozen.
func swapProfileDotenv(dotenv map[string]string) (previous map[string]string) {
	profileSnapshot.mu.Lock()
	defer profileSnapshot.mu.Unlock()
	previous, profileSnapshot.dotenv = profileSnapshot.dotenv, dotenv
	return previous
}

// resetProfileSnapshot returns the holder to its zero value. Tests that capture
// a snapshot restore it with this in t.Cleanup and must not run in parallel.
func resetProfileSnapshot() {
	profileSnapshot.mu.Lock()
	defer profileSnapshot.mu.Unlock()
	profileSnapshot.captured = false
	profileSnapshot.ambient = ""
	profileSnapshot.dotenv = nil
}

// profileSnapshotInputs returns the ambient value and .env data. With nothing
// captured (a unit test calling LoadConfig directly) the ambient value is read
// live and there is no .env data.
func profileSnapshotInputs() (ambient string, dotenv map[string]string) {
	profileSnapshot.mu.Lock()
	defer profileSnapshot.mu.Unlock()
	if !profileSnapshot.captured {
		return os.Getenv(claudeprofile.BareEnv), nil
	}
	return profileSnapshot.ambient, profileSnapshot.dotenv
}

// resolveClaudeProfile resolves the Claude profile directory through the shared
// resolver: flag > PRUEFER_CLAUDE_CONFIG_DIR (shell) > .env > claude_config_dir
// (YAML) > bare CLAUDE_CONFIG_DIR (ambient) > unset. The shell value is read
// live: it cannot change inside a running process, and it is the true shell
// value because Execute withholds the name from the .env export.
func resolveClaudeProfile(flagVal, yamlVal string) claudeprofile.Resolution {
	ambient, dotenv := profileSnapshotInputs()
	return claudeprofile.ResolveHost(claudeprofile.Inputs{
		Flag:        flagVal,
		ShellFabrik: os.Getenv(PrueferClaudeConfigDirEnv),
		Dotenv:      dotenv,
		YAML:        yamlVal,
		Ambient:     ambient,
		Names:       claudeProfileNames,
	})
}

// formatClaudeProfileLine is the one-line statement of which account reviews
// run under. Unlike the engine's, it is printed when nothing resolved too: the
// account a review bills to should always be visible.
func formatClaudeProfileLine(res claudeprofile.Resolution) string {
	if res.Dir == "" {
		return "Claude profile: none configured — reviews use Claude Code's default profile"
	}
	return fmt.Sprintf("Claude profile %q%s (from %s)", res.Dir, emailClause(res.WinnerEmail()), res.Sources[0].Name)
}

// formatClaudeProfileDisagreement names every source that supplied a value, its
// directory and account, and the winner.
func formatClaudeProfileDisagreement(res claudeprofile.Resolution) string {
	parts := make([]string, 0, len(res.Sources))
	for _, s := range res.Sources {
		parts = append(parts, fmt.Sprintf("%s=%q%s", s.Name, s.Dir, emailClause(s.Email)))
	}
	return fmt.Sprintf("warning: Claude profile sources disagree: %s — %s wins; reviews run under %q",
		strings.Join(parts, "; "), res.Sources[0].Name, res.Dir)
}

func emailClause(email string) string {
	if email == "" {
		return ""
	}
	return " (" + email + ")"
}

// reportClaudeProfile logs the effective-account line and, when two or more
// sources disagree, the warning. showEffective is true at startup and on a
// reload where the effective directory changed; the warning is logged every
// time because a disagreement is a standing condition, not an event.
func reportClaudeProfile(tag string, res claudeprofile.Resolution, showEffective bool) {
	if showEffective {
		logf(0, tag, "%s\n", formatClaudeProfileLine(res))
	}
	if res.Disagree() {
		logf(0, tag, "%s\n", formatClaudeProfileDisagreement(res))
	}
}
