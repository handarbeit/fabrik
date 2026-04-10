package cmd

import (
	"os"

	"github.com/handarbeit/fabrik/streamfilter"
)

// RunStreamFilter reads stream-json (NDJSON) or a JSON array from stdin
// and prints a human-readable summary to stdout.
func RunStreamFilter() {
	streamfilter.RunFilter(os.Stdin, os.Stdout)
}
