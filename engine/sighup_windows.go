//go:build windows

package engine

import (
	"context"
	"os"
)

// registerSighupHandler is a no-op on Windows: SIGHUP is not a Windows signal.
func registerSighupHandler(_ context.Context, _ context.CancelFunc, _ *Engine) {}

// performSighupRestart is a no-op on Windows: SIGHUP is not a Windows signal.
func performSighupRestart(_ *Engine, _ *os.File) {}
