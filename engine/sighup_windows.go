//go:build windows

package engine

import "context"

// registerSighupHandler is a no-op on Windows: SIGHUP is not a Windows signal.
func registerSighupHandler(_ context.Context, _ context.CancelFunc, _ *Engine, _ <-chan struct{}) {}

// performSighupRestart is a no-op on Windows: SIGHUP is not a Windows signal.
func performSighupRestart(_ *Engine, _ *instanceLocks) {}
