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

// transcriptRetention bounds the transcript directory (#2363, ADR 2363):
// entries whose file-name timestamp is older than MaxAge are deleted, then the
// oldest go first until Pruefer's own entries total at most MaxBytes.
type transcriptRetention struct {
	MaxAge   time.Duration
	MaxBytes int64
}

// retentionFor returns cfg's retention bounds, falling back to the defaults for
// a Config that did not come from LoadConfig.
func retentionFor(cfg Config) transcriptRetention {
	r := transcriptRetention{MaxAge: cfg.TranscriptMaxAge, MaxBytes: cfg.TranscriptMaxBytes}
	if r.MaxAge <= 0 {
		r.MaxAge = DefaultTranscriptMaxAge
	}
	if r.MaxBytes <= 0 {
		r.MaxBytes = DefaultTranscriptMaxBytes
	}
	return r
}

const (
	transcriptStampLayout = "20060102T150405.000000000Z"
	transcriptStampLen    = len(transcriptStampLayout)
	toolResultsSuffix     = ".tool-results"
)

// maxTranscriptNamePart caps each sanitised owner/repo/sha component.
const maxTranscriptNamePart = 80

// transcriptNameRE matches only files saveTranscript itself wrote: a
// timestamp-first name (so lexical order is chronological), then
// owner_repo_prN_sha with an optional collision suffix.
var transcriptNameRE = regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}Z_.+_pr\d+_[A-Za-z0-9._-]+\.jsonl$`)

// transcriptToolResultsRE matches only the directories relocateToolResults
// wrote beside a transcript: the transcript's stem plus ".tool-results".
var transcriptToolResultsRE = regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}Z_.+_pr\d+_[A-Za-z0-9._-]+\.tool-results$`)

// transcriptMu serialises save+relocate+prune: ReviewPR runs concurrently
// under ConcurrencyCap, and two unsynchronised pruners could overshoot the
// bounds or race on deleting the same entry.
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
// and a timestamp. When stageDir is not empty, the files staged there (the
// review's hidden tool results, #2363) are moved into <name>.tool-results/
// beside it. It then prunes Pruefer's own entries by retention, never the one
// just written for the byte ceiling. It returns the written path. The write
// goes to a temp name and is renamed, so a crash never leaves a truncated file
// counting toward the bounds. A relocation or prune failure is logged and does
// not fail the save.
func saveTranscript(dir, owner, repo string, pr int, headSHA string, data []byte, now time.Time, ret transcriptRetention, stageDir string) (string, error) {
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
		err := placeTranscript(tmpName, final, data)
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

	stem := strings.TrimSuffix(filepath.Base(final), ".jsonl")
	if stageDir != "" {
		if n, err := relocateToolResults(stageDir, filepath.Join(dir, stem+toolResultsSuffix)); err != nil {
			logf(pr, "warn", "moving review tool results beside transcript %s: %v (moved %d)\n", stem, err, n)
		}
	}
	pruneTranscriptsLocked(dir, now, ret, stem)
	return final, nil
}

// linkFile is os.Link, a variable so a test can simulate a filesystem
// without hard-link support.
var linkFile = os.Link

// placeTranscript creates final exclusively (it fails with os.ErrExist rather
// than overwrite, so a forced re-review of the same head never clobbers an
// earlier transcript). It hard-links the already-written temp file, which is
// atomic; on a filesystem that cannot hard-link (FAT/exFAT, some SMB/FUSE
// mounts) it falls back to an O_EXCL create and write of data.
func placeTranscript(tmpName, final string, data []byte) error {
	err := linkFile(tmpName, final)
	if err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	f, err := os.OpenFile(final, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(final) // best effort: do not leave a truncated file counting toward the cap
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(final) // best effort, as above
		return err
	}
	return nil
}

// transcriptUnit is one retained review: the transcript file and/or its
// .tool-results directory, which share a stem.
type transcriptUnit struct {
	stem  string
	at    time.Time
	size  int64
	file  string // path of <stem>.jsonl, or ""
	trDir string // path of <stem>.tool-results, or ""
}

// transcriptStemTime parses the timestamp a transcript stem starts with.
func transcriptStemTime(stem string) (time.Time, bool) {
	if len(stem) < transcriptStampLen {
		return time.Time{}, false
	}
	t, err := time.Parse(transcriptStampLayout, stem[:transcriptStampLen])
	return t, err == nil
}

// dirFileBytes totals the regular files directly inside dir.
func dirFileBytes(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range entries {
		if e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
	}
	return n
}

