package claudeprofile

import (
	"reflect"
	"testing"
)

func TestEnvEntry_UnconfiguredAddsNothing(t *testing.T) {
	Reset()
	if got := EnvEntry(); got != "" {
		t.Errorf("EnvEntry = %q, want empty", got)
	}
	env := []string{"A=1", BareEnv + "=/amb"}
	if got := ApplyToEnv(env); !reflect.DeepEqual(got, env) {
		t.Errorf("ApplyToEnv changed an unconfigured env: %v", got)
	}
}

func TestEnvEntry_ResolvedOverridesAmbient(t *testing.T) {
	t.Cleanup(Reset)
	Set(Resolution{Dir: "/c"})
	if got := EnvEntry(); got != "CLAUDE_CONFIG_DIR=/c" {
		t.Errorf("EnvEntry = %q", got)
	}
	got := ApplyToEnv([]string{"A=1", BareEnv + "=/amb", "B=2"})
	want := []string{"A=1", "B=2", "CLAUDE_CONFIG_DIR=/c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyToEnv = %v, want %v", got, want)
	}
}

func TestEnvEntry_ConfiguredEmptyRemoves(t *testing.T) {
	t.Cleanup(Reset)
	Set(Resolution{})
	if got := EnvEntry(); got != BareEnv {
		t.Errorf("EnvEntry = %q, want bare sentinel", got)
	}
	got := ApplyToEnv([]string{"A=1", BareEnv + "=/amb"})
	if !reflect.DeepEqual(got, []string{"A=1"}) {
		t.Errorf("ApplyToEnv = %v", got)
	}
}

func TestCommand_CarriesResolvedProfileNotAmbient(t *testing.T) {
	t.Cleanup(Reset)
	t.Setenv(BareEnv, "/profiles/a")
	Set(Resolution{Dir: "/profiles/c"})
	cmd := Command("claude", "--version")
	n := 0
	for _, kv := range cmd.Env {
		if kv == "CLAUDE_CONFIG_DIR=/profiles/c" {
			n++
		} else if len(kv) >= len(BareEnv) && kv[:len(BareEnv)] == BareEnv {
			t.Errorf("stray entry %q", kv)
		}
	}
	if n != 1 {
		t.Errorf("want exactly one resolved entry, got %d", n)
	}
	Set(Resolution{})
	for _, kv := range Command("claude").Env {
		if len(kv) >= len(BareEnv) && kv[:len(BareEnv)] == BareEnv {
			t.Errorf("nothing resolved: ambient leaked: %q", kv)
		}
	}
}
