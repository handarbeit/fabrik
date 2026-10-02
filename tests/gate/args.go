package gate

import "strings"

// RunArgs is the parsed command line of a gate run.
type RunArgs struct {
	// Clean: the first argument was --clean (the bed is reset before the run).
	Clean bool
	// Resume: run only the (test, leg) pairs the per-SHA coverage ledger still
	// lacks a valid PASS for (#1972, ADR-1972).
	Resume bool
	// Rest is everything else, passed through to `go test`.
	Rest []string
}

// ParseRunArgs consumes the gate's own flags — --clean and --resume — only while
// they lead the command line, exactly as run.sh did for --clean, in either order.
// Everything after the first other argument is left for `go test`, so a
// `-run ... --resume` never has its flag swallowed from the middle.
func ParseRunArgs(argv []string) RunArgs {
	var a RunArgs
	i := 0
	for ; i < len(argv); i++ {
		switch argv[i] {
		case "--clean":
			a.Clean = true
		case "--resume":
			a.Resume = true
		default:
			a.Rest = append([]string(nil), argv[i:]...)
			return a
		}
	}
	return a
}

// HasRunFlag reports whether the caller supplied -run/--run (with or without
// =value). A caller-supplied -run means they are targeting specific scenarios:
// the reviewer check is skipped.
func HasRunFlag(args []string) bool {
	for _, a := range args {
		if a == "-run" || a == "--run" || strings.HasPrefix(a, "-run=") || strings.HasPrefix(a, "--run=") {
			return true
		}
	}
	return false
}
