package gate

import (
	"fmt"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/handarbeit/fabrik/internal/sessionreap"
)

// fakeProcs is a scripted Procs: no real process is ever signalled, so the
// teardown tests run sandboxed and deterministic.
type fakeProcs struct {
	mu      sync.Mutex
	procs   map[int]*fakeProc
	signals []string // "pid SIGNAME" or "pid group SIGNAME", in order
}

// fakeProc is one scripted process. diesOn names the first thing that kills it:
// "sigint" (first SIGINT), "sigint2" (second), "sigterm" / "sigkill" (group
// signals) or "never".
type fakeProc struct {
	token    string
	diesOn   string
	unlocked bool // it does not hold the bed's instance flock
	sigints  int
	dead     bool
}

func newFakeProcs() *fakeProcs { return &fakeProcs{procs: map[int]*fakeProc{}} }

// launch registers a live process, writes the bed's lock record for it, and
// returns it.
func (f *fakeProcs) launch(t *testing.T, bed string, pid int, token, diesOn string) *fakeProc {
	t.Helper()
	p := &fakeProc{token: token, diesOn: diesOn}
	f.mu.Lock()
	f.procs[pid] = p
	f.mu.Unlock()
	mustWrite(t, bed+"/.fabrik/fabrik.lock", strconv.Itoa(pid)+"\n")
	return p
}

func (f *fakeProcs) live(pid int) *fakeProc {
	if p := f.procs[pid]; p != nil && !p.dead {
		return p
	}
	return nil
}

func (f *fakeProcs) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live(pid) != nil
}

func (f *fakeProcs) Token(pid int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.live(pid); p != nil {
		return p.token
	}
	return ""
}

func (f *fakeProcs) Signal(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, fmt.Sprintf("%d %v", pid, sig))
	p := f.live(pid)
	if p == nil || sig != syscall.SIGINT {
		return nil
	}
	p.sigints++
	if (p.diesOn == "sigint" && p.sigints == 1) || (p.diesOn == "sigint2" && p.sigints == 2) {
		p.dead = true
	}
	return nil
}

func (f *fakeProcs) SignalGroup(owner sessionreap.Owner, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, fmt.Sprintf("%d group %v", owner.PID, sig))
	p := f.live(owner.PID)
	if p == nil {
		return nil
	}
	if p.token != owner.Start {
		return fmt.Errorf("%w: recycled", sessionreap.ErrGroupNotOwned)
	}
	if (p.diesOn == "sigterm" && sig == syscall.SIGTERM) || (p.diesOn == "sigkill" && sig == syscall.SIGKILL) {
		p.dead = true
	}
	return nil
}

func (f *fakeProcs) LockHeld(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.procs {
		if !p.dead && !p.unlocked {
			return true
		}
	}
	return false
}

func (f *fakeProcs) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.signals...)
}
