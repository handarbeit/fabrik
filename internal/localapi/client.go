package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"
)

// DialTimeout bounds connecting to the daemon socket.
const DialTimeout = 3 * time.Second

// DefaultCallTimeout bounds one request when the context has no deadline.
const DefaultCallTimeout = 30 * time.Second

// NoDaemonError means nothing is listening on the socket (no daemon, a stale
// socket file, or a permission problem). Callers surface it as "Fabrik daemon
// not running at <path>".
type NoDaemonError struct {
	Path string
	Err  error
}

func (e *NoDaemonError) Error() string {
	return fmt.Sprintf("Fabrik daemon not running at %s (%v)", e.Path, e.Err)
}

func (e *NoDaemonError) Unwrap() error { return e.Err }

var reqSeq atomic.Uint64

// Call sends one request on a fresh connection and decodes the result into
// result (which may be nil). Server-initiated frames received while waiting are
// skipped. A protocol-level error is returned as *Error; an unreachable daemon
// as *NoDaemonError. It never blocks past ctx's deadline (or
// DefaultCallTimeout when ctx has none).
func Call(ctx context.Context, path, method string, params, result any) error {
	d := net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return &NoDaemonError{Path: path, Err: err}
	}
	defer conn.Close()

	deadline := time.Now().Add(DefaultCallTimeout)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	id := strconv.FormatUint(reqSeq.Add(1), 10)
	req := Frame{ID: id, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encoding params: %w", err)
		}
		req.Params = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing request: %w", err)
	}

	r := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := readLine(r, 16*MaxLineBytes)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("reading response: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			return fmt.Errorf("malformed response frame: %w", err)
		}
		if f.IsServerInitiated() {
			continue
		}
		if f.ID != id && f.Error == nil {
			continue
		}
		if f.Error != nil {
			return f.Error
		}
		if result != nil && len(f.Result) > 0 {
			if err := json.Unmarshal(f.Result, result); err != nil {
				return fmt.Errorf("decoding result: %w", err)
			}
		}
		return nil
	}
}

// IsNoDaemon reports whether err is a *NoDaemonError.
func IsNoDaemon(err error) bool {
	var nd *NoDaemonError
	return errors.As(err, &nd)
}
