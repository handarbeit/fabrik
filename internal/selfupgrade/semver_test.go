package selfupgrade

import "testing"

// TestSemverGreater covers the basic semver comparison logic including the
// 0.0.2 vs 0.0.10 edge case where string comparison would produce wrong results.
func TestSemverGreater(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"v0.0.2", "v0.0.1", true},
		{"v0.0.1", "v0.0.2", false},
		{"v0.0.2", "v0.0.2", false}, // equal
		{"v0.0.10", "v0.0.2", true}, // integer comparison: 10 > 2
		{"v0.0.2", "v0.0.10", false},
		{"v1.0.0", "v0.9.9", true},
		{"v0.9.9", "v1.0.0", false},
		{"v1.2.3", "v1.2.3", false},
		{"v2.0.0", "v1.99.99", true},
		// No v prefix
		{"0.0.2", "0.0.1", true},
		{"0.0.10", "0.0.2", true},
		// Mismatched segment counts
		{"1.0", "0.9.9", true},
		// Suffixed versions (#1074): a non-numeric suffix must not defeat the
		// comparison — only the numeric MAJOR.MINOR.PATCH core is compared.
		{"v0.0.75", "0.0.73+dirty", true},                                             // +dirty release stamp on b
		{"v0.0.75", "0.0.73-abc1234", true},                                           // SHA suffix on b
		{"v0.0.75", "v0.0.72-0.20260716173320-6198e8102f90+dirty", true},              // go install @main pseudo-version on b
		{"v0.0.74-0.20260716173320-6198e8102f90+dirty", "v0.0.75", false},             // suffixed a, not greater
		{"0.0.73+dirty", "0.0.73-abc1234", false},                                     // equal numeric core, both suffixed
		{"v0.0.73-0.20260716173320-aaa+dirty", "v0.0.73-0.20260716173320-bbb", false}, // equal core, both suffixed, different sha
		{"garbage", "0.0.1", false},                                                   // non-numeric core, no panic
		{"0.0.1", "garbage", false},                                                   // non-numeric core on b, no panic
		// Equal numeric core, one suffixed: a clean version outranks a suffixed
		// one, so a daemon running +dirty/pseudo-version of the same release
		// still upgrades once the clean tag exists.
		{"v0.0.73", "v0.0.73-alpha", true},  // clean release beats a pre-release of the same core
		{"v0.0.73", "v0.0.73+dirty", true},  // clean release beats a +dirty build of the same core
		{"v0.0.73-alpha", "v0.0.73", false}, // suffixed a, clean b: a is not greater
	}
	for _, tc := range tests {
		got := SemverGreater(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("SemverGreater(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
