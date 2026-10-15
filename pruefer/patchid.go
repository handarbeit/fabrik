package pruefer

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Patch identity (#2230, adrs/2230-pruefer-patch-identity-skip.md).
//
// Two heads of one PR carry "the same change" when the unified diff of each
// against its merge base with the PR's base branch is the same patch, whatever
// their commit SHAs. patchFingerprint reduces such a diff to a hash that does
// not move when only the base moves under an unchanged patch.
//
// What is dropped, because it describes the repository around the patch rather
// than the patch:
//   - `index <blob>..<blob>` lines (blob IDs change with the base);
//   - the line numbers and function-context heading on `@@` hunk headers (they
//     shift when the base adds or removes lines above the hunk).
//
// Everything else is hashed exactly — file headers, rename/mode lines, context
// lines, added/removed lines, whitespace, CRLF and `\ No newline at end of
// file` — so any real difference reads as "changed" and leads to a review. This
// is deliberately NOT `git patch-id --stable`, which ignores whitespace: a
// whitespace-only edit (Python, YAML, Makefiles) is a real change here.
//
// Anything not positively understood (binary content, a preamble before the
// first file header, duplicate paths, an empty diff) is "not comparable", and
// a caller treats that as "review".

// patchPrint is the fingerprint of one diff.
type patchPrint struct {
	Whole   string            // hash over every file, in path order
	PerFile map[string]string // path → hash of that file's normalised block
}

// patchFingerprint fingerprints diff. ok is false when diff is not comparable.
func patchFingerprint(diff string) (patchPrint, bool) {
	if strings.TrimSpace(diff) == "" {
		return patchPrint{}, false
	}
	blocks, preamble := splitDiffFiles(diff)
	if preamble != "" || len(blocks) == 0 {
		return patchPrint{}, false
	}
	per := make(map[string]string, len(blocks))
	for _, b := range blocks {
		if b.Path == "" {
			return patchPrint{}, false
		}
		if _, dup := per[b.Path]; dup {
			return patchPrint{}, false
		}
		norm, ok := normalizeBlock(b.Raw)
		if !ok {
			return patchPrint{}, false
		}
		sum := sha256.Sum256([]byte(norm))
		per[b.Path] = hex.EncodeToString(sum[:])
	}
	paths := make([]string, 0, len(per))
	for p := range per {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(per[p]))
		h.Write([]byte{'\n'})
	}
	return patchPrint{Whole: hex.EncodeToString(h.Sum(nil)), PerFile: per}, true
}

// normalizeBlock returns one file block with the base-dependent lines removed.
// ok is false for a block whose content cannot be fingerprinted from the diff
// text (binary files: the `index` line is the only thing naming the content).
func normalizeBlock(raw string) (string, bool) {
	lines := strings.Split(raw, "\n")
	var b strings.Builder
	inHunk := false
	for i, line := range lines {
		if !inHunk {
			switch {
			case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
				return "", false
			case strings.HasPrefix(line, "index "):
				continue
			case strings.HasPrefix(line, "@@"):
				inHunk = true
				b.WriteString("@@\n")
				continue
			}
		} else if strings.HasPrefix(line, "@@") {
			b.WriteString("@@\n")
			continue
		}
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String(), true
}
