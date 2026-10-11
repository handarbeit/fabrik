package engine

import "strings"

// warnLogWriter adapts Engine.logf to the io.Writer that warnings.Report logs
// through (#2135), so a condition recorded from engine code is logged and
// recorded by one call. warnings.Report prefixes its line with
// "[startup] warning: " / "[startup] notice: "; the engine's own log already
// prefixes the tag, so the line is re-prefixed "WARNING: " / "NOTICE: " — the
// wording these engine-side warnings have always used.
type warnLogWriter struct {
	e   *Engine
	tag string
}

func (e *Engine) warnWriter(tag string) warnLogWriter { return warnLogWriter{e: e, tag: tag} }

func (w warnLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSuffix(string(p), "\n")
	switch {
	case strings.HasPrefix(msg, "[startup] warning: "):
		msg = "WARNING: " + strings.TrimPrefix(msg, "[startup] warning: ")
	case strings.HasPrefix(msg, "[startup] notice: "):
		msg = "NOTICE: " + strings.TrimPrefix(msg, "[startup] notice: ")
	}
	w.e.logf(0, w.tag, "%s\n", msg)
	return len(p), nil
}
