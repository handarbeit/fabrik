//go:build darwin

package sessionreap

import "golang.org/x/sys/unix"

// Environ returns pid's initial (exec-time) environment as KEY=VALUE strings.
// Readable for same-user, non-protected processes (Go test binaries, python);
// SIP-protected platform binaries return a stub with no environment, and pid 1
// is refused — all of those are ErrEnvUnreadable.
func Environ(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, ErrEnvUnreadable
	}
	return parseProcArgs2(buf)
}
