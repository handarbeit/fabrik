package warnings

import (
	"bytes"
	"strings"
	"testing"
)

func TestReport_LogsAndRecords(t *testing.T) {
	setOverride(t)
	var buf bytes.Buffer
	c := Condition{Type: "x", Subject: "a", Title: "Thing wrong", Detail: "details", FixAction: "shell_command"}
	if err := Report(&buf, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[startup] warning: Thing wrong: details") {
		t.Errorf("log = %q", buf.String())
	}
	es, _ := Load()
	if len(es) != 1 || es[0].Key != "x:a" || es[0].Fingerprint != "details" {
		t.Fatalf("entries = %+v", es)
	}
}

func TestReport_NilWriterStillRecords(t *testing.T) {
	setOverride(t)
	if err := Report(nil, Condition{Type: "x", Subject: "a", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if es, _ := Load(); len(es) != 1 {
		t.Fatalf("want 1 entry, got %d", len(es))
	}
}

func TestReport_NoticeAndLogLine(t *testing.T) {
	setOverride(t)
	var buf bytes.Buffer
	_ = Report(&buf, Condition{Type: "x", Subject: "a", Level: LevelNotice, Title: "t", LogLine: "custom"})
	if buf.String() != "[startup] notice: custom\n" {
		t.Errorf("log = %q", buf.String())
	}
}

func TestReport_RepeatYieldsOneEntry(t *testing.T) {
	setOverride(t)
	c := Condition{Type: "x", Subject: "a", Title: "t", Detail: "d"}
	for i := 0; i < 5; i++ {
		if err := Report(nil, c); err != nil {
			t.Fatal(err)
		}
	}
	if es, _ := Load(); len(es) != 1 {
		t.Fatalf("want 1 entry, got %d", len(es))
	}
}

func TestReport_DismissalFollowsDetail(t *testing.T) {
	setOverride(t)
	c := Condition{Type: "x", Subject: "a", Title: "t", Detail: "d1"}
	_ = Report(nil, c)
	_ = Dismiss("x:a")
	_ = Report(nil, c)
	if es, _ := Load(); !es[0].Dismissed {
		t.Fatal("unchanged condition must stay dismissed")
	}
	c.Detail = "d2"
	_ = Report(nil, c)
	if es, _ := Load(); es[0].Dismissed {
		t.Fatal("changed detail must reappear")
	}
	// Explicit fingerprint: detail churn does not reappear.
	c = Condition{Type: "x", Subject: "b", Title: "t", Detail: "n=1", Fingerprint: "const"}
	_ = Report(nil, c)
	_ = Dismiss("x:b")
	c.Detail = "n=2"
	_ = Report(nil, c)
	es, _ := Load()
	for _, e := range es {
		if e.Key == "x:b" && !e.Dismissed {
			t.Fatal("explicit fingerprint must keep dismissal")
		}
	}
}

func TestResolve(t *testing.T) {
	setOverride(t)
	_ = Report(nil, Condition{Type: "x", Subject: "a", Title: "t"})
	if err := Resolve("x:a"); err != nil {
		t.Fatal(err)
	}
	if es, _ := Load(); len(es) != 0 {
		t.Fatalf("want cleared, got %+v", es)
	}
}

func TestReconcile_ClearsFixedSubjectAcrossRestart(t *testing.T) {
	setOverride(t)
	mk := func(subs ...string) []Condition {
		var cs []Condition
		for _, s := range subs {
			cs = append(cs, Condition{Subject: s, Title: "t " + s})
		}
		return cs
	}
	_ = Report(nil, Condition{Type: "other", Subject: "z", Title: "keep"})
	if err := Reconcile(nil, "x", mk("a", "b")); err != nil {
		t.Fatal(err)
	}
	// "restart": b fixed.
	if err := Reconcile(nil, "x", mk("a")); err != nil {
		t.Fatal(err)
	}
	es, _ := Load()
	keys := map[string]bool{}
	for _, e := range es {
		keys[e.Key] = true
	}
	if !keys["x:a"] || keys["x:b"] || !keys["other:z"] {
		t.Fatalf("keys = %v", keys)
	}
	// Empty set clears the type, leaves other types.
	if err := Reconcile(nil, "x", nil); err != nil {
		t.Fatal(err)
	}
	es, _ = Load()
	if len(es) != 1 || es[0].Key != "other:z" {
		t.Fatalf("entries = %+v", es)
	}
}
