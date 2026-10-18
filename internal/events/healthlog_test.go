package events

import (
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func reconnecting(err string, at time.Time) HealthEvent {
	return HealthEvent{State: HealthReconnecting, At: at, Err: err}
}

func TestHealthErrorLog_FirstErrorLogs(t *testing.T) {
	var l HealthErrorLog
	line, ok := l.Observe(reconnecting("dial tcp: lookup api.hookdeck.com: no such host", t0))
	if !ok || line != "connection error: dial tcp: lookup api.hookdeck.com: no such host" {
		t.Fatalf("got %q, %v", line, ok)
	}
}

func TestHealthErrorLog_RepeatsCoalesced(t *testing.T) {
	var l HealthErrorLog
	l.Observe(reconnecting("boom", t0))
	for i := 1; i <= 20; i++ {
		if line, ok := l.Observe(reconnecting("boom", t0.Add(time.Duration(i)*10*time.Second))); ok {
			t.Fatalf("repeat %d logged %q", i, line)
		}
	}
}

func TestHealthErrorLog_ChangedErrorLogsImmediately(t *testing.T) {
	var l HealthErrorLog
	l.Observe(reconnecting("boom", t0))
	line, ok := l.Observe(reconnecting("bang", t0.Add(time.Second)))
	if !ok || line != "connection error: bang" {
		t.Fatalf("got %q, %v", line, ok)
	}
	// the new text is now the baseline
	if _, ok := l.Observe(reconnecting("bang", t0.Add(2*time.Second))); ok {
		t.Fatal("repeat of changed error logged")
	}
}

func TestHealthErrorLog_ReminderWithCount(t *testing.T) {
	var l HealthErrorLog
	l.Observe(reconnecting("boom", t0))
	for i := 1; i <= 3; i++ {
		l.Observe(reconnecting("boom", t0.Add(time.Duration(i)*time.Minute)))
	}
	line, ok := l.Observe(reconnecting("boom", t0.Add(HealthErrorReminderInterval)))
	want := "connection still failing: boom (4 occurrences since last report)"
	if !ok || line != want {
		t.Fatalf("got %q, %v; want %q", line, ok, want)
	}
	// the interval restarts from the reminder
	if _, ok := l.Observe(reconnecting("boom", t0.Add(HealthErrorReminderInterval+time.Minute))); ok {
		t.Fatal("logged again before the next interval")
	}
	line, ok = l.Observe(reconnecting("boom", t0.Add(2*HealthErrorReminderInterval)))
	if !ok || !strings.Contains(line, "(2 occurrences since last report)") {
		t.Fatalf("second reminder: %q, %v", line, ok)
	}
}

func TestHealthErrorLog_ConnectedResetsEpisode(t *testing.T) {
	var l HealthErrorLog
	l.Observe(reconnecting("boom", t0))
	if line, ok := l.Observe(HealthEvent{State: HealthConnected, At: t0.Add(time.Second)}); ok {
		t.Fatalf("Connected logged %q", line)
	}
	line, ok := l.Observe(reconnecting("boom", t0.Add(2*time.Second)))
	if !ok || line != "connection error: boom" {
		t.Fatalf("new episode should log the same text again: %q, %v", line, ok)
	}
}

func TestHealthErrorLog_EmptyErrLogsNothing(t *testing.T) {
	var l HealthErrorLog
	if line, ok := l.Observe(reconnecting("", t0)); ok {
		t.Fatalf("got %q", line)
	}
	l.Observe(reconnecting("boom", t0))
	if _, ok := l.Observe(reconnecting("", t0.Add(time.Second))); ok {
		t.Fatal("empty Err logged")
	}
	// an empty Err does not break coalescing of the following repeat
	if _, ok := l.Observe(reconnecting("boom", t0.Add(2*time.Second))); ok {
		t.Fatal("repeat logged after an empty-Err event")
	}
}

func TestHealthErrorLog_ZeroAtUsesNow(t *testing.T) {
	now := t0
	l := HealthErrorLog{Now: func() time.Time { return now }}
	l.Observe(HealthEvent{State: HealthReconnecting, Err: "boom"})
	now = now.Add(HealthErrorReminderInterval)
	if _, ok := l.Observe(HealthEvent{State: HealthReconnecting, Err: "boom"}); !ok {
		t.Fatal("expected a reminder from the injected clock")
	}
}

func TestSanitizeErrText(t *testing.T) {
	got := SanitizeErrText("creating cli-session: unexpected status 401:\n  {\"message\":\n\t\"nope\"}\r\n")
	if strings.ContainsAny(got, "\r\n\t") || strings.Contains(got, "  ") {
		t.Fatalf("not collapsed: %q", got)
	}
	long := strings.Repeat("é", 1000)
	got = SanitizeErrText(long)
	if r := []rune(got); len(r) != healthErrMaxRunes+1 || r[len(r)-1] != '…' {
		t.Fatalf("bad truncation: %d runes", len(r))
	}
	if short := SanitizeErrText("short"); short != "short" {
		t.Fatalf("short changed: %q", short)
	}
}

func TestHealthErrorLog_SanitisesButComparesRaw(t *testing.T) {
	var l HealthErrorLog
	prefix := strings.Repeat("x", 400)
	line, ok := l.Observe(reconnecting(prefix+"A\nline2", t0))
	if !ok || strings.Contains(line, "\n") || len([]rune(line)) > 400 {
		t.Fatalf("line not bounded/single: %q", line)
	}
	// differs only beyond the truncation point: still a different error
	if _, ok := l.Observe(reconnecting(prefix+"B", t0.Add(time.Second))); !ok {
		t.Fatal("raw-different error was coalesced")
	}
}

func TestHealthErrorLog_Concurrent(t *testing.T) {
	var l HealthErrorLog
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Observe(reconnecting("boom", t0.Add(time.Duration(j)*time.Second)))
				if j%25 == 0 {
					l.Observe(HealthEvent{State: HealthConnected})
				}
			}
		}(i)
	}
	wg.Wait()
}
