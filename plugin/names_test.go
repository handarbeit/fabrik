package plugin

import "testing"

func TestQualify(t *testing.T) {
	if got := Qualify("myorg", "review"); got != "myorg:review" {
		t.Errorf("Qualify = %q", got)
	}
	if got := Qualify(BasePluginName, "fabrik-review"); got != BasePluginName+NamespaceSep+"fabrik-review" {
		t.Errorf("Qualify base = %q", got)
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		ref, plugin, skill string
		qualified          bool
	}{
		{"fabrik-review", "", "fabrik-review", false},
		{"fabrik:fabrik-review", "fabrik", "fabrik-review", true},
		{"a:b:c", "a", "b:c", true},
		{":x", "", "x", true},
		{"x:", "x", "", true},
		{"", "", "", false},
	}
	for _, tt := range tests {
		p, s, q := Split(tt.ref)
		if p != tt.plugin || s != tt.skill || q != tt.qualified {
			t.Errorf("Split(%q) = (%q,%q,%v), want (%q,%q,%v)", tt.ref, p, s, q, tt.plugin, tt.skill, tt.qualified)
		}
	}
}
