package cmd

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	fabrikplugin "github.com/handarbeit/fabrik/plugin"
	"github.com/handarbeit/fabrik/stages"
)

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

	fmt.Printf("fabrik init: wrote %d stage file(s), skipped %d\n", wrote, skipped)

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
		fmt.Printf("  write  %s\n", destPath)
		return nil
	})
	if err != nil {
		return fmt.Errorf("extracting plugin: %w", err)
	}

	fmt.Printf("fabrik init: wrote %d plugin file(s), skipped %d\n", pluginWrote, pluginSkipped)
	fmt.Println("\nFabrik is ready. Stage configs and plugin skills are in .fabrik/")
	return nil
}
