package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// LoadDotenv loads .env from CWD if it exists, and verifies it is listed in .gitignore.
// Returns nil if no .env file is present.
func LoadDotenv() error {
	if _, err := os.Stat(".env"); os.IsNotExist(err) {
		return nil
	}

	if !isInGitignore(".env") {
		return fmt.Errorf(".env file found but not listed in .gitignore — add '.env' to .gitignore to prevent accidental token leaks")
	}

	return godotenv.Load(".env")
}

// isInGitignore checks whether the given filename is covered by an entry in .gitignore.
// It accepts any non-comment, non-negation line that contains filename as a substring
// (e.g. ".env", "**/.env", ".env*" all match ".env").
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
		if strings.Contains(line, filename) {
			return true
		}
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
