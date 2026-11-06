package appwebhook

import "net"

// DirectListener returns the listener factory of the direct receiver
// (event_source: direct, #2142): a plain TCP listener on addr. TLS is the
// fronting proxy's job.
func DirectListener(addr string) func() (net.Listener, error) {
	return func() (net.Listener, error) { return net.Listen("tcp", addr) }
}
