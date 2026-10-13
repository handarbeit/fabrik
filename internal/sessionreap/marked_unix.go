//go:build !windows

package sessionreap

import (
	"os"
	"syscall"
	"time"
)

// Owner-marker reap (#2128, ADR-2128).
//
// A worker's environment carries FABRIK_WORKER_OWNER=<owner file path>; every descendant —
// go test, sim.test, its re-exec'd helpers — inherits it, whatever session,
// process group, parent or cwd it ends up with. That makes the marker positive
// evidence of ownership where process-tree shape never is: a backgrounded Bash
// command that is still in use looks exactly like an orphan by ppid/pgid, but
// carries the marker of a worker that is alive.
//
// This package only answers "which processes carry these markers, and signal
// them safely". WHETHER the owning worker is provably gone is the caller's
// decision (the engine's classifyWorker); a marker alone never authorises a kill.
//
// Signals go to exact PIDs, not to the process's session or group: marker
// membership is the precise set, and a session or group may hold unmarked
// processes. Each PID is re-verified — start token unchanged and marker still
// present — immediately before every signal, and the R4 refusals of CheckSID and
// CheckPGID (SID/PGID 0 and 1, the caller's own) apply to its session and group.

type markedProc struct {
	pid   int
	start string
	value string
}

func (o Options) environ(pid int) ([]string, error) {
	if o.Environ != nil {
		return o.Environ(pid)
	}
	return Environ(pid)
}

// scanMarked lists live, non-zombie processes whose initial environment carries
// key=<one of markers>. An unreadable environment is "not marked".
func scanMarked(key string, markers map[string]struct{}, o Options) ([]markedProc, error) {
	if o.IgnoreMarkers || len(markers) == 0 {
		return nil, nil
	}
	pids, err := o.list()
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []markedProc
	for _, pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		env, eerr := o.environ(pid)
		if eerr != nil {
			continue
		}
		v, ok := LookupEnv(env, key)
		if !ok {
			continue
		}
		if _, want := markers[v]; !want {
			continue
		}
		if o.zombie(pid) {
			continue
		}
		out = append(out, markedProc{pid: pid, start: o.startToken(pid), value: v})
	}
	return out, nil
}

// MarkedPIDs returns the live PIDs carrying key=<marker> for each marker, keyed
// by marker value.
func MarkedPIDs(key string, markers map[string]struct{}, o Options) (map[string][]int, error) {
	procs, err := scanMarked(key, markers, o)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]int)
	for _, p := range procs {
		out[p.value] = append(out[p.value], p.pid)
	}
	return out, nil
}

// stillMarked re-verifies p immediately before a signal: the PID must still be
// the process that was scanned (start token unchanged; an empty token means it
// could not be established, which fails closed), still carry the marker, and sit
// in a session and group that pass the R4 refusals.
func stillMarked(key string, p markedProc, o Options) bool {
	if p.start == "" || o.startToken(p.pid) != p.start {
		return false
	}
	env, err := o.environ(p.pid)
	if err != nil || !HasEnv(env, key, p.value) {
		return false
	}
	sid, err := o.getsid(p.pid)
	if err != nil || CheckSID(sid) != nil {
		return false
	}
	pgid, err := o.getpgid(p.pid)
	if err != nil || CheckPGID(pgid) != nil {
		return false
	}
	return true
}

// ReapMarked signals every live process carrying key=<one of markers>:
// SIGTERM, then SIGKILL after grace to any that remain. It returns the number of
// distinct processes signalled. The caller must already have established that
// the owning worker is gone (dead or recycled); ReapMarked does not check.
func ReapMarked(key string, markers map[string]struct{}, reason string, grace time.Duration, o Options) int {
	procs, err := scanMarked(key, markers, o)
	if err != nil {
		o.logf("warn", "marked reap (reason=%s): listing processes: %v\n", reason, err)
		return 0
	}
	if len(procs) == 0 {
		return 0
	}
	signalled := map[int]bool{}
	var names []string
	send := func(sig syscall.Signal, ps []markedProc) []markedProc {
		var left []markedProc
		for _, p := range ps {
			if !stillMarked(key, p, o) {
				continue
			}
			name := o.comm(p.pid)
			if err := o.kill(p.pid, sig); err != nil {
				continue
			}
			if !signalled[p.pid] {
				signalled[p.pid] = true
				names = append(names, name)
			}
			left = append(left, p)
		}
		return left
	}
	left := send(syscall.SIGTERM, procs)
	if len(left) > 0 && grace > 0 {
		deadline := time.Now().Add(grace)
		for len(left) > 0 && time.Now().Before(deadline) {
			time.Sleep(min(o.poll(), time.Until(deadline)))
			var alive []markedProc
			for _, p := range left {
				if o.startToken(p.pid) == p.start && !o.zombie(p.pid) {
					alive = append(alive, p)
				}
			}
			left = alive
		}
	}
	send(syscall.SIGKILL, left)
	if len(signalled) > 0 {
		o.logf("kill", "marked reap: signalled %d process(es) carrying the owner marker (reason=%s): %s\n",
			len(signalled), reason, formatComms(names))
	}
	return len(signalled)
}
