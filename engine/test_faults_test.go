package engine

import (
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseTestFaults(t *testing.T) {
	if f, unk := parseTestFaults(""); f != nil || len(unk) != 0 {
		t.Fatalf("empty = %v %v, want inert", f, unk)
	}
	f, unk := parseTestFaults(" no-dedupe, drop-event:issue_comment ,bogus,drop-event:")
	if !f.active(FaultNoDedupe) || !f.dropsEvent("issue_comment") || f.dropsEvent("issues") {
		t.Errorf("parsed = %+v", f)
	}
	if len(unk) != 2 || unk[0] != "bogus" {
		t.Errorf("unknown = %v, want [bogus drop-event:]", unk)
	}
	var nilFaults *testFaults
	if nilFaults.active(FaultNoEcho) || nilFaults.dropsEvent("issues") || nilFaults.describe() != "" {
		t.Error("nil faults must be inert")
	}
}

func TestInstallTestFaults_RefusedWithoutPollControl(t *testing.T) {
	e := &Engine{cfg: Config{TestFaults: FaultNoDedupe}}
	e.installTestFaults()
	if e.faults != nil {
		t.Fatal("faults honoured without the poll-control seam")
	}
}

func TestInstallTestFaults_InertWhenUnset(t *testing.T) {
	e := &Engine{cfg: Config{PollControlFile: "/x"}}
	e.installTestFaults()
	if e.faults != nil {
		t.Fatal("faults set with an empty value")
	}
}

func TestInstallTestFaults_ActiveOnBed(t *testing.T) {
	e := &Engine{cfg: Config{PollControlFile: "/x", TestFaults: FaultNoEcho + ",drop-event:issues"}}
	e.installTestFaults()
	if !e.faults.active(FaultNoEcho) || !e.faults.dropsEvent("issues") {
		t.Fatalf("faults = %+v", e.faults)
	}
}

// drop-event skips the apply of one event type only and is not counted applied.
func TestIntakeFault_DropEvent(t *testing.T) {
	var applied atomic.Int32
	in := newTestIntake(func(string, []byte) { applied.Add(1) })
	in.faults, _ = parseTestFaults("drop-event:issue_comment")
	p := payloadFor("o/r", 1)
	in.Submit(delivery{ID: "A", EventType: "issue_comment", Payload: p})
	in.Submit(delivery{ID: "B", EventType: "issues", Payload: p})
	in.Drain()
	if applied.Load() != 1 {
		t.Errorf("applied %d, want only the issues delivery", applied.Load())
	}
	if s := in.stats.snapshot(); s.Applied["issue_comment"] != 0 || s.Applied["issues"] != 1 {
		t.Errorf("applied counters = %v", s.Applied)
	}
}

// no-dedupe applies a redelivered copy a second time.
func TestIntakeFault_NoDedupe(t *testing.T) {
	var applied atomic.Int32
	in := newTestIntake(func(string, []byte) { applied.Add(1) })
	in.faults, _ = parseTestFaults(FaultNoDedupe)
	p := payloadFor("o/r", 1)
	in.Submit(delivery{ID: "A", EventType: "issues", Payload: p})
	in.Submit(delivery{ID: "A", EventType: "issues", Payload: p})
	in.Drain()
	if applied.Load() != 2 {
		t.Errorf("applied %d, want 2 with dedupe neutralised", applied.Load())
	}
}

// no-echo makes a reserved echo never match.
func TestEchoFault_NoEcho(t *testing.T) {
	r := newEchoRegistry()
	r.disabled = true
	r.Reserve("issues", "labeled", "o/r#1+x")
	if r.MatchEchoReported("issues", "labeled", "o/r#1+x") {
		t.Fatal("echo matched with the no-echo fault")
	}
}

// Faults never reach a worker: the variable is scrubbed.
func TestTestFaultsEnvName(t *testing.T) {
	if !strings.HasPrefix(TestFaultsEnv, "FABRIK_") {
		t.Fatal("fault env must be FABRIK_-prefixed so the worker scrub covers it")
	}
}
