package localapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// maxSocketPath is the longest socket path SocketPath will return directly.
// sun_path is 104 bytes on macOS and 108 on Linux (including the NUL); this
// leaves headroom for both.
const maxSocketPath = 100

// SocketName is the socket's file name under <fabrikDir>/.fabrik/state/.
const SocketName = "fabrik.sock"

// SocketPath returns the Unix socket path for the daemon whose working
// directory is fabrikDir: <fabrikDir>/.fabrik/state/fabrik.sock. When that path
// would exceed sun_path (a deep checkout, or t.TempDir() on macOS), it falls
// back to a short, deterministic path in a private per-user directory:
// /tmp/fabrik-<uid>/<12 hex of sha256(resolved fabrikDir)>.sock.
//
// fabrikDir is symlink-resolved first, so /tmp vs /private/tmp (or any other
// spelling of one directory) gives the same primary-vs-fallback decision and
// the same hash. The fallback is a fixed /tmp, not $TMPDIR: the daemon and a
// Claude-launched `fabrik mcp` need not share an environment, and a differing
// $TMPDIR would read as "daemon not running".
//
// The daemon and `fabrik mcp --dir` both call this with the same directory, so
// they always agree, and two daemons in different directories never share a
// socket.
func SocketPath(fabrikDir string) string {
	abs, err := filepath.Abs(fabrikDir)
	if err != nil {
		abs = fabrikDir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	primary := filepath.Join(abs, ".fabrik", "state", SocketName)
	if len(primary) <= maxSocketPath {
		return primary
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(fallbackDir(), hex.EncodeToString(sum[:6])+".sock")
}

// fallbackRoot is where fallback socket directories live. Fixed rather than
// $TMPDIR (see SocketPath); a var only so tests can point it at a short temp dir.
var fallbackRoot = "/tmp"

// fallbackDir is the private per-user directory holding fallback sockets.
func fallbackDir() string {
	return filepath.Join(fallbackRoot, fmt.Sprintf("fabrik-%d", os.Getuid()))
}

// prepareDir makes sure the directory that will hold the socket exists. A
// directory this process creates for a socket is 0700; an existing directory
// under the per-user fallback prefix must be owned by the caller and not be a
// symlink, and is tightened to 0700.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating socket directory %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspecting socket directory %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket directory %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("socket directory %s is owned by uid %d, not %d", dir, st.Uid, os.Getuid())
	}
	// Only the per-user fallback directory is ours to tighten; the primary
	// <fabrikDir>/.fabrik/state directory also holds other engine state files.
	if dir == fallbackDir() && fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("tightening socket directory %s to 0700: %w", dir, err)
		}
	}
	return nil
}

// umaskMu serialises the process-wide umask change around bind.
var umaskMu sync.Mutex

// withUmask runs fn with the process umask set to mask, restoring it after.
func withUmask(mask int, fn func() error) error {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	return fn()
}
