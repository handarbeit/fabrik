package cmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	fabrikplugin "github.com/verveguy/fabrik/plugin"
	"github.com/verveguy/fabrik/stages"
)

// configYAMLTemplate is the all-commented-out template written by fabrik init.
// Required fields use placeholder comments; optional fields show their defaults.
const configYAMLTemplate = `# .fabrik/config.yaml — project-level configuration for Fabrik
# Commit this file to git so project settings travel with the repo.
# Keep secrets (FABRIK_TOKEN, GITHUB_TOKEN) in .env (gitignored).
#
# Precedence: CLI flag > shell env var > .env > .fabrik/config.yaml > built-in default

# Required: GitHub repository owner (org or username)
# owner: your-org

# Required: GitHub repository name
# repo: your-repo

# Required: GitHub project number (the number in the project URL)
# project: 1

# Required: Your GitHub username (Fabrik only processes issues assigned to you)
# user: your-github-username

# Optional settings (defaults shown):
# stages: ./.fabrik/stages      # Path to stage YAML configs directory.
# poll: 30                      # Polling interval in seconds. Lower = more responsive, higher = fewer API calls.
# max_concurrent: 5             # Max parallel Claude sessions. Tune based on your API tier capacity.
# max_retries: 3                # Max stage failures before pausing an issue (0 = unlimited retries).
# yolo: false                   # Auto-advance issues through stages without human card moves.
# auto_upgrade: false           # Self-upgrade from origin/main when idle (self-evolving workflow).
# tui: false                    # Enable the interactive TUI dashboard (requires a real terminal).
# terminal: ""                  # Terminal for TUI log viewer: terminal, iterm2, ghostty, kitty, alacritty, warp.
#                               # Auto-detected from TERM_PROGRAM if not set; set explicitly for kitty.
# debug_output: false           # Save raw Claude output to .fabrik/debug/ for diagnosing prompt issues.
# version: ""                   # Project version shown in TUI footer. Auto-inferred from package.json/go.mod if not set.
`

// writeConfigTemplate writes the .fabrik/config.yaml template.
// If stdin is a TTY and the file does not already exist, it prompts for
// required values and writes them as uncommented entries.
func writeConfigTemplate(force bool) error {
	configPath := ".fabrik/config.yaml"

	if !force {
		if _, err := os.Stat(configPath); err == nil {
			fmt.Printf("  skip   %s (already exists; use --force to overwrite)\n", configPath)
			return nil
		}
	}

	content := configYAMLTemplate

	// Interactive mode: prompt for required values when stdin is a TTY.
	if isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		owner, repo, project, user := promptRequiredValues()
		if owner != "" || repo != "" || project != "" || user != "" {
			content = buildConfigWithValues(owner, repo, project, user)
		}
	}

	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", configPath, err)
	}
	fmt.Printf("  config: %s\n", configPath)
	return nil
}

// promptRequiredValues reads owner/repo/project/user from stdin interactively.
// Returns empty strings for any field the user skips (just hits enter).
func promptRequiredValues() (owner, repo, project, user string) {
	reader := bufio.NewReader(os.Stdin)
	prompt := func(label string) string {
		fmt.Printf("  %s: ", label)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}
	fmt.Println("\nFabrik interactive setup — press Enter to skip a field and fill it in later.")
	owner = prompt("GitHub owner (org or username)")
	repo = prompt("GitHub repository name (leave blank for multi-repo)")
	project = prompt("GitHub project number")
	user = prompt("Your GitHub username")
	return
}

// buildConfigWithValues returns a config.yaml where the supplied required values
// are written as uncommented entries; optional settings remain commented out.
func buildConfigWithValues(owner, repo, project, user string) string {
	lines := strings.Split(configYAMLTemplate, "\n")
	var out []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "# owner:") && owner != "":
			out = append(out, "owner: "+owner)
		case strings.HasPrefix(line, "# repo:") && repo != "":
			out = append(out, "repo: "+repo)
		case strings.HasPrefix(line, "# project:") && project != "":
			out = append(out, "project: "+project)
		case strings.HasPrefix(line, "# user:") && user != "":
			out = append(out, "user: "+user)
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// runInit implements the `fabrik init` subcommand.
// It extracts the embedded default stage YAML files into .fabrik/stages/
// and the Fabrik plugin into .fabrik/plugin/ in the current directory.
// Existing files are skipped unless --force is passed.
func runInit(args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fset.Bool("force", false, "Overwrite existing files")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return fmt.Errorf("init: unexpected positional arguments: %v", fset.Args())
	}

	// Extract stage configs
	stagesDir := ".fabrik/stages"
	if err := os.MkdirAll(stagesDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", stagesDir, err)
	}

	entries, err := fs.ReadDir(stages.DefaultStages, "examples")
	if err != nil {
		return fmt.Errorf("reading embedded stages: %w", err)
	}

	wrote := 0
	skipped := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		destPath := filepath.Join(stagesDir, entry.Name())
		if !*force {
			if _, statErr := os.Stat(destPath); statErr == nil {
				skipped++
				continue
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("checking %s: %w", destPath, statErr)
			}
		}
		data, err := stages.DefaultStages.ReadFile(path.Join("examples", entry.Name()))
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", destPath, err)
		}
		wrote++
	}

	if skipped > 0 {
		fmt.Printf("  stages: %d written, %d skipped (use --force to overwrite)\n", wrote, skipped)
	} else {
		fmt.Printf("  stages: %d written\n", wrote)
	}

	// Extract plugin
	pluginDir := ".fabrik/plugin"
	pluginWrote := 0
	pluginSkipped := 0
	err = fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-plugin", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-plugin", p)
		destPath := filepath.Join(pluginDir, rel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		if !*force {
			if _, statErr := os.Stat(destPath); statErr == nil {
				pluginSkipped++
				return nil
			}
		}

		data, readErr := fabrikplugin.FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", p, readErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return fmt.Errorf("writing %s: %w", destPath, writeErr)
		}
		pluginWrote++
		return nil
	})
	if err != nil {
		return fmt.Errorf("extracting plugin: %w", err)
	}

	if pluginSkipped > 0 {
		fmt.Printf("  plugin: %d written, %d skipped\n", pluginWrote, pluginSkipped)
	} else {
		fmt.Printf("  plugin: %d skill files written\n", pluginWrote)
	}

	// Generate .fabrik/config.yaml template
	if err := writeConfigTemplate(*force); err != nil {
		return err
	}

	fmt.Println("\nFabrik is ready. Stage configs and plugin skills are in .fabrik/")
	fmt.Println("Edit .fabrik/config.yaml with your project settings, then run fabrik.")
	return nil
}
