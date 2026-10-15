package claudeprofile

import (
	"strings"
	"sync"
)

var (
	stateMu    sync.RWMutex
	current    Resolution
	configured bool
)

// Set records the process's resolution. cmd calls it once at startup, before
// the engine is built. Tests that call it must restore the zero value with
// Reset in a t.Cleanup and must not run in parallel.
func Set(r Resolution) {
	stateMu.Lock()
	defer stateMu.Unlock()
	current, configured = r, true
}

// Reset returns the holder to its unconfigured zero value.
func Reset() {
	stateMu.Lock()
	defer stateMu.Unlock()
	current, configured = Resolution{}, false
}

// Current returns the recorded resolution; the zero value when unconfigured.
func Current() Resolution {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return current
}

// Configured reports whether Set has been called.
func Configured() bool {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return configured
}

// EnvEntry is the environment entry that makes a child process use the
// resolved profile: "CLAUDE_CONFIG_DIR=<dir>", or the bare name (a removal
// sentinel for mergeEnv) when nothing resolved. When unconfigured it returns
// "" — the caller adds nothing and the child inherits, as before resolution
// existed (engines built without cmd, i.e. tests).
func EnvEntry() string {
	stateMu.RLock()
	defer stateMu.RUnlock()
	if !configured {
		return ""
	}
	if current.Dir == "" {
		return BareEnv
	}
	return BareEnv + "=" + current.Dir
}

// ApplyToEnv returns env with CLAUDE_CONFIG_DIR replaced by the resolved value
// (or removed when nothing resolved). Unconfigured, env is returned unchanged.
// Used for interactive claude launches that inherit os.Environ().
func ApplyToEnv(env []string) []string {
	entry := EnvEntry()
	if entry == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if kv == BareEnv || strings.HasPrefix(kv, BareEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	if strings.Contains(entry, "=") {
		out = append(out, entry)
	}
	return out
}
