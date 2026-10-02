//go:build !windows

package sessionreap

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// killRec is a Kill seam that records every call and never signals anything.
type killRec struct {
	mu    sync.Mutex
	calls [][2]int
	probe error // returned for sig 0
}

func (k *killRec) kill(pid int, sig syscall.Signal) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if sig == 0 {
		return k.probe
	}
	k.calls = append(k.calls, [2]int{pid, int(sig)})
	return nil
}

func (k *killRec) signals() [][2]int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([][2]int(nil), k.calls...)
}

func TestCheckPGID_RefusesCatastrophicTargets(t *testing.T) {
	own := syscall.Getpgrp()
	for _, pgid := range []int{-1, 0, 1, own} {
		if err := CheckPGID(pgid); !errors.Is(err, ErrUnsafeGroup) {
			t.Errorf("CheckPGID(%d) = %v, want ErrUnsafeGroup", pgid, err)
		}
	}
	if err := CheckPGID(own + 100000); err != nil {
		t.Errorf("CheckPGID of an ordinary pgid refused: %v", err)
	}
}

// R4 is checked before ownership: even a seam that says everything is owned must
// not make SignalGroup send anything to -1, 0, 1 or the caller's own group.
func TestSignalGroup_RefusesBeforeOwnership(t *testing.T) {
	for _, pid := range []int{-1, 0, 1, syscall.Getpgrp()} {
		rec := &killRec{} // probe nil: "live process holds the PID"
		sink := &logSink{}
		o := Options{
			Log:     sink.log,
			Kill:    rec.kill,
			Getpgid: func(p int) (int, error) { return p, nil },
			Ppid:    func(int) (int, bool) { return os.Getpid(), true },
		}
		err := SignalGroup(Owner{PID: pid}, syscall.SIGKILL, o)
		if !errors.Is(err, ErrUnsafeGroup) {
			t.Errorf("SignalGroup(%d) = %v, want ErrUnsafeGroup", pid, err)
		}
		if len(rec.signals()) != 0 {
			t.Errorf("SignalGroup(%d) signalled %v", pid, rec.signals())
		}
		if !strings.Contains(sink.joined(), "refused") {
			t.Errorf("refusal for %d not logged: %q", pid, sink.joined())
		}
	}
}

func TestSignalGroup_LiveOwnGroupIsSignalled(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	// No token recorded: the parent check (we started it) authorises the kill.
	if err := SignalGroup(Owner{PID: pid}, syscall.SIGKILL, Options{}); err != nil {
		t.Fatalf("SignalGroup = %v", err)
	}
	if !waitDead(pid, 3*time.Second) {
		t.Fatal("own live group was not signalled")
	}
}

func TestSignalGroup_LiveOwnGroupWithRecordedToken(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	tok := RecordStart(pid, Options{})
	t.Cleanup(func() { ForgetStart(pid) })
	if tok == "" {
		t.Skip("platform start token unavailable")
	}
	if got := OwnerOf(pid); got.Start != tok {
		t.Fatalf("OwnerOf = %+v, want token %q", got, tok)
	}
	if err := SignalGroup(OwnerOf(pid), syscall.SIGKILL, Options{}); err != nil {
		t.Fatal(err)
	}
	if !waitDead(pid, 3*time.Second) {
		t.Fatal("group with matching recorded token was not signalled")
	}
}

func TestSignalGroup_RecycledPIDIsSkipped(t *testing.T) {
	const pid = 424242
	cases := []struct {
		name  string
		owner Owner
		o     Options
	}{
		{
			name:  "live leader, wrong parent, no token",
			owner: Owner{PID: pid},
			o: Options{
				Getpgid: func(p int) (int, error) { return p, nil },
				Ppid:    func(int) (int, bool) { return 7, true },
			},
		},
		{
			name:  "live leader, mismatched start token even with us as parent",
			owner: Owner{PID: pid, Start: "old"},
			o: Options{
				Getpgid: func(p int) (int, error) { return p, nil },
				Ppid:    func(int) (int, bool) { return os.Getpid(), true },
				Start:   func(int) string { return "new" },
			},
		},
		{
			name:  "live process does not lead its own group",
			owner: Owner{PID: pid, Start: "t"},
			o: Options{
				Getpgid: func(int) (int, error) { return 99, nil },
				Start:   func(int) string { return "t" },
			},
		},
		{
			name:  "parent unknown fails closed",
			owner: Owner{PID: pid},
			o: Options{
				Getpgid: func(p int) (int, error) { return p, nil },
				Ppid:    func(int) (int, bool) { return 0, false },
			},
		},
		{
			name:  "leader gone and membership unreadable",
			owner: Owner{PID: pid},
			o: Options{
				List: func() ([]int, error) { return nil, errors.New("boom") },
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &killRec{}
			if strings.Contains(tc.name, "leader gone") {
				rec.probe = syscall.ESRCH
			}
			sink := &logSink{}
			o := tc.o
			o.Log, o.Kill = sink.log, rec.kill
			err := SignalGroup(tc.owner, syscall.SIGKILL, o)
			if !errors.Is(err, ErrGroupNotOwned) {
				t.Fatalf("err = %v, want ErrGroupNotOwned", err)
			}
			if len(rec.signals()) != 0 {
				t.Fatalf("signalled %v", rec.signals())
			}
			if !strings.Contains(sink.joined(), "skipped") {
				t.Errorf("skip not logged: %q", sink.joined())
			}
		})
	}
}

