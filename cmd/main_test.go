package cmd

import (
	"os"
	"testing"

	"github.com/handarbeit/fabrik/internal/testenv"
)

// TestMain makes the whole package hermetic against a Fabrik stage worker's
// inherited environment (#2027): a worker runs this suite with the daemon's
// App credentials, FABRIK_TOKEN, GH_TOKEN and ADR-1846 GIT_CONFIG_* entries in
// its env. Execute()-driving tests that inherited the App config attempted real
// App setup against a fake org and hung. CI has none of these variables, so the
// suite behaved differently there. Tests that need one set it with t.Setenv.
func TestMain(m *testing.M) {
	testenv.ScrubProcess()
	os.Exit(m.Run())
}
