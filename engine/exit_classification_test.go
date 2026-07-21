package engine

import (
	"context"
	"testing"
	"time"
)

// These tests exercise classifyExit and sleepOrStop, extracted from supervise,
// in isolation from the subprocess lifecycle.

func TestClassifyExit(t *testing.T) {
	tests := []struct {
		name          string
		elapsed       time.Duration
		probeTimeout  time.Duration
		stderrContent string
		wantQuick     bool
		wantIs422     bool
		wantAuth      bool
	}{
		{
			name:         "durable run, clean stderr",
			elapsed:      time.Minute,
			probeTimeout: 5 * time.Second,
			wantQuick:    false,
		},
		{
			name:          "quick exit with 422 stderr",
			elapsed:       time.Second,
			probeTimeout:  5 * time.Second,
			stderrContent: "HTTP 422: Validation Failed",
			wantQuick:     true,
			wantIs422:     true,
		},
		{
			name:          "quick exit with auth-shaped stderr",
			elapsed:       time.Second,
			probeTimeout:  5 * time.Second,
			stderrContent: "HTTP 403: Forbidden",
			wantQuick:     true,
			wantAuth:      true,
		},
		{
			name:          "durable run still reports shape (caller gates on quick)",
			elapsed:       time.Minute,
			probeTimeout:  5 * time.Second,
			stderrContent: "HTTP 422",
			wantQuick:     false,
			wantIs422:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cls := classifyExit(tt.elapsed, tt.probeTimeout, tt.stderrContent)
			if cls.quick != tt.wantQuick {
				t.Errorf("quick = %v, want %v", cls.quick, tt.wantQuick)
			}
			if cls.is422 != tt.wantIs422 {
				t.Errorf("is422 = %v, want %v", cls.is422, tt.wantIs422)
			}
			if cls.authShaped != tt.wantAuth {
				t.Errorf("authShaped = %v, want %v", cls.authShaped, tt.wantAuth)
			}
		})
	}
}

func TestSleepOrStop_CompletesNormally(t *testing.T) {
	ok := sleepOrStop(context.Background(), make(chan struct{}), time.Millisecond)
	if !ok {
		t.Errorf("expected sleepOrStop to return true when the wait elapses normally")
	}
}

func TestSleepOrStop_CtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok := sleepOrStop(ctx, make(chan struct{}), time.Second)
	if ok {
		t.Errorf("expected sleepOrStop to return false when ctx is already cancelled")
	}
}

func TestSleepOrStop_StopChClosed(t *testing.T) {
	stopCh := make(chan struct{})
	close(stopCh)
	ok := sleepOrStop(context.Background(), stopCh, time.Second)
	if ok {
		t.Errorf("expected sleepOrStop to return false when stopCh is already closed")
	}
}
