//go:build !linux && !darwin && !windows

package sessionreap

// Environ has no reader on this platform: every process reads as unmarked.
func Environ(int) ([]string, error) { return nil, ErrEnvUnreadable }
