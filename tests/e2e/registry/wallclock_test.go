package registry

import "testing"

// Every classified test must be a live test in the registry, so a rename or a
// removal cannot leave a stale classification behind (#2417).
func TestWallclockEntriesAreRegistryTests(t *testing.T) {
	w, err := LoadWallclock()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if bad := w.CheckAgainst(reg); len(bad) != 0 {
		t.Errorf("wallclock.json names tests that are not in registry.json: %v", bad)
	}
	if len(w.Tests) < 20 {
		t.Errorf("the audit must classify about 20 tests, have %d", len(w.Tests))
	}
}

func TestDecodeWallclockRejectsBadInput(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown category": `{"version":1,"categories":{"a":"x"},"tests":[{"name":"T","dominant":"b","note":"n"}]}`,
		"duplicate":        `{"version":1,"categories":{"a":"x"},"tests":[{"name":"T","dominant":"a","note":"n"},{"name":"T","dominant":"a","note":"n"}]}`,
		"no note":          `{"version":1,"categories":{"a":"x"},"tests":[{"name":"T","dominant":"a","note":""}]}`,
		"unknown field":    `{"version":1,"categories":{},"tests":[],"extra":1}`,
	} {
		if _, err := DecodeWallclock([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
