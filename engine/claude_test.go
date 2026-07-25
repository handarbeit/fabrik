package engine

import "testing"

func TestIsDegenerateOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// True positives: bare file references.
		{"at-ref absolute path", "@/tmp/plan_comment.md", true},
		{"bare absolute path", "/var/data/foo.md", true},
		{"at-ref bare filename", "@plan.md", true},
		{"at-ref relative path", "@relative/path.md", true},

		// False positives to guard against.
		{"short prose N/A", "N/A", false},
		{"short prose TBD", "TBD", false},
		{"short sentence", "Plan is ready.", false},
		{"sentence mentioning a path", "See docs/README.md for details.", false},
		{"bare relative path, no @ or leading slash", "docs/README.md", false},
		{"empty string", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDegenerateOutput(c.in); got != c.want {
				t.Errorf("isDegenerateOutput(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestIsDegenerateOutput_MultiLineNeverFlagged confirms that a multi-line body is
// never treated as degenerate, even if its only non-blank line is a bare @-reference —
// a real plan/research body is never single-line, so this keeps the detector conservative.
func TestIsDegenerateOutput_MultiLineNeverFlagged(t *testing.T) {
	in := "Some heading\n@/tmp/x.md"
	if isDegenerateOutput(in) {
		t.Errorf("isDegenerateOutput(%q) = true, want false (multi-line body must never be flagged)", in)
	}

	in2 := "@/tmp/x.md\nmore content"
	if isDegenerateOutput(in2) {
		t.Errorf("isDegenerateOutput(%q) = true, want false (multi-line body must never be flagged)", in2)
	}
}
