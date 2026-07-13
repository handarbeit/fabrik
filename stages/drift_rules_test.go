package stages

import "testing"

func TestIsKillGraceNoOp(t *testing.T) {
	tests := []struct {
		name   string
		stage  *Stage
		wantOK bool
	}{
		{
			name:   "both empty",
			stage:  &Stage{},
			wantOK: true,
		},
		{
			name:   "both explicit 10s",
			stage:  &Stage{KillGrace: KillGrace{SigIntRaw: "10s", SigTermRaw: "10s"}},
			wantOK: true,
		},
		{
			name:   "sigint empty, sigterm explicit 10s",
			stage:  &Stage{KillGrace: KillGrace{SigTermRaw: "10s"}},
			wantOK: true,
		},
		{
			name:   "sigint explicit 0s is meaningful, not a no-op",
			stage:  &Stage{KillGrace: KillGrace{SigIntRaw: "0s", SigTermRaw: "10s"}},
			wantOK: false,
		},
		{
			name:   "sigterm explicit 0s is meaningful, not a no-op",
			stage:  &Stage{KillGrace: KillGrace{SigIntRaw: "10s", SigTermRaw: "0s"}},
			wantOK: false,
		},
		{
			name:   "sigint explicit 30s differs from default",
			stage:  &Stage{KillGrace: KillGrace{SigIntRaw: "30s", SigTermRaw: "10s"}},
			wantOK: false,
		},
		{
			name:   "unparseable raw string is treated as meaningful",
			stage:  &Stage{KillGrace: KillGrace{SigIntRaw: "not-a-duration"}},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isKillGraceNoOp(tt.stage); got != tt.wantOK {
				t.Errorf("isKillGraceNoOp(%+v) = %v, want %v", tt.stage.KillGrace, got, tt.wantOK)
			}
		})
	}
}

func TestIsCompletionNoOp(t *testing.T) {
	tests := []struct {
		name   string
		stage  *Stage
		wantOK bool
	}{
		{
			name:   "empty type",
			stage:  &Stage{},
			wantOK: true,
		},
		{
			name:   "explicit claude",
			stage:  &Stage{Completion: CompletionCriteria{Type: "claude"}},
			wantOK: true,
		},
		{
			name:   "some other type would be meaningful",
			stage:  &Stage{Completion: CompletionCriteria{Type: "other"}},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCompletionNoOp(tt.stage); got != tt.wantOK {
				t.Errorf("isCompletionNoOp(%+v) = %v, want %v", tt.stage.Completion, got, tt.wantOK)
			}
		})
	}
}

func TestFilterNoOpKeys(t *testing.T) {
	noOpDefault := &Stage{
		KillGrace:  KillGrace{SigIntRaw: "10s", SigTermRaw: "10s"},
		Completion: CompletionCriteria{Type: "claude"},
	}
	meaningfulDefault := &Stage{
		KillGrace:  KillGrace{SigIntRaw: "0s", SigTermRaw: "10s"},
		Completion: CompletionCriteria{Type: "claude"},
	}

	t.Run("filters registered no-op keys", func(t *testing.T) {
		got := FilterNoOpKeys([]string{"completion", "kill_grace", "wait_for_ci"}, noOpDefault)
		want := []string{"wait_for_ci"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("FilterNoOpKeys = %v, want %v", got, want)
		}
	})

	t.Run("keeps meaningful kill_grace", func(t *testing.T) {
		got := FilterNoOpKeys([]string{"kill_grace"}, meaningfulDefault)
		if len(got) != 1 || got[0] != "kill_grace" {
			t.Errorf("FilterNoOpKeys = %v, want [kill_grace]", got)
		}
	})

	t.Run("never filters an unregistered key", func(t *testing.T) {
		got := FilterNoOpKeys([]string{"wait_for_ci", "wait_for_reviews"}, noOpDefault)
		if len(got) != 2 {
			t.Errorf("FilterNoOpKeys = %v, want both keys retained", got)
		}
	})

	t.Run("nil default stage returns missing unchanged", func(t *testing.T) {
		in := []string{"kill_grace", "completion"}
		got := FilterNoOpKeys(in, nil)
		if len(got) != 2 {
			t.Errorf("FilterNoOpKeys with nil default = %v, want unchanged %v", got, in)
		}
	})

	t.Run("empty missing returns empty", func(t *testing.T) {
		got := FilterNoOpKeys(nil, noOpDefault)
		if len(got) != 0 {
			t.Errorf("FilterNoOpKeys(nil, ...) = %v, want empty", got)
		}
	})
}
