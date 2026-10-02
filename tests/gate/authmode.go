package gate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveAuthModes is run.sh's resolve_auth_modes: the auth legs to run for
// E2E_AUTH_MODE=v. Lowercased and end-trimmed (not internal whitespace), exactly
// like the Go side's normalizeAuthMode in tests/e2e/auth_mode.go. Empty means
// both, "pat" then "app" (#1861).
func ResolveAuthModes(v string) ([]string, error) {
	switch mode := strings.ToLower(strings.TrimSpace(v)); mode {
	case "":
		return []string{"pat", "app"}, nil
	case "pat", "app":
		return []string{mode}, nil
	default:
		return nil, fmt.Errorf("E2E_AUTH_MODE=%q is invalid (must be pat, app, or unset for both)", v)
	}
}

func authModesInclude(modes []string, mode string) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

var appConfigKeyRE = regexp.MustCompile(`(?m)^[ \t\r\f\v]*github_app_(id|private_key_path|installation_id)[ \t\r\f\v]*:`)

// AuthModeProblems is run.sh's auth_mode_problems: one entry per unmet
// auth-mode precondition (none when all are met). Pure over the bed's files.
func AuthModeProblems(bed string, modes []string) []string {
	var problems []string
	if data, err := os.ReadFile(filepath.Join(bed, ".fabrik", "config.yaml")); err == nil && appConfigKeyRE.Match(data) {
		problems = append(problems, fmt.Sprintf("%s/.fabrik/config.yaml sets github_app_* keys — auth mode is applied per leg through .env, so these would turn every pat leg into App auth; move the values to E2E_APP_ID / E2E_APP_PRIVATE_KEY_PATH / E2E_APP_INSTALLATION_ID in %s/.env", bed, bed))
	}
	if authModesInclude(modes, "app") {
		envFile := filepath.Join(bed, ".env")
		for _, k := range []string{"E2E_APP_ID", "E2E_APP_PRIVATE_KEY_PATH", "E2E_APP_INSTALLATION_ID"} {
			if envFileLastValue(envFile, k) == "" {
				problems = append(problems, fmt.Sprintf("%s is not set in %s/.env (needed for the app auth leg)", k, bed))
			}
		}
		if keyPath := envFileLastValue(envFile, "E2E_APP_PRIVATE_KEY_PATH"); keyPath != "" {
			if !filepath.IsAbs(keyPath) {
				keyPath = bed + "/" + keyPath
			}
			if f, err := os.Open(keyPath); err != nil {
				problems = append(problems, fmt.Sprintf("E2E_APP_PRIVATE_KEY_PATH points at %s, which is not a readable file", keyPath))
			} else {
				f.Close()
			}
		}
	}
	return problems
}

// CheckAuthModePreconditions refuses (ExitPreconditionFailed) before any live
// spend when a planned auth leg cannot run.
func (g *Gate) CheckAuthModePreconditions(modes []string) error {
	problems := AuthModeProblems(g.Cfg.TestBed, modes)
	if len(problems) > 0 {
		var b strings.Builder
		b.WriteString("\n")
		b.WriteString("############################################################\n")
		fmt.Fprintf(&b, "## PRECONDITION FAILED: auth-mode legs (%s) cannot run (#1861)\n", strings.Join(modes, " "))
		b.WriteString("##\n")
		for _, p := range problems {
			fmt.Fprintf(&b, "##   %s\n", p)
		}
		b.WriteString("##\n")
		b.WriteString("## Or set E2E_AUTH_MODE=pat to run only the PAT legs.\n")
		b.WriteString("############################################################")
		return &ExitError{Code: ExitPreconditionFailed, Msg: b.String()}
	}
	g.outf("== auth-mode legs: %s ==\n", strings.Join(modes, " "))
	return nil
}
