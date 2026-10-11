package sessionreap

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// ErrEnvUnreadable is returned by Environ when a process's environment cannot be
// read: it is not ours, it is protected (SIP on macOS), it is gone, or the
// platform has no reader. Callers must treat it as "not marked" — never as a
// reason to signal a process (#2128).
var ErrEnvUnreadable = errors.New("process environment unreadable")

// parseProcArgs2 extracts the environment from a macOS kern.procargs2 buffer:
// int32 argc, the exec path, NUL padding, argc argv strings, then the
// environment strings. A buffer that is too short to hold an environment (the
// stub the kernel returns for protected binaries) yields ErrEnvUnreadable.
func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 5 {
		return nil, ErrEnvUnreadable
	}
	argc := int(int32(binary.LittleEndian.Uint32(buf[:4])))
	if argc < 0 {
		return nil, ErrEnvUnreadable
	}
	rest := buf[4:]
	// exec path
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, ErrEnvUnreadable
	}
	rest = rest[i:]
	// padding NULs between the exec path and argv[0]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	// argv
	for n := 0; n < argc; n++ {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return nil, ErrEnvUnreadable
		}
		rest = rest[i+1:]
	}
	return splitEnv(rest), nil
}

// splitEnv splits a NUL-separated environment block, stopping at the first empty
// entry (the terminator on macOS; Linux's block simply ends).
func splitEnv(b []byte) []string {
	var out []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, 0)
		var ent []byte
		if i < 0 {
			ent, b = b, nil
		} else {
			ent, b = b[:i], b[i+1:]
		}
		if len(ent) == 0 {
			break
		}
		out = append(out, string(ent))
	}
	return out
}

// HasEnv reports whether env contains exactly the entry key=value.
func HasEnv(env []string, key, value string) bool {
	want := key + "=" + value
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// LookupEnv returns the value of key in a KEY=VALUE slice.
func LookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return e[len(prefix):], true
		}
	}
	return "", false
}
