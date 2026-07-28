package selfupgrade

import (
	"strconv"
	"strings"
)

// SemverGreater reports whether version a is greater than version b.
// Both versions may have a leading "v" and a trailing pre-release/build/
// pseudo-version suffix (anything from the first "-" or "+" onward), both of
// which are stripped before comparison — only the numeric MAJOR.MINOR.PATCH
// core is compared. This tolerates suffixed running versions such as
// "0.0.73+dirty", "v0.0.74-0.20260716173320-6198e8102f90+dirty" (a go
// install @main/branch pseudo-version), or "0.0.73-abc1234" (a SHA suffix) —
// all of which previously made the per-segment strconv.Atoi parse fail and
// silently return false ("not an upgrade"), see #1074.
// When the numeric cores are equal, a clean version (no suffix) is treated
// as greater than a suffixed one — this lets a daemon running a +dirty or
// pseudo-version build of the same core version upgrade to the clean
// release once one exists, instead of being stuck comparing "equal".
// Returns false (not an upgrade) on any parse error.
func SemverGreater(a, b string) bool {
	aCore, aSuffixed, aOK := semverCore(a)
	bCore, bSuffixed, bOK := semverCore(b)
	if !aOK || !bOK {
		return false
	}
	// Pad shorter slice with zeros.
	for len(aCore) < len(bCore) {
		aCore = append(aCore, 0)
	}
	for len(bCore) < len(aCore) {
		bCore = append(bCore, 0)
	}
	for i := range aCore {
		if aCore[i] != bCore[i] {
			return aCore[i] > bCore[i]
		}
	}
	// Equal numeric cores: a clean version outranks a suffixed one.
	return !aSuffixed && bSuffixed
}

// semverCore strips a leading "v" and any pre-release/build/pseudo-version
// suffix (everything from the first "-" or "+" onward), then splits the
// remaining numeric core on "." and parses each segment as an integer.
// suffixed reports whether a suffix was stripped. Returns ok=false if any
// core segment fails to parse.
func semverCore(v string) (core []int, suffixed bool, ok bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		suffixed = true
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	segs := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false, false
		}
		segs[i] = n
	}
	return segs, suffixed, true
}
