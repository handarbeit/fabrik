// Package workerowner is the owner-file handshake between a Claude worker's
// engine and the test binaries that run underneath it (#2128, ADR-2128).
//
// The engine sets FABRIK_WORKER_OWNER=<path of an owner file> in the worker's
// environment before starting it. The worker's PID does not exist yet at that
// point, so the engine writes {pid, start token} into the file right after
// cmd.Start(). Every descendant inherits the variable. A test binary reads the
// file and polls the identity: when the PID is gone, or alive with a different
// start token (a recycled PID), the worker is provably dead and the binary exits.
//
// Every ambiguity — no file yet, unreadable file, an unreadable start token, a
// platform without one — is Unknown, and callers treat Unknown as "keep
// running". Only positive evidence of death is Gone. The package is stdlib plus
// internal/sessionreap and is shared by engine and internal/testlife.
package workerowner

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// Info identifies the process that owns a worker's process tree.
type Info struct {
	PID        int    `json:"pid"`
	StartToken string `json:"start_token"`
	WrittenAt  string `json:"written_at,omitempty"`
}

// State is the liveness verdict for an owner.
type State int

const (
	// Unknown: identity could not be established. Callers keep running.
	Unknown State = iota
	// Alive: the PID is held by the process that was recorded.
	Alive
	// Gone: the PID is absent, or held by a process with a different start token.
	Gone
)

func (s State) String() string {
	switch s {
	case Alive:
		return "alive"
	case Gone:
		return "gone"
	}
	return "unknown"
}

// NewPath returns a fresh, unique owner-file path under dir. It does not create
// the file.
func NewPath(dir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating owner nonce: %w", err)
	}
	return filepath.Join(dir, hex.EncodeToString(b[:])+".json"), nil
}

// Write atomically writes info to path (temp file + rename), creating the
// directory if needed.
func Write(path string, info Info) error {
	if info.WrittenAt == "" {
		info.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".owner-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return errors.Join(werr, cerr)
	}
	if err := os.Chmod(name, 0600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Read parses the owner file at path.
func Read(path string) (Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, err
	}
	if info.PID <= 1 {
		return Info{}, fmt.Errorf("owner file %s names unusable pid %d", path, info.PID)
	}
	return info, nil
}

// Check classifies info. Only an absent PID, or a readable start token that
// differs from the recorded one, is Gone.
func Check(info Info, o sessionreap.Options) State {
	if info.PID <= 1 {
		return Unknown
	}
	if !processExists(info.PID) {
		return Gone
	}
	if info.StartToken == "" {
		return Unknown
	}
	tok := startToken(info.PID, o)
	switch {
	case tok == "":
		return Unknown
	case tok != info.StartToken:
		return Gone
	}
	return Alive
}

func startToken(pid int, o sessionreap.Options) string {
	if o.Start != nil {
		return o.Start(pid)
	}
	return sessionreap.StartToken(pid)
}
