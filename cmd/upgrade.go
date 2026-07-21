package cmd

import (
	"bufio"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/handarbeit/fabrik/config"
	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	fabrikplugin "github.com/handarbeit/fabrik/plugin"
)

// upgradeGitHubClient is the GitHub client used by runUpgrade. It can be
// replaced in tests to avoid real network calls.
var upgradeGitHubClient engine.GitHubClient

// upgradeReconcilePrompt is the text printed by --reconcile or the TUI [1] option.
const upgradeReconcilePrompt = "In .fabrik/plugin/, compare the on-disk plugin files against the embedded source at plugin/fabrik-workflows/ in the fabrik repo. Help me reconcile my local customizations with the new embedded version. Preserve my customizations where they don't conflict with the new behavior; flag conflicts for review."

// runUpgrade implements the `fabrik upgrade` subcommand.
// For release builds it first checks for a newer binary, downloads and
// atomically replaces it if found, then re-execs so the new binary's embedded
// skills are extracted. Dev builds skip the binary check and only refresh
// plugin skills from the current binary's embedded defaults.
func runUpgrade(args []string) error {
	fset := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	var force, reconcile bool
	fset.BoolVar(&force, "force", false, "Overwrite local plugin customizations (destructive)")
	fset.BoolVar(&reconcile, "reconcile", false, "Print a Claude Code reconciliation prompt and exit")
	fset.Usage = func() {
		fmt.Fprintf(fset.Output(), "Usage: fabrik upgrade [--force] [--reconcile]\n\n")
		fmt.Fprintf(fset.Output(), "Upgrade the Fabrik binary and refresh embedded plugin skills.\n\n")
		fmt.Fprintf(fset.Output(), "For release builds: downloads the latest binary from GitHub Releases and\n")
		fmt.Fprintf(fset.Output(), "re-execs with the updated binary. For dev builds (built from source):\n")
		fmt.Fprintf(fset.Output(), "rebuilds from origin/main rather than downloading a release binary.\n")
		fmt.Fprintf(fset.Output(), "In both cases, embedded plugin skills are refreshed on disk.\n")
		fmt.Fprintf(fset.Output(), "\nFlags:\n")
		fmt.Fprintf(fset.Output(), "  --force      Overwrite local plugin customizations (destructive)\n")
		fmt.Fprintf(fset.Output(), "  --reconcile  Print a Claude Code reconciliation prompt and exit\n")
		fmt.Fprintf(fset.Output(), "\nTo update stage YAML files with missing keys, run: fabrik refresh-stages --apply\n")
	}
	if err := fset.Parse(args); err != nil {
		return err
	}

	if reconcile {
		fmt.Println(upgradeReconcilePrompt)
		return nil
	}

	if !strings.HasPrefix(Version, "dev") {
		token := config.Token()
		client := upgradeGitHubClient
		if client == nil {
			client = gh.NewClient(token)
		}
		if err := engine.PerformReleaseUpgrade(client, Version, token, nil, func(format string, args ...any) {
			fmt.Printf(format, args...)
		}); err != nil {
			return fmt.Errorf("release upgrade: %w", err)
		}
	}

	if force {
		wrote, err := fabrikplugin.RefreshPlugin()
		if err != nil {
			return err
		}
		if err := fabrikplugin.WriteInstalledVersion(".fabrik/plugin"); err != nil {
			return fmt.Errorf("writing installed version: %w", err)
		}
		fmt.Printf("fabrik upgrade --force: overwrote %d plugin file(s) (customizations discarded)\n", wrote)
		return nil
	}

	// Default path: check three-way state before refreshing.
	customWorkflow, _, stateErr := fabrikplugin.CheckPluginState(".fabrik/plugin")
	if stateErr != nil {
		return fmt.Errorf("checking plugin state: %w", stateErr)
	}
	if customWorkflow {
		return fmt.Errorf("fabrik: local customizations detected in .fabrik/plugin/ — refusing to overwrite.\n" +
			"  Options:\n" +
			"    fabrik upgrade --force       Overwrite customizations (destructive)\n" +
			"    fabrik upgrade --reconcile   Print a Claude Code reconciliation prompt\n" +
			"    (or use the TUI 'u' key for an interactive dialog)")
	}

	wrote, err := fabrikplugin.RefreshPlugin()
	if err != nil {
		return err
	}
	if err := fabrikplugin.WriteInstalledVersion(".fabrik/plugin"); err != nil {
		return fmt.Errorf("writing installed version: %w", err)
	}
	fmt.Printf("fabrik upgrade: refreshed %d plugin file(s)\n", wrote)
	return nil
}

