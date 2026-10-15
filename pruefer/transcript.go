package pruefer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transcript retention (#2303, ADR 2303). When a review's coverage is
// partial, the invocation's raw stream-json stdout is the only evidence that
// can say whether the check undercounted or the reviewer's own account was
// false, so ReviewPR keeps it under .pruefer/transcripts/ instead of letting
// it die with the review's temp directory.

// KeepTranscripts values (Config.KeepTranscripts).
const (
	KeepTranscriptsPartial = "partial"
	KeepTranscriptsAll     = "all"
	KeepTranscriptsOff     = "off"
)

// transcriptRetainCount is the number of transcript files kept; older ones
// are deleted when a new one is written. Not operator-configurable.
const transcriptRetainCount = 50

// maxTranscriptNamePart caps each sanitised owner/repo/sha component.
const maxTranscriptNamePart = 80

// transcriptNameRE matches only files saveTranscript itself wrote: a
// timestamp-first name (so lexical order is chronological), then
// owner_repo_prN_sha with an optional collision suffix.
var transcriptNameRE = regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}Z_.+_pr\d+_[A-Za-z0-9._-]+\.jsonl$`)

// transcriptMu serialises write+prune: ReviewPR runs concurrently under
// ConcurrencyCap, and two unsynchronised pruners could overshoot the cap or
// race on deleting the same file.
var transcriptMu sync.Mutex

// sanitizeTranscriptPart reduces s to a single safe path component made of
// [A-Za-z0-9.-]. It never contains "_" (the field separator) or a path
// separator and is never "." or "..".
func sanitizeTranscriptPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > maxTranscriptNamePart {
		out = out[:maxTranscriptNamePart]
	}
	out = strings.Trim(out, ".")
	if out == "" {
		return "x"
	}
	return out
}

// transcriptFileName builds the file name for one transcript.
func transcriptFileName(now time.Time, owner, repo string, pr int, headSHA string) string {
	return fmt.Sprintf("%s_%s_%s_pr%d_%s.jsonl",
		now.UTC().Format("20060102T150405.000000000Z"),
		sanitizeTranscriptPart(owner), sanitizeTranscriptPart(repo), pr, sanitizeTranscriptPart(headSHA))
}

// saveTranscript writes data (the unmodified stream-json, no header or
// metadata) to dir under a name carrying the owner, repo, PR number, head SHA
// and a timestamp, then deletes the oldest of Pruefer's own transcript files
// beyond keep. It returns the written path. The write goes to a temp name and
// is renamed, so a crash never leaves a truncated file counting toward the
// cap. A prune failure is logged and does not fail the save.
func saveTranscript(dir, owner, repo string, pr int, headSHA string, data []byte, now time.Time, keep int) (string, error) {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating transcript dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".transcript-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating transcript temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) } // best effort: the temp file is abandoned either way
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("setting transcript mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("writing transcript: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("closing transcript: %w", err)
	}

	base := transcriptFileName(now, owner, repo, pr, headSHA)
	final := filepath.Join(dir, base)
	for i := 1; ; i++ {
		// Link-then-remove gives an exclusive create: it fails if final exists,
		// so a forced re-review of the same head can never overwrite.
		err := os.Link(tmpName, final)
		if err == nil {
			cleanup()
			break
		}
		if !errors.Is(err, os.ErrExist) || i > 100 {
			cleanup()
			return "", fmt.Errorf("placing transcript: %w", err)
		}
		final = filepath.Join(dir, strings.TrimSuffix(base, ".jsonl")+fmt.Sprintf("-%d.jsonl", i))
	}

	pruneTranscripts(dir, keep)
	return final, nil
}

// pruneTranscripts deletes the oldest of Pruefer's own transcript files in dir
// beyond keep. Unrelated files and temp files are never touched; failures are
// logged and never returned.
func pruneTranscripts(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logf(0, "warn", "listing transcript dir %s for pruning: %v\n", dir, err)
		return
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && transcriptNameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			logf(0, "warn", "pruning old transcript %s: %v\n", n, err)
		}
	}
}

// maybeKeepTranscript applies the retention policy to a finished review and
// returns the saved path, or "" when nothing was kept (policy, empty
// transcript, or a write failure — which is logged, never fatal).
func maybeKeepTranscript(cfg Config, prNumber int, owner, repo, headSHA string, cov Coverage, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	switch cfg.KeepTranscripts {
	case KeepTranscriptsOff:
		return ""
	case KeepTranscriptsAll:
	default: // partial, and the empty value
		if cov.Complete {
			return ""
		}
	}
	dir := cfg.TranscriptDir
	if dir == "" {
		dir = DefaultTranscriptDir
	}
	path, err := saveTranscript(dir, owner, repo, prNumber, headSHA, data, time.Now(), transcriptRetainCount)
	if err != nil {
		logf(prNumber, "warn", "keeping review transcript for %s/%s#%d: %v\n", owner, repo, prNumber, err)
		return ""
	}
	return path
}
