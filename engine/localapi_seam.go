package engine

// StartLocalAPIForTest binds the daemon's local API on the socket at path, as
// Run() does after the instance lock is held (#2484): a sim scenario attaches a
// real localapi.RunStream subscriber to it. Start the channel events first, so the
// server advertises streaming. A SIGHUP restart tail closes it and unlinks the
// socket; the next engine binds the same path. Tests only.
func (e *Engine) StartLocalAPIForTest(path string) { e.startLocalAPIAt(path) }

// CloseLocalAPIForTest stops the local API and removes its socket. Idempotent.
func (e *Engine) CloseLocalAPIForTest() { e.closeLocalAPI() }