// checkPluginSkillsWithReader compares embedded plugin files against on-disk
// files in pluginDir. If any differ and isTTY is true, the user is prompted
// to upgrade. In non-interactive mode a warning is printed to stderr and
// execution continues. Returns nil if the directory does not exist (no
// fabrik init done yet). isTTY and r control interactive-prompt behaviour
// without requiring a real PTY.
func checkPluginSkillsWithReader(pluginDir string, isTTY bool, r io.Reader) error {
	if _, err := os.Stat(pluginDir); os.IsNotExist(err) {
		return nil
	}

	diffing, err := diffingPluginFiles(pluginDir)
	if err != nil {
		return fmt.Errorf("checking plugin files: %w", err)
	}
	if len(diffing) == 0 {
		return nil
	}

	// Three-way check: detect operator customizations before refreshing.
	// upgradeNeeded=true only when disk==installed and embedded differs (safe to refresh).
	// Migration path (installedVer absent) returns (false,false): do nothing until next cycle.
	customWorkflow, upgradeNeeded, stateErr := fabrikplugin.CheckPluginState(pluginDir)
	if stateErr != nil {
		return fmt.Errorf("checking plugin state: %w", stateErr)
	}

	if !isTTY {
		if customWorkflow {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: plugin skills have local customizations — skipping auto-refresh; run 'fabrik upgrade --force' to overwrite\n")
			return nil
		}
		if !upgradeNeeded {
			// Nothing to do: fingerprints all match (up-to-date or empty plugin dir).
			return nil
		}
		// Non-interactive (headless daemon, auto-upgrade re-exec, CI): refresh
		// silently so dev builds and auto-upgraded builds always have matching
		// plugin skills without manual intervention.
		for _, rel := range diffing {
			embeddedPath := filepath.Join("fabrik-workflows", rel)
			data, readErr := fabrikplugin.FabrikPlugin.ReadFile(embeddedPath)
			if readErr != nil {
				return fmt.Errorf("reading embedded %s: %w", embeddedPath, readErr)
			}
			destPath := filepath.Join(pluginDir, rel)
			if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
				return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
			}
			if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
				return fmt.Errorf("writing %s: %w", destPath, writeErr)
			}
		}
		if werr := fabrikplugin.WriteInstalledVersion(pluginDir); werr != nil {
			fmt.Fprintf(os.Stderr, "[upgrade] warning: writing installed version failed: %v\n", werr)
		}
		fmt.Fprintf(os.Stderr, "fabrik: auto-refreshed %d plugin file(s)\n", len(diffing))
		return nil
	}

	if customWorkflow {
		fmt.Printf("Local customizations detected in %s.\n", pluginDir)
		fmt.Printf("Use 'fabrik upgrade --force' to overwrite, or 'fabrik upgrade --reconcile' for a reconciliation prompt.\n")
		return nil
	}
	if !upgradeNeeded {
		// Nothing to do: fingerprints all match (up-to-date or empty plugin dir).
		return nil
	}

	fmt.Printf("Plugin skills on disk don't match this version of fabrik. Do you want to upgrade them? [y/N] ")
	reader := bufio.NewReader(r)
	line, _ := reader.ReadString('\n')
	answer := strings.TrimSpace(line)
	if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
		return nil
	}

	for _, rel := range diffing {
		embeddedPath := filepath.Join("fabrik-workflows", rel)
		data, readErr := fabrikplugin.FabrikPlugin.ReadFile(embeddedPath)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", embeddedPath, readErr)
		}
		destPath := filepath.Join(pluginDir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return fmt.Errorf("writing %s: %w", destPath, writeErr)
		}
	}
	if werr := fabrikplugin.WriteInstalledVersion(pluginDir); werr != nil {
		fmt.Fprintf(os.Stderr, "[upgrade] warning: writing installed version failed: %v\n", werr)
	}
	fmt.Printf("fabrik: upgraded %d plugin file(s)\n", len(diffing))
	return nil
}

// diffingPluginFiles walks the embedded FabrikPlugin FS and returns the relative
// paths (from the fabrik-workflows/ root) of files whose SHA256 differs from the
// on-disk counterpart in pluginDir. Missing on-disk files count as differing.
func diffingPluginFiles(pluginDir string) ([]string, error) {
	var diffing []string
	err := fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		embeddedData, readErr := fabrikplugin.FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", p, readErr)
		}
		embeddedSum := sha256.Sum256(embeddedData)

		diskPath := filepath.Join(pluginDir, rel)
		diskData, diskErr := os.ReadFile(diskPath)
		if os.IsNotExist(diskErr) {
			diffing = append(diffing, rel)
			return nil
		}
		if diskErr != nil {
			return fmt.Errorf("reading %s: %w", diskPath, diskErr)
		}
		diskSum := sha256.Sum256(diskData)
		if embeddedSum != diskSum {
			diffing = append(diffing, rel)
		}
		return nil
	})
	return diffing, err
}
