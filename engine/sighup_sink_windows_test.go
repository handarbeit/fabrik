//go:build windows

package engine

// installStraySighupSink is a no-op on Windows, which has no SIGHUP.
func installStraySighupSink() {}
