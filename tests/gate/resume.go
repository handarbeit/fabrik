package gate

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// maxRunRegexLen bounds the anchored -run regex --resume builds. ~44 live test
// names are ~2 KB and macOS's ARG_MAX is ~1 MB; a regex past this is a usage
// error, never a silently truncated or chunked selection.
const maxRunRegexLen = 100_000

// extractFlag removes every occurrence of a Go test flag (-name, --name, with
// "=value" or a following value) from args and returns the LAST value given, as
// the flag package would. found is false when the flag is absent.
func extractFlag(args []string, name string) (value string, rest []string, found bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-"+name || a == "--"+name:
			found = true
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-"+name+"=") || strings.HasPrefix(a, "--"+name+"="):
			found = true
			value = a[strings.IndexByte(a, '=')+1:]
		default:
			rest = append(rest, a)
		}
	}
	return value, rest, found
}

// splitTestPattern splits a -run/-skip pattern at its unbracketed slashes, as
// `go test` does: element 0 matches top-level test names, the rest subtests.
func splitTestPattern(pat string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			i++
		case '[', '(':
			depth++
		case ']', ')':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				parts = append(parts, pat[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, pat[start:])
}

// SelectedTests returns the tests of live that a `go test` invocation with args
// would run, by Go's top-level matching rules: -run's first element must match
// the name (unanchored); -skip skips a top-level test only when its pattern is a
// single element that matches (a "a/b" skip pattern only skips subtests).
// An invalid regexp is returned as an error rather than guessed at.
func SelectedTests(live []string, args []string) ([]string, error) {
	runPat, rest, hasRun := extractFlag(args, "run")
	skipPat, _, hasSkip := extractFlag(rest, "skip")
	var runRE, skipRE *regexp.Regexp
	var err error
	if hasRun && runPat != "" {
		if runRE, err = regexp.Compile(splitTestPattern(runPat)[0]); err != nil {
			return nil, fmt.Errorf("-run %q: %w", runPat, err)
		}
	}
	if hasSkip && skipPat != "" {
		if parts := splitTestPattern(skipPat); len(parts) == 1 {
			if skipRE, err = regexp.Compile(parts[0]); err != nil {
				return nil, fmt.Errorf("-skip %q: %w", skipPat, err)
			}
		}
	}
	var out []string
	for _, n := range live {
		if runRE != nil && !runRE.MatchString(n) {
			continue
		}
		if skipRE != nil && skipRE.MatchString(n) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// anchoredRun is the uncapped `^(A|B|…)$`.
func anchoredRun(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// anchoredRunRegex is `^(A|B|…)$` over names. Go test names are identifiers, so
// they need no escaping; the quoting is defensive.
func anchoredRunRegex(names []string) (string, error) {
	re := anchoredRun(names)
	if len(re) > maxRunRegexLen {
		return "", fmt.Errorf("--resume: the -run regex for %d tests is %d bytes, over the %d-byte limit", len(names), len(re), maxRunRegexLen)
	}
	return re, nil
}

// rewriteSelection replaces a cell's -run/-skip with one anchored -run over
// names, keeping every other argument (e.g. -v) in place.
func rewriteSelection(args []string, names []string) ([]string, error) {
	_, rest, _ := extractFlag(args, "run")
	_, rest, _ = extractFlag(rest, "skip")
	re, err := anchoredRunRegex(names)
	if err != nil {
		return nil, err
	}
	return cat([]string{"-run", re}, rest), nil
}

// RequiredTests maps each leg label of a plan to the live tests it requires:
// the union, over the plan's cells that share the label, of each cell's
// selection. It is taken from PlanCells' own output, never a hard-coded matrix,
// so #1975's sparse plan changes the required set with no change here. Legs are
// returned in plan order.
func RequiredTests(live []string, cells []Cell) (legs []string, required map[string][]string, err error) {
	required = map[string][]string{}
	for _, c := range cells {
		sel, err := SelectedTests(live, c.Args)
		if err != nil {
			return nil, nil, err
		}
		l := c.Label()
		if _, ok := required[l]; !ok {
			legs = append(legs, l)
			required[l] = nil
		}
		required[l] = append(required[l], sel...)
	}
	for l, ts := range required {
		sort.Strings(ts)
		required[l] = dedupSorted(ts)
	}
	return legs, required, nil
}

func dedupSorted(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ResumeCells narrows a plan to the work the ledger still needs. Per cell: the
// tests its own -run/-skip selects (which already folds in any caller-supplied
// -run, so the caller's selection is INTERSECTED with the uncovered set), minus
// those already resolved for that leg, become one anchored -run. A cell with
// nothing left is dropped, so a fully covered leg is skipped — and with it the
// bed restart. The isolated cell keeps its own, separate regex. A caller's -run
// can only ever shrink the work; the recorder credits only tests that emit a
// terminal event, so nothing outside the selection is recorded.
//
// resolved is how many selected pairs were already resolved (for reporting).
func ResumeCells(ctx context.Context, cells []Cell, live []string, ev *Evaluator) (out []Cell, resolved int, err error) {
	for _, c := range cells {
		sel, err := SelectedTests(live, c.Args)
		if err != nil {
			return nil, 0, err
		}
		var todo []string
		for _, t := range sel {
			if ev.Eval(ctx, c.Label(), t).Status.Resolved() {
				resolved++
				continue
			}
			todo = append(todo, t)
		}
		if len(todo) == 0 {
			continue
		}
		args, err := rewriteSelection(c.Args, todo)
		if err != nil {
			return nil, 0, err
		}
		c.Args = args
		out = append(out, c)
	}
	return out, resolved, nil
}

// hasSubtestFilter reports whether the caller's -run or -skip narrows to
// SUBTESTS ("TestX/case"). Such a run executes only part of a top-level test, yet
// go test still reports the parent PASS — so recording it would certify the
// whole test on a fraction of its body. The gate refuses to record (and refuses
// --resume) for it.
func hasSubtestFilter(args []string) bool {
	runPat, rest, _ := extractFlag(args, "run")
	skipPat, _, _ := extractFlag(rest, "skip")
	return len(splitTestPattern(runPat)) > 1 || len(splitTestPattern(skipPat)) > 1
}
