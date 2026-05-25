package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const installedVersionFile = ".installed-version"

// computePluginFingerprint computes a deterministic SHA256 fingerprint over a
// set of (path, content) pairs. paths must be sorted lexicographically.
func computePluginFingerprint(entries []struct{ path, hex string }) string {
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.hex))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeEmbeddedVersion returns a deterministic fingerprint of the embedded
// FabrikPlugin FS. It is a pure function — no disk I/O beyond embed.FS reads.
func ComputeEmbeddedVersion() string {
	var entries []struct{ path, hex string }
	_ = fs.WalkDir(FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		entries = append(entries, struct{ path, hex string }{p, hex.EncodeToString(sum[:])})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return computePluginFingerprint(entries)
}

// ComputeDiskVersion computes the same fingerprint over on-disk files in
// pluginDir, skipping .installed-version. Returns ("", nil) if pluginDir does
// not exist or contains no plugin files.
func ComputeDiskVersion(pluginDir string) (string, error) {
	if _, err := os.Stat(pluginDir); os.IsNotExist(err) {
		return "", nil
	}
	var entries []struct{ path, hex string }
	err := filepath.WalkDir(pluginDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(pluginDir, p)
		// Exclude the metadata file itself so disk and embedded hashes are comparable.
		if rel == installedVersionFile || strings.HasSuffix(rel, string(filepath.Separator)+installedVersionFile) {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", p, readErr)
		}
		sum := sha256.Sum256(data)
		entries = append(entries, struct{ path, hex string }{rel, hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", err
	}
	// Return "" (no plugin files) rather than sha256("") so that a dir containing
	// only .installed-version is treated the same as a non-existent dir.
	if len(entries) == 0 {
		return "", nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return computePluginFingerprint(entries), nil
}

// WriteVersionHash writes hash to pluginDir/.installed-version.
func WriteVersionHash(pluginDir, hash string) error {
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		return fmt.Errorf("creating plugin dir: %w", err)
	}
	dest := filepath.Join(pluginDir, installedVersionFile)
	return os.WriteFile(dest, []byte(hash+"\n"), 0644)
}

// WriteInstalledVersion writes the embedded plugin fingerprint to
// pluginDir/.installed-version. Call after RefreshPlugin() to record the last
// known-good installed state.
func WriteInstalledVersion(pluginDir string) error {
	return WriteVersionHash(pluginDir, ComputeEmbeddedVersion())
}

// ReadInstalledVersion reads the hash from pluginDir/.installed-version.
// Returns ("", nil) if the file does not exist (first-run / pre-migration case).
func ReadInstalledVersion(pluginDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(pluginDir, installedVersionFile))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading installed version: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// CheckPluginState performs a three-way comparison (embedded vs installedVer vs
// disk) and determines whether the operator has local customizations or whether
// an auto-refresh is needed. It delegates to checkPluginState with the global
// KnownEmbeddedVersions list.
func CheckPluginState(pluginDir string) (customWorkflow, upgradeNeeded bool, err error) {
	return checkPluginState(pluginDir, KnownEmbeddedVersions)
}

// checkPluginState is the testable implementation of CheckPluginState.
// knownVersions is the list of all plugin fingerprints ever legitimately written
// by a release binary; it is injected so tests can supply custom lists.
//
// Migration path (installedVer absent):
//   - diskVer == ""            → no-op (empty dir is not a customization)
//   - diskVer == embeddedVer   → seed installedVer=diskVer, return (false,false)
//   - diskVer != embeddedVer   → customized pre-v0.0.64 install; do NOT seed,
//     return (customWorkflow=true, upgradeNeeded=false)
//
// Corrupted-state guard (installedVer present, disk==installed, embedded differs):
//   - installedVer in knownVersions → legitimate upgrade; return (false,true)
//   - installedVer not in knownVersions → buggy v0.0.64 migration wrote a
//     customized disk hash as installedVer; treat as custom workflow; return (true,false)
//
// Return values:
//
//	customWorkflow=true  — disk differs from installedVer; skip auto-refresh.
//	upgradeNeeded=true   — disk matches installedVer but embedded differs; auto-refresh safe.
//	both false           — no action needed.
func checkPluginState(pluginDir string, knownVersions []string) (customWorkflow, upgradeNeeded bool, err error) {
	installedVer, err := ReadInstalledVersion(pluginDir)
	if err != nil {
		return false, false, err
	}

	diskVer, err := ComputeDiskVersion(pluginDir)
	if err != nil {
		return false, false, err
	}

	embeddedVer := ComputeEmbeddedVersion()

	if installedVer == "" {
		// Migration path: .installed-version absent (pre-v0.0.64 or first run).
		if diskVer == "" {
			// Empty plugin dir — not a customization, nothing to seed.
			return false, false, nil
		}
		if diskVer == embeddedVer {
			// Pristine install: disk matches embedded. Seed normally.
			if wErr := WriteVersionHash(pluginDir, diskVer); wErr != nil {
				return false, false, fmt.Errorf("writing installed version (migration): %w", wErr)
			}
			return false, false, nil
		}
		// Disk differs from embedded: operator has pre-existing customizations.
		// Do NOT seed installedVer — doing so would corrupt it with a custom hash.
		return true, false, nil
	}

	if diskVer != installedVer {
		// Operator has customized the plugin directory since last install.
		return true, false, nil
	}

	if embeddedVer != installedVer {
		// Disk matches installedVer but embedded has changed. Before treating this
		// as a safe auto-refresh, verify that installedVer was written by a
		// legitimate release binary (i.e., it is a known embedded hash).
		// If it is not in the known list, the buggy v0.0.64 migration wrote a
		// customized disk hash as installedVer — treat as custom workflow.
		if isKnownEmbedded(installedVer, knownVersions) {
			return false, true, nil
		}
		return true, false, nil
	}

	// No-op: everything matches.
	return false, false, nil
}

// isKnownEmbedded reports whether hash appears in the known embedded versions list.
func isKnownEmbedded(hash string, knownVersions []string) bool {
	for _, v := range knownVersions {
		if v == hash {
			return true
		}
	}
	return false
}

// RefreshPlugin overwrites .fabrik/plugin/ with the embedded plugin files.
// Returns the number of files written.
func RefreshPlugin() (int, error) {
	pluginDir := ".fabrik/plugin"
	wrote := 0
	err := fs.WalkDir(FabrikPlugin, "fabrik-workflows", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel("fabrik-workflows", p)
		destPath := filepath.Join(pluginDir, rel)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		data, readErr := FabrikPlugin.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("reading embedded %s: %w", p, readErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(destPath), 0755); mkErr != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, mkErr)
		}
		if writeErr := os.WriteFile(destPath, data, 0644); writeErr != nil {
			return fmt.Errorf("writing %s: %w", destPath, writeErr)
		}
		wrote++
		return nil
	})
	return wrote, err
}
