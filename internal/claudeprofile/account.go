// Package claudeprofile reads which Claude account a Claude Code profile
// directory is logged in as. Shared by engine and pruefer, neither of which
// imports the other.
package claudeprofile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Account returns the email address the profile in configDir is logged in as
// (oauthAccount.emailAddress in <configDir>/.claude.json), or "" on any
// failure. An empty configDir means the default profile, which Claude Code
// keeps at ~/.claude.json, not ~/.claude/.claude.json.
//
// The directory name does not determine the account: a profile directory holds
// whichever account last ran /login there, so the account has to be read from
// the file.
func Account(configDir string) string {
	path := filepath.Join(configDir, ".claude.json")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		path = filepath.Join(home, ".claude.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var parsed struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	return parsed.OAuthAccount.EmailAddress
}
