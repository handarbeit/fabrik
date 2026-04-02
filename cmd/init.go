package cmd

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/verveguy/fabrik/stages"
)

// runInit implements the `fabrik init` subcommand.
// It extracts the embedded default stage YAML files into .fabrik/stages/.
// Existing files are skipped unless --force is passed.
func runInit(args []string) error {
	fs_ := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs_.Bool("force", false, "Overwrite existing stage files")
	if err := fs_.Parse(args); err != nil {
		return err
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
			if _, err := os.Stat(destPath); err == nil {
				skipped++
				fmt.Printf("  skip   %s (already exists; use --force to overwrite)\n", destPath)
				continue
			}
		}
		data, err := stages.DefaultStages.ReadFile(filepath.Join("examples", entry.Name()))
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", destPath, err)
		}
		wrote++
		fmt.Printf("  write  %s\n", destPath)
	}

	fmt.Printf("fabrik init: wrote %d file(s), skipped %d file(s)\n", wrote, skipped)
	fmt.Printf("Stage configs are in %s — edit them to customize your pipeline.\n", destDir)
	return nil
}
