package channelevents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// writeFileAtomic writes data to path via a temp file and rename, so a crash or
// the SIGHUP exec never leaves a half-written state file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// loadJSON reads path into v. A missing file is not an error (ok=false). A
// corrupt file is moved aside to <path>.corrupt-<unix> and reported through
// quarantined, so one bad file never wedges startup (mirrors workers.json).
func loadJSON(path string, v any, now time.Time) (ok bool, quarantined string, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		dst := fmt.Sprintf("%s.corrupt-%d", path, now.Unix())
		if rerr := os.Rename(path, dst); rerr != nil {
			return false, "", fmt.Errorf("quarantining %s: %w", path, rerr)
		}
		return false, dst, nil
	}
	return true, "", nil
}

func saveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
