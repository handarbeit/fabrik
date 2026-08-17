package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestUnrecognizedTopLevelKeys_NoFile(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for absent file, got %v", got)
	}
}

func TestUnrecognizedTopLevelKeys_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(""), 0644)

	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for empty file, got %v", got)
	}
}

func TestUnrecognizedTopLevelKeys_AllRecognized(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "owner: org\nrepo: myrepo\nmax_retries: 3\n"
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yamlContent), 0644)

	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no unrecognized keys, got %v", got)
	}
}

func TestUnrecognizedTopLevelKeys_SingleUnrecognized(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "owner: org\narchive_after: 48h\n"
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yamlContent), 0644)

	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"archive_after"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestUnrecognizedTopLevelKeys_MultipleUnrecognizedSorted(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "owner: org\nreconcile_interval: 60\narchive_after: 48h\ntotally_bogus_key: 1\n"
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yamlContent), 0644)

	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"archive_after", "reconcile_interval", "totally_bogus_key"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (all unrecognized keys must be reported, sorted)", got, want)
	}
}

func TestUnrecognizedTopLevelKeys_NestedMapValuesNeverFlagged(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".fabrik"), 0755); err != nil {
		t.Fatal(err)
	}
	yamlContent := "owner: org\n" +
		"required_status_contexts:\n" +
		"  o/r:\n" +
		"    - ci\n" +
		"    - lint\n" +
		"archive_after: 48h\n"
	os.WriteFile(filepath.Join(dir, ".fabrik", "config.yaml"), []byte(yamlContent), 0644)

	got, err := UnrecognizedTopLevelKeys()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"archive_after"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (nested required_status_contexts values must never be flagged)", got, want)
	}
}

func TestKnownConfigKeys_MatchesProjectConfigTags(t *testing.T) {
	known := knownConfigKeys()
	// Spot-check a representative sample of ProjectConfig's yaml tags,
	// including the map-typed field, to confirm the reflection walk sees
	// every field kind without special-casing.
	for _, key := range []string{
		"owner", "repo", "project", "user", "max_retries", "merge_train",
		"required_status_contexts", "ghes_host",
	} {
		if !known[key] {
			t.Errorf("knownConfigKeys() missing %q — reflection walk should derive it from ProjectConfig's yaml tag", key)
		}
	}
	if known["archive_after"] {
		t.Error("knownConfigKeys() should not contain archive_after — it is CLI/env-only, no ProjectConfig field exists for it")
	}
}
