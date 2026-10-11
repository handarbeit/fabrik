//go:build windows

package workerowner

// processExists is always true on Windows: with no start-token reader the
// verdict is Unknown, never Gone.
func processExists(int) bool { return true }
