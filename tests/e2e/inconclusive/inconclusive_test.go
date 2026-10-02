package inconclusive

import "testing"

func TestMessageAndIsMarked(t *testing.T) {
	msg := Message("poll boundary straddled the unpause (%d members)", 3)
	if msg != "E2E-INCONCLUSIVE: poll boundary straddled the unpause (3 members)" {
		t.Fatalf("Message = %q", msg)
	}
	if !IsMarked(msg) {
		t.Fatalf("IsMarked(%q) = false", msg)
	}
	if got := Reason(msg); got != "poll boundary straddled the unpause (3 members)" {
		t.Fatalf("Reason = %q", got)
	}
}

func TestIsMarkedPrefixOnly(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"marked", "E2E-INCONCLUSIVE: x", true},
		{"marked with surrounding space", "  E2E-INCONCLUSIVE: x\n", true},
		{"mentioned mid-line", "skipping: saw E2E-INCONCLUSIVE: x earlier", false},
		{"ordinary skip", "E2E_AUTH_MODE=app not configured", false},
		{"empty", "", false},
		{"lowercase", "e2e-inconclusive: x", false},
		{"bare INCONCLUSIVE", "INCONCLUSIVE: x", false},
	}
	for _, c := range cases {
		if got := IsMarked(c.msg); got != c.want {
			t.Errorf("%s: IsMarked(%q) = %v, want %v", c.name, c.msg, got, c.want)
		}
	}
}
