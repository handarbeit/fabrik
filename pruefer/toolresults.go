package pruefer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Hidden tool results (#2363, ADR 2363). The Claude CLI writes the full output
// of a tool call that exceeds its inline limit to
// <profile>/projects/<encoded cwd>/<session id>/tool-results/<file>.txt and
// hands the model a 2 KB <persisted-output> preview; those files are the only
// full copy. removeReviewSessions stages the regular files of that directory
// into a per-review directory before deleting the session, and saveTranscript
// moves them beside the transcript when one is kept.

// toolResultName reduces a CLI-chosen file name to one safe path component.
func toolResultName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if out == "" {
		return "x"
	}
	return out
}

// uniqueFileIn returns a path in dir for name that does not exist yet, adding
// a numeric suffix before the extension on a collision.
func uniqueFileIn(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		p := filepath.Join(dir, cand)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free name for %s in %s", name, dir)
}

// moveRegularFile moves src (a regular file) to dst, which must not exist. It
// renames when it can and otherwise copies and deletes (the profile and the
// transcript directory may sit on different filesystems). The result is 0600.
func moveRegularFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return os.Chmod(dst, 0o600)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst) // best effort: do not leave a truncated copy
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst) // best effort, as above
		return err
	}
	return os.Remove(src)
}

// moveRegularFilesFlat moves every regular file directly inside srcDir into
// dstDir (created 0700), under a sanitised, collision-free name. Symlinks,
// directories and other non-regular entries are skipped, so nothing is followed
// out of srcDir. It returns the number moved and the first error, continuing
// after one.
func moveRegularFilesFlat(srcDir, dstDir string) (int, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var first error
	note := func(err error) {
		if first == nil {
			first = err
		}
	}
	moved := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := os.MkdirAll(dstDir, 0o700); err != nil {
			note(err)
			break
		}
		dst, err := uniqueFileIn(dstDir, toolResultName(e.Name()))
		if err != nil {
			note(err)
			continue
		}
		if err := moveRegularFile(filepath.Join(srcDir, e.Name()), dst); err != nil {
			note(err)
			continue
		}
		moved++
	}
	return moved, first
}

// relocateToolResults moves the files staged in stageDir into dst, the
// <transcript stem>.tool-results directory.
func relocateToolResults(stageDir, dst string) (int, error) {
	return moveRegularFilesFlat(stageDir, dst)
}

// stageSessionToolResults stages <sessionDir>/tool-results/* into dest. The
// session directory and its tool-results directory must be real directories,
// not symlinks.
func stageSessionToolResults(sessionDir, dest string) (int, error) {
	for _, d := range []string{sessionDir, filepath.Join(sessionDir, "tool-results")} {
		fi, err := os.Lstat(d)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0, nil
			}
			return 0, err
		}
		if !fi.IsDir() {
			return 0, nil
		}
	}
	return moveRegularFilesFlat(filepath.Join(sessionDir, "tool-results"), dest)
}
