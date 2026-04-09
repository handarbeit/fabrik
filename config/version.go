// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package config

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// InferVersion attempts to determine a version or project identifier from
// common metadata files in cwd. It checks (in order):
//  1. package.json — returns the "version" field
//  2. go.mod — returns the module name (no version field exists)
//  3. Cargo.toml — returns the "version" field under [package]
//  4. pyproject.toml — returns the "version" field under [project]
//
// Returns the first non-empty match, or "" if none is found.
func InferVersion(cwd string) string {
	if v := inferFromPackageJSON(filepath.Join(cwd, "package.json")); v != "" {
		return v
	}
	if v := inferFromGoMod(filepath.Join(cwd, "go.mod")); v != "" {
		return v
	}
	if v := inferFromTOML(filepath.Join(cwd, "Cargo.toml"), "package", "version"); v != "" {
		return v
	}
	if v := inferFromTOML(filepath.Join(cwd, "pyproject.toml"), "project", "version"); v != "" {
		return v
	}
	return ""
}

// inferFromPackageJSON reads the "version" field from a package.json file.
func inferFromPackageJSON(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Version)
}

// inferFromGoMod reads the module name from a go.mod file.
// go.mod does not have a version field; the module name is returned instead.
func inferFromGoMod(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ""
	}
	return ""
}

// inferFromTOML reads a value from a TOML file using a simple state-machine
// scanner. It looks for [section] and extracts key = "value" within it.
// No regex and no new imports are used.
func inferFromTOML(path, section, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	targetSection := "[" + section + "]"
	inSection := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Detect section header.
		if strings.HasPrefix(line, "[") {
			inSection = line == targetSection
			continue
		}

		if !inSection {
			continue
		}

		// Look for key = "value" or key = 'value'.
		eqIdx := strings.IndexByte(line, '=')
		if eqIdx < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eqIdx])
		if k != key {
			continue
		}
		v := strings.TrimSpace(line[eqIdx+1:])
		// Strip surrounding quotes.
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v
	}
	if err := scanner.Err(); err != nil {
		return ""
	}
	return ""
}
