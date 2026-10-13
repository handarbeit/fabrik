//go:build linux

package sessionreap

import (
	"os"
	"strconv"
)

// Environ returns pid's initial (exec-time) environment as KEY=VALUE strings,
// read from /proc/<pid>/environ (same uid only). pid 1 and below are refused
// outright, as on macOS, so a root process (a container's init) reads the same.
func Environ(pid int) ([]string, error) {
	if pid <= 1 {
		return nil, ErrEnvUnreadable
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, ErrEnvUnreadable
	}
	return splitEnv(b), nil
}
