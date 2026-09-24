package cmd

import "testing"

func TestResolveRetryBackoff(t *testing.T) {
	i := func(n int) *int { return &n }
	for _, tc := range []struct {
		name    string
		env     string
		yaml    *int
		current int
		want    int
	}{
		{"nothing set keeps current (flag default)", "", nil, 60, 60},
		{"env overrides", "15", nil, 60, 15},
		{"env beats config.yaml", "20", i(40), 60, 20},
		{"config.yaml used when env unset", "", i(45), 60, 45},
		{"env zero rejected — no hot-loop floor", "0", nil, 60, 60},
		{"env negative rejected", "-5", nil, 60, 60},
		{"env non-numeric rejected", "soon", nil, 60, 60},
		{"config.yaml zero rejected", "", i(0), 60, 60},
		{"env of 1 is the allowed minimum", "1", nil, 60, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRetryBackoff(tc.env, tc.yaml, tc.current); got != tc.want {
				t.Errorf("resolveRetryBackoff(%q, %v, %d) = %d, want %d", tc.env, tc.yaml, tc.current, got, tc.want)
			}
		})
	}
}
