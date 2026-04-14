package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// ProjectConfig holds the non-secret, project-level settings read from
// .fabrik/config.yaml. All fields are optional; absent fields stay zero/nil.
type ProjectConfig struct {
	Owner         string `yaml:"owner"`
	Repo          string `yaml:"repo"`
	ProjectNum    *int   `yaml:"project"`
	OwnerType     string `yaml:"owner_type"`
	User          string `yaml:"user"`
	StagesDir     string `yaml:"stages"`
	Poll          *int   `yaml:"poll"`
	MaxConcurrent *int   `yaml:"max_concurrent"`
	MaxRetries    *int   `yaml:"max_retries"`
	Yolo          bool   `yaml:"yolo"`
	AutoUpgrade   bool   `yaml:"auto_upgrade"`
	GitSSH        bool   `yaml:"git_ssh"`
	TUI           *bool  `yaml:"tui"`
	DebugOutput   bool   `yaml:"debug_output"`
	Version       string `yaml:"version"`
}

// LoadProjectConfig reads .fabrik/config.yaml from CWD.
// If the file does not exist, a zero-value struct is returned with no error.
func LoadProjectConfig() (ProjectConfig, error) {
	data, err := os.ReadFile(".fabrik/config.yaml")
	if os.IsNotExist(err) {
		return ProjectConfig{}, nil
	}
	if err != nil {
		return ProjectConfig{}, fmt.Errorf("reading .fabrik/config.yaml: %w", err)
	}
	var pc ProjectConfig
	if err := yaml.Unmarshal(data, &pc); err != nil {
		return ProjectConfig{}, fmt.Errorf("parsing .fabrik/config.yaml: %w", err)
	}
	return pc, nil
}

// WarnIfConfigIgnored prints a warning to stderr if .fabrik/config.yaml is
// listed in .gitignore. The config file should be committed to git so it
// travels with the repo; ignoring it defeats that purpose. The warning is
// suppressed when no .git entry is present — there is no repo to leak into.
func WarnIfConfigIgnored() {
	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		return
	}
	if isInGitignore(".fabrik/config.yaml") {
		fmt.Fprintln(os.Stderr, "[warn] .fabrik/config.yaml is listed in .gitignore — this file should be committed to git so project config travels with the repo")
	}
}

// LoadDotenv loads .env from CWD if it exists, and verifies it is listed in .gitignore.
// Returns nil if no .env file is present. When no .git entry exists in CWD there is no
// git repository that could leak secrets, so the gitignore check is skipped.
func LoadDotenv() error {
	if _, err := os.Stat(".env"); os.IsNotExist(err) {
		return nil
	}

	if _, err := os.Stat(".git"); !os.IsNotExist(err) {
		// A git repo is present — enforce the gitignore safety check.
		if !isInGitignore(".env") {
			return fmt.Errorf(".env file found but not listed in .gitignore — add '.env' to .gitignore to prevent accidental token leaks")
		}
	}

	return godotenv.Load(".env")
}

// isInGitignore checks whether the given filename is covered by an entry in .gitignore.
// It accepts lines where filename appears at the end of the pattern or is followed only
// by a glob wildcard (*) or path separator (/), so that ".envrc" or ".env.example" in
// .gitignore do NOT falsely match ".env".
func isInGitignore(filename string) bool {
	f, err := os.Open(".gitignore")
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		idx := strings.Index(line, filename)
		if idx < 0 {
			continue
		}
		// Reject if the match is embedded inside a longer filename component,
		// e.g. ".envrc" or ".env.example" must not match ".env".
		rest := line[idx+len(filename):]
		if rest != "" && rest[0] != '/' && rest[0] != '*' {
			continue
		}
		return true
	}
	return false
}

// Token returns the GitHub token, preferring FABRIK_TOKEN over GITHUB_TOKEN.
func Token() string {
	if t := os.Getenv("FABRIK_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GITHUB_TOKEN")
}