// pruneTranscripts retires Pruefer's own transcripts and .tool-results
// directories in dir (#2363, ADR 2363): first anything whose file-name
// timestamp is older than ret.MaxAge, then, while the remaining total exceeds
// ret.MaxBytes, the oldest first. Age comes from the name, never mtime
// (ADR 2303). Names not matching Pruefer's own shapes — and symlinked
// directories — are neither counted nor deleted. protect names a stem exempt
// from the byte ceiling (the review just saved). Failures are logged and never
// returned.
func pruneTranscripts(dir string, now time.Time, ret transcriptRetention, protect string) {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	pruneTranscriptsLocked(dir, now, ret, protect)
}

// pruneTranscriptsLocked is pruneTranscripts for a caller holding transcriptMu.
func pruneTranscriptsLocked(dir string, now time.Time, ret transcriptRetention, protect string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logf(0, "warn", "listing transcript dir %s for pruning: %v\n", dir, err)
		}
		return
	}
	units := map[string]*transcriptUnit{}
	unit := func(stem string) *transcriptUnit {
		u := units[stem]
		if u == nil {
			u = &transcriptUnit{stem: stem}
			units[stem] = u
		}
		return u
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.Type().IsRegular() && transcriptNameRE.MatchString(name):
			at, ok := transcriptStemTime(name)
			if !ok {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			u := unit(strings.TrimSuffix(name, ".jsonl"))
			u.at, u.file = at, filepath.Join(dir, name)
			u.size += info.Size()
		case e.Type().IsDir() && transcriptToolResultsRE.MatchString(name):
			at, ok := transcriptStemTime(name)
			if !ok {
				continue
			}
			u := unit(strings.TrimSuffix(name, toolResultsSuffix))
			u.at, u.trDir = at, filepath.Join(dir, name)
			u.size += dirFileBytes(u.trDir)
		}
	}
	list := make([]*transcriptUnit, 0, len(units))
	for _, u := range units {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].stem < list[j].stem })

	remove := func(u *transcriptUnit, why string) {
		if u.file != "" {
			if err := os.Remove(u.file); err != nil && !errors.Is(err, os.ErrNotExist) {
				logf(0, "warn", "pruning transcript %s (%s): %v\n", u.stem, why, err)
			}
		}
		if u.trDir != "" {
			if err := os.RemoveAll(u.trDir); err != nil {
				logf(0, "warn", "pruning transcript tool results %s (%s): %v\n", u.stem, why, err)
			}
		}
	}
	var total int64
	kept := list[:0:0]
	for _, u := range list {
		if now.Sub(u.at) > ret.MaxAge {
			remove(u, "older than "+ret.MaxAge.String())
			continue
		}
		total += u.size
		kept = append(kept, u)
	}
	for _, u := range kept {
		if total <= ret.MaxBytes {
			break
		}
		if u.stem == protect {
			continue
		}
		remove(u, "over the size ceiling")
		total -= u.size
	}
}

// pruneTranscriptsAtStartup runs the one startup prune with cfg's bounds
// (#2363). It is a no-op when the directory does not exist yet.
func pruneTranscriptsAtStartup(cfg Config, now time.Time) {
	dir := cfg.TranscriptDir
	if dir == "" {
		dir = DefaultTranscriptDir
	}
	pruneTranscripts(dir, now, retentionFor(cfg), "")
}

// maybeKeepTranscript applies the retention policy to a finished review and
// returns the saved path, or "" when nothing was kept (policy, empty
// transcript, or a write failure — which is logged, never fatal).
//
// stageDir holds the review's staged tool results (#2363); they are moved
// beside the transcript only when one is kept.
func maybeKeepTranscript(cfg Config, prNumber int, owner, repo, headSHA string, cov Coverage, data []byte, stageDir string) string {
	if len(data) == 0 {
		return ""
	}
	switch cfg.KeepTranscripts {
	case KeepTranscriptsOff:
		return ""
	case KeepTranscriptsPartial:
		if cov.Complete {
			return ""
		}
	default: // all, and the empty value
	}
	dir := cfg.TranscriptDir
	if dir == "" {
		dir = DefaultTranscriptDir
	}
	path, err := saveTranscript(dir, owner, repo, prNumber, headSHA, data, time.Now(), retentionFor(cfg), stageDir)
	if err != nil {
		logf(prNumber, "warn", "keeping review transcript for %s/%s#%d: %v\n", owner, repo, prNumber, err)
		return ""
	}
	return path
}
