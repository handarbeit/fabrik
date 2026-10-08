package engine

import "testing"

func TestSingletonCatchUpEnabled(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", true},
		{"merge", true},
		{"MERGE", true},
		{"off", false},
		{"OFF", false},
	}
	for _, c := range cases {
		e := &Engine{cfg: Config{SingletonCatchUp: c.mode}}
		if got := e.singletonCatchUpEnabled(); got != c.want {
			t.Errorf("singletonCatchUpEnabled(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}
