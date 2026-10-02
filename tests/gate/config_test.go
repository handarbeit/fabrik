package gate

import "testing"

func TestLoadConfigInconclusiveTunables(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	c, err := LoadConfig(get(nil), "/repo")
	if err != nil || c.InconclusiveRetries != 2 || c.InconclusiveWarn != 3 {
		t.Fatalf("defaults = %d/%d err=%v, want 2/3", c.InconclusiveRetries, c.InconclusiveWarn, err)
	}
	c, err = LoadConfig(get(map[string]string{"E2E_INCONCLUSIVE_RETRIES": "0", "E2E_INCONCLUSIVE_WARN": "7"}), "/repo")
	if err != nil || c.InconclusiveRetries != 0 || c.InconclusiveWarn != 7 {
		t.Fatalf("explicit = %d/%d err=%v, want 0/7 (0 disables retries)", c.InconclusiveRetries, c.InconclusiveWarn, err)
	}
	for _, bad := range []map[string]string{
		{"E2E_INCONCLUSIVE_RETRIES": "-1"},
		{"E2E_INCONCLUSIVE_RETRIES": "two"},
		{"E2E_INCONCLUSIVE_WARN": "x"},
	} {
		if _, err := LoadConfig(get(bad), "/repo"); err == nil {
			t.Errorf("%v must be a hard error", bad)
		}
	}
}

// #1977: the shared phase keeps the pre-#1977 caps until a measured leg justifies
// raising them; 8/4 are the values to try, opt-in through the environment.
func TestLoadConfigParallelDefaultsAreUnchangedAndOptInRaises(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := LoadConfig(get(nil), "/repo")
	if err != nil || c.Parallel != "4" || c.ParallelOn != "2" {
		t.Fatalf("defaults = %s/%s err=%v, want 4/2", c.Parallel, c.ParallelOn, err)
	}
	c, err = LoadConfig(get(map[string]string{"E2E_PARALLEL": "8", "E2E_PARALLEL_ON": "4"}), "/repo")
	if err != nil || c.Parallel != "8" || c.ParallelOn != "4" {
		t.Fatalf("opt-in = %s/%s err=%v, want 8/4", c.Parallel, c.ParallelOn, err)
	}
}
