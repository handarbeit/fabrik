package localapi

import (
	"errors"
	"net"
	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
)

// failingConn fails every write but is otherwise a live connection, the shape of
// a push that hit its write deadline.
type failingConn struct {
	net.Conn
	closed bool
}

func (c *failingConn) Write([]byte) (int, error) { return 0, errors.New("write timeout") }
func (c *failingConn) Close() error              { c.closed = true; return c.Conn.Close() }

// A failed push must end the connection: heartbeats would otherwise keep a
// stream that has lost a (possibly partial) frame looking healthy, and the held
// entry would never be redelivered.
func TestSessionSinkClosesConnectionOnFailedDeliver(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	fc := &failingConn{Conn: srv}
	sess := newSession(fc)

	err := sessionSink{sess: sess, subscriber: "x"}.Deliver(channelevents.Event{Type: channelevents.EventType("label-applied")})
	if err == nil {
		t.Fatal("Deliver succeeded although the write failed")
	}
	if !fc.closed {
		t.Fatal("connection left open after a failed Deliver; it must be closed so the client re-attaches")
	}
}
