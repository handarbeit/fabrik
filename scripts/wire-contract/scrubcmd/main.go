// Command scrubcmd is a thin CLI wrapper around internal/wirescrub, used by
// scripts/wire-contract/record-fixtures.sh so the shell script never has to
// re-implement secret detection/redaction itself.
//
// Usage:
//
//	scrubcmd -in raw.json -out scrubbed.json   # redact, write to -out
//	scrubcmd -check -in fixture.json           # exit 1 if anything secret-shaped is found
//	scrubcmd < raw.json > scrubbed.json        # stdin/stdout, same as omitting -in/-out
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/handarbeit/fabrik/internal/wirescrub"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scrubcmd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "input file (default: stdin)")
	out := fs.String("out", "", "output file (default: stdout; ignored with -check)")
	check := fs.Bool("check", false, "only report findings and exit non-zero if any are found; writes nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var data []byte
	var err error
	if *in != "" {
		data, err = os.ReadFile(*in)
	} else {
		data, err = io.ReadAll(stdin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "scrubcmd: reading input: %v\n", err)
		return 1
	}

	if *check {
		findings := wirescrub.Findings(data)
		for _, f := range findings {
			fmt.Fprintln(stderr, f)
		}
		if len(findings) > 0 {
			fmt.Fprintf(stderr, "scrubcmd: %d secret-shaped finding(s)\n", len(findings))
			return 1
		}
		return 0
	}

	redacted := wirescrub.Redact(data)
	if *out != "" {
		if err := os.WriteFile(*out, redacted, 0o644); err != nil {
			fmt.Fprintf(stderr, "scrubcmd: writing output: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := stdout.Write(redacted); err != nil {
		fmt.Fprintf(stderr, "scrubcmd: writing output: %v\n", err)
		return 1
	}
	return 0
}
