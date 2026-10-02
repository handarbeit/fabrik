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
