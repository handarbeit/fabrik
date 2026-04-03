package cmd

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	fabrikplugin "github.com/verveguy/fabrik/plugin"
	"github.com/verveguy/fabrik/stages"
)

// runInit implements the `fabrik init` subcommand.
// It extracts the embedded default stage YAML files into .fabrik/stages/.
// Existing files are skipped unless --force is passed.
func runInit(args []string) error {
	fset := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fset.Bool("force", false, "Overwrite existing stage files")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() != 0 {
		return fmt.Errorf("init: unexpected positional arguments: %v", fset.Args())
	}

	destDir := ".fabrik/stages"
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", destDir, err)
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
		destPath := filepath.Join(destDir, entry.Name())
		if !*force {
			if _, statErr := os.Stat(destPath); statErr == nil {
				skipped++
				fmt.Printf("  skip   %s (already exists; use --force to overwrite)\n", destPath)
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
		fmt.Printf("  write  %s\n", destPath)
	}

	fmt.Printf("fabrik init: wrote %d stage file(s), skipped %d file(s)\n", wrote, skipped)
	fmt.Printf("Stage configs are in %s — edit them to customize your pipeline.\n", destDir)

	// Install the Fabrik plugin to ~/.claude/plugins/fabrik/
	if err := installPlugin(*force); err != nil {
		return fmt.Errorf("installing plugin: %w", err)
	}

	return nil
}

// installPlugin copies the embedded Fabrik plugin to ~/.claude/plugins/fabrik/.
func installPlugin(force bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("finding home directory: %w", err)
	}
	pluginDir := filepath.Join(home, ".claude", "plugins", "fabrik")

	wrote := 0
	skipped := 0
	err = fs.WalkDir(fabrikplugin.FabrikPlugin, "fabrik-plugin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Convert embedded path (fabrik-plugin/...) to dest path (~/.claude/plugins/fabrik/...)
		rel, _ := filepath.Rel("fabrik-plugin", p)
		destPath := filepath.Join(pluginDir, rel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		if !force {
			if _, statErr := os.Stat(destPath); statErr == nil {
				skipped++
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
		wrote++
		fmt.Printf("  write  %s\n", destPath)
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("fabrik init: installed plugin to %s (%d file(s), %d skipped)\n", pluginDir, wrote, skipped)

	// Check if the plugin is enabled in user settings
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if data, readErr := os.ReadFile(settingsPath); readErr == nil {
		// Simple check — don't parse JSON, just look for the plugin name
		if !bytes.Contains(data, []byte(`"fabrik"`)) {
			fmt.Printf("\nTo enable the plugin, add to %s:\n", settingsPath)
			fmt.Printf("  \"enabledPlugins\": { \"fabrik\": true }\n")
			fmt.Printf("Or run: claude /plugins\n")
		}
	}

	return nil
}
