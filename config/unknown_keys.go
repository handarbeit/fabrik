package config

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnrecognizedTopLevelKeys reads .fabrik/config.yaml from CWD and returns the
// sorted list of top-level keys present in the file that have no matching
// yaml:"..." struct tag on ProjectConfig. Detection is limited to the
// document's outermost keys — values inside map- or slice-typed fields (e.g.
// RequiredStatusContexts) are never inspected, so a nested key can never be
// misreported as unrecognized.
//
// An absent or empty file returns (nil, nil), matching LoadProjectConfig's
// own no-op behavior. This is a pure, side-channel diagnostic: it does not
// affect LoadProjectConfig's return value, and a malformed file's parse
// error is reported here rather than causing this function to panic or
// silently swallow the problem.
func UnrecognizedTopLevelKeys() ([]string, error) {
	data, err := os.ReadFile(".fabrik/config.yaml")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading .fabrik/config.yaml: %w", err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing .fabrik/config.yaml: %w", err)
	}

	known := knownConfigKeys()
	var unrecognized []string
	for k := range raw {
		if !known[k] {
			unrecognized = append(unrecognized, k)
		}
	}
	sort.Strings(unrecognized)
	return unrecognized, nil
}

// knownConfigKeys reflects over ProjectConfig's fields and returns the set of
// yaml:"..." tag names declared on it. Deriving this from the struct itself
// (rather than a hand-maintained list) guarantees it can never drift from
// what LoadProjectConfig actually recognizes.
func knownConfigKeys() map[string]bool {
	t := reflect.TypeOf(ProjectConfig{})
	keys := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			keys[name] = true
		}
	}
	return keys
}
