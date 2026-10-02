package gate

import (
	"os"
	"testing"
)

func TestIsTSanForkCrash(t *testing.T) {
	cases := []struct {
		file string
		want bool
		sig  string
	}{
		{"tsan-abort.log", true, "tsan-abort"},
		{"git-segfault.log", true, "git-segfault"},
		{"ordinary-failure.log", false, ""},
		{"engine-panic.log", false, ""},
		{"segfault-and-panic.log", false, "git-segfault"}, // matches a form, but the panic vetoes it
		{"bare-sigsegv.log", false, ""},
	}
	for _, c := range cases {
		b, err := os.ReadFile("testdata/pregate/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := IsTSanForkCrash(string(b)); got != c.want {
			t.Errorf("%s: IsTSanForkCrash = %v, want %v", c.file, got, c.want)
		}
		if got := crashSignature(string(b)); got != c.sig {
			t.Errorf("%s: crashSignature = %q, want %q", c.file, got, c.sig)
		}
	}
}

// A plain mention of SIGSEGV or a fatal error — without the narrow shape — must
// never match: the signature is not "any crash".
func TestIsTSanForkCrashIsNarrow(t *testing.T) {
	for _, s := range []string{
		"", "FAIL", "signal: segmentation fault",
		"fatal error: all goroutines are asleep - deadlock!",
		"SIGSEGV", "ThreadSanitizer: data race",
		"exit status 66",
		"WARNING: DATA RACE\nCHECK failed: tsan_rtl.cpp:94",
	} {
		if IsTSanForkCrash(s) {
			t.Errorf("%q must not match", s)
		}
	}
}
