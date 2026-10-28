//go:build windows

package lockprobe

// flockHeld has no flock to probe on this platform; it reports not held.
func flockHeld(string) bool { return false }
