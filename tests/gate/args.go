package gate

import "strings"

// RunArgs is the parsed command line of a gate run.
type RunArgs struct {
	// Clean: the first argument was --clean (the bed is reset before the run).
	Clean bool
	// Rest is everything else, passed through to `go test`.
	Rest []string
}

// ParseRunArgs consumes --clean — only when it is the FIRST argument, exactly as
// run.sh did — and leaves the rest for `go test`.
func ParseRunArgs(argv []string) RunArgs {
	if len(argv) > 0 && argv[0] == "--clean" {
		return RunArgs{Clean: true, Rest: append([]string(nil), argv[1:]...)}
	}
	return RunArgs{Rest: append([]string(nil), argv...)}
}

// HasRunFlag reports whether the caller supplied -run/--run (with or without
// =value). A caller-supplied -run means they are targeting specific scenarios:
// the reviewer check is skipped and no isolated leg is forced on them.
func HasRunFlag(args []string) bool {
	for _, a := range args {
		if a == "-run" || a == "--run" || strings.HasPrefix(a, "-run=") || strings.HasPrefix(a, "--run=") {
			return true
		}
	}
	return false
}
