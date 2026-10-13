package plugin

import "testing"

func TestQualify(t *testing.T) {
	if got := Qualify("myorg", "review"); got != "myorg:review" {
		t.Errorf("Qualify = %q", got)
	}
	if got := Qualify(BasePluginName, "review"); got != "fabrik-stages:review" {
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

func TestLegacyRef(t *testing.T) {
	tests := []struct {
		ref, want string
		ok        bool
	}{
		{"fabrik:fabrik-review", "fabrik-stages:review", true},
		{"fabrik-review", "fabrik-stages:review", true},
		{"fabrik:fabrik-specify-comment", "fabrik-stages:specify-comment", true},
		{"fabrik-validate-comment", "fabrik-stages:validate-comment", true},
		// Nothing else is mapped.
		{"local-workflows:fabrik-review", "", false},
		{"other:fabrik-review", "", false},
		{"fabrik-stages:fabrik-review", "", false},
		{"fabrik:custom", "", false},
		{"fabrik-custom", "", false},
		{"review", "", false},
		{"fabrik:review", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := LegacyRef(tt.ref)
		if got != tt.want || ok != tt.ok {
			t.Errorf("LegacyRef(%q) = (%q,%v), want (%q,%v)", tt.ref, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLegacySkillTable_CoversEveryLegacySkill(t *testing.T) {
	names := LegacySkillNames()
	if len(names) != 12 {
		t.Fatalf("legacy skill table has %d entries, want 12", len(names))
	}
	for _, old := range names {
		target, ok := LegacySkillTarget(old)
		if !ok || target == "" || target == old {
			t.Errorf("LegacySkillTarget(%q) = (%q,%v)", old, target, ok)
		}
		// Every target is a skill the current embed ships.
		found := false
		for _, s := range ShippedSkillNames() {
			if s == target {
				found = true
			}
		}
		if !found {
			t.Errorf("legacy %q maps to %q, which the embedded plugin does not ship", old, target)
		}
	}
	// And the reverse: every shipped skill has a legacy name.
	for _, s := range ShippedSkillNames() {
		if _, ok := LegacyRef(LegacyBasePluginName + NamespaceSep + legacySkillPrefix + s); !ok {
			t.Errorf("shipped skill %q has no legacy mapping", s)
		}
	}
}
