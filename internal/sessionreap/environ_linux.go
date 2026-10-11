//go:build linux

package sessionreap

import (
	"os"
	"strconv"
)

// Environ returns pid's initial (exec-time) environment as KEY=VALUE strings,
// read from /proc/<pid>/environ (same uid only).
func Environ(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, ErrEnvUnreadable
	}
	return splitEnv(b), nil
}
