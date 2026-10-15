package engine

import (
	"fmt"
	"io"
	"strings"

	"github.com/handarbeit/fabrik/internal/claudeprofile"
	"github.com/handarbeit/fabrik/warnings"
)

// claudeProfileWarningType is the warnings-panel type for a Claude profile
// whose sources disagree (#2293).
const claudeProfileWarningType = "claude_config_dir"

// claudeProfileSourceLabel names a layer the way the startup line reads it.
func claudeProfileSourceLabel(l claudeprofile.Layer) string {
	if l == claudeprofile.LayerAmbient {
		return string(l) + " (environment)"
	}
	return string(l)
}

// claudeProfileAccountClause is " (a@b.com)", or "" when the account is unknown.
func claudeProfileAccountClause(email string) string {
	if email == "" {
		return ""
	}
	return " (" + email + ")"
}

// claudeProfileStartupLine is the single startup line naming the effective
// Claude profile, its account when readable, and where it came from. Empty when
// nothing resolved, so the unset case stays silent. The directory name does not
// determine the account — a profile dir holds whichever account last ran /login
// there — which is why the email is read from the profile, not inferred.
func claudeProfileStartupLine(res claudeprofile.Resolution) string {
	if res.Dir == "" {
		return ""
	}
	return fmt.Sprintf("[startup] notice: Claude profile %q%s from %s",
		res.Dir, claudeProfileAccountClause(res.WinnerEmail()), claudeProfileSourceLabel(res.Winner))
}

// claudeProfileCondition is the warnings-panel entry for a disagreement between
// sources, or nil when the sources agree (or fewer than two are set). The
// fingerprint is the source/directory pairs only, so a changed account email
// does not resurface a dismissed warning.
func claudeProfileCondition(res claudeprofile.Resolution) *warnings.Condition {
	if !res.Disagree() {
		return nil
	}
	var b strings.Builder
	for _, s := range res.Sources {
		fmt.Fprintf(&b, "\n  - %s: %q%s", s.Name, s.Dir, claudeProfileAccountClause(s.Email))
	}
	winner := res.Sources[0]
	detail := fmt.Sprintf("Sources name different Claude profile directories:%s\nUsing %q%s from %s; workers run under it.",
		b.String(), winner.Dir, claudeProfileAccountClause(winner.Email), claudeProfileSourceLabel(winner.Layer))
	return &warnings.Condition{
		Type:        claudeProfileWarningType,
		Subject:     "sources",
		Level:       warnings.LevelWarning,
		Title:       "Claude profile sources disagree",
		Detail:      detail,
		Fingerprint: res.Fingerprint(),
		LogLine:     "Claude profile sources disagree — " + strings.TrimSpace(strings.ReplaceAll(detail, "\n", " ")),
	}
}

// reportClaudeProfile writes the startup line and records (or clears) the
// disagreement entry. It is the only consumer-facing output of the resolution;
// the footer and workers read the same claudeprofile.Current() value.
func reportClaudeProfile(res claudeprofile.Resolution, w io.Writer) {
	if line := claudeProfileStartupLine(res); line != "" {
		fmt.Fprintln(w, line)
	}
	var conds []warnings.Condition
	if c := claudeProfileCondition(res); c != nil {
		conds = append(conds, *c)
	}
	// The check always completes, so Reconcile also clears an earlier run's entry
	// once the sources agree (#2135).
	if err := warnings.Reconcile(w, claudeProfileWarningType, conds); err != nil {
		fmt.Fprintf(w, "[startup] warning: recording %s warning: %v\n", claudeProfileWarningType, err)
	}
}