func TestSignalGroup_LeaderGoneEmptyGroupIsNoOp(t *testing.T) {
	rec := &killRec{probe: syscall.ESRCH}
	o := Options{
		Kill:    rec.kill,
		List:    func() ([]int, error) { return []int{10, 11}, nil },
		Getpgid: func(int) (int, error) { return 1234, nil },
		Zombie:  func(int) bool { return false },
	}
	if err := SignalGroup(Owner{PID: 424242}, syscall.SIGKILL, o); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(rec.signals()) != 0 {
		t.Fatalf("empty group signalled %v", rec.signals())
	}
}

func TestSignalGroup_LeaderGoneZombieOnlyGroupIsNoOp(t *testing.T) {
	rec := &killRec{probe: syscall.ESRCH}
	o := Options{
		Kill:    rec.kill,
		List:    func() ([]int, error) { return []int{10}, nil },
		Getpgid: func(int) (int, error) { return 424242, nil },
		Zombie:  func(int) bool { return true },
	}
	if err := SignalGroup(Owner{PID: 424242}, syscall.SIGKILL, o); err != nil {
		t.Fatal(err)
	}
	if len(rec.signals()) != 0 {
		t.Fatalf("zombie-only group signalled %v", rec.signals())
	}
}

func TestSignalGroup_LeaderGoneWithMemberIsSignalled(t *testing.T) {
	rec := &killRec{probe: syscall.ESRCH}
	o := Options{
		Kill:    rec.kill,
		List:    func() ([]int, error) { return []int{10, 11}, nil },
		Getpgid: func(p int) (int, error) { return map[int]int{10: 1234, 11: 424242}[p], nil },
		Zombie:  func(int) bool { return false },
	}
	if err := SignalGroup(Owner{PID: 424242}, syscall.SIGKILL, o); err != nil {
		t.Fatal(err)
	}
	got := rec.signals()
	if len(got) != 1 || got[0] != [2]int{-424242, int(syscall.SIGKILL)} {
		t.Fatalf("signals = %v, want one SIGKILL to -424242", got)
	}
}

// The normal post-exit state: the worker's leader has exited and been reaped,
// but a grandchild in its group lives on. SignalGroup must still clean it up.
func TestSignalGroup_PostExitGrandchildIsKilled(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $! ; exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output() // Wait() reaps the leader
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("grandchild pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	leader := cmd.Process.Pid
	if pg, gerr := unix.Getpgid(grandchild); gerr != nil || pg != leader {
		t.Fatalf("fixture: grandchild pgid=%d err=%v, want %d", pg, gerr, leader)
	}
	if alive(leader) {
		t.Fatal("fixture: leader still alive after Wait")
	}
	if err := SignalGroup(Owner{PID: leader, Start: "recorded-at-spawn"}, syscall.SIGKILL, Options{}); err != nil {
		t.Fatalf("SignalGroup = %v", err)
	}
	if !waitDead(grandchild, 3*time.Second) {
		t.Fatal("post-exit grandchild survived")
	}
}

// listing failure inside Signal falls back to the group — now ownership-checked.
func TestSignal_ListErrorFallbackSkipsRecycledPID(t *testing.T) {
	rec := &killRec{}
	sink := &logSink{}
	o := Options{
		Log:     sink.log,
		Kill:    rec.kill,
		List:    func() ([]int, error) { return nil, errors.New("boom") },
		Getpgid: func(p int) (int, error) { return p, nil },
		Ppid:    func(int) (int, bool) { return 7, true }, // not us
	}
	if _, err := Signal(424242, syscall.SIGKILL, o); !errors.Is(err, ErrGroupNotOwned) {
		t.Fatalf("err = %v, want ErrGroupNotOwned", err)
	}
	if len(rec.signals()) != 0 {
		t.Fatalf("signalled %v", rec.signals())
	}
}

func TestSignal_ListErrorFallbackSignalsOwnedGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	o := Options{List: func() ([]int, error) { return nil, errors.New("boom") }}
	if _, err := Signal(pid, syscall.SIGKILL, o); err != nil {
		t.Fatal(err)
	}
	if !waitDead(pid, 3*time.Second) {
		t.Fatal("owned worker survived the fallback")
	}
}
