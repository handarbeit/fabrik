package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeCfg(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLegacyHookdeck_ReadsRemovedKeys(t *testing.T) {
	writeCfg(t, "owner: o\nhookdeck_api_key_env: MY_HD_KEY\nhookdeck_webhook_secret_env: MY_HD_SECRET\n")
	l, err := LoadLegacyHookdeck()
	if err != nil {
		t.Fatal(err)
	}
	if l.APIKeyEnv != "MY_HD_KEY" || l.WebhookSecretEnv != "MY_HD_SECRET" {
		t.Errorf("got %+v", l)
	}
	if want := []string{"hookdeck_api_key_env", "hookdeck_webhook_secret_env"}; !reflect.DeepEqual(l.Present, want) {
		t.Errorf("Present = %v, want %v", l.Present, want)
	}
}

func TestLoadLegacyHookdeck_NoFileAndNoKeys(t *testing.T) {
	chdir(t, t.TempDir())
	if l, err := LoadLegacyHookdeck(); err != nil || len(l.Present) != 0 {
		t.Errorf("no file: %+v, %v", l, err)
	}
	writeCfg(t, "owner: o\n")
	if l, err := LoadLegacyHookdeck(); err != nil || len(l.Present) != 0 {
		t.Errorf("no keys: %+v, %v", l, err)
	}
}

// A leftover Hookdeck key is reported once by the dedicated notice, not as an
// unrecognised-key panel entry; a genuinely unknown key still is.
func TestUnrecognizedTopLevelKeys_LegacyHookdeckExcluded(t *testing.T) {
	writeCfg(t, "owner: o\nhookdeck_api_key_env: X\nhookdeck_webhook_secret_env: Y\nbogus_key: 1\n")
	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bogus_key"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
