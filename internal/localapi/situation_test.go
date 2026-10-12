package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

type fakeSituation struct {
	got   SituationParams
	calls int
}

func (f *fakeSituation) Situation(p SituationParams) (*SituationResult, error) {
	f.calls++
	f.got = p
	if p.Subscriber == "bad" {
		return nil, Errorf(CodeBadRequest, "bad subscriber")
	}
	return &SituationResult{View: SituationView{NothingNeedsYou: true}, Pushed: p.Push}, nil
}

func startSituationServer(t *testing.T, sb SituationBackend) *Server {
	t.Helper()
	path := filepath.Join(shortDir(t), "s.sock")
	srv := NewServer(path, echoBackend(), nil)
	srv.Situation = sb
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func TestSituationMethodAdvertisedOnlyWithBackend(t *testing.T) {
	if slices.Contains(helloMethods(t, startServer(t, echoBackend())), MethodSituation) {
		t.Error("hello advertises situation with no SituationBackend")
	}
	if !slices.Contains(helloMethods(t, startSituationServer(t, &fakeSituation{})), MethodSituation) {
		t.Error("hello does not advertise situation")
	}
}

func TestSituationUnknownMethodWithoutBackend(t *testing.T) {
	srv := startServer(t, echoBackend())
	err := Call(context.Background(), srv.Path(), MethodSituation, nil, &SituationResult{})
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeUnknownMethod {
		t.Fatalf("err = %v, want unknown_method", err)
	}
}

func TestSituationRoundTrip(t *testing.T) {
	f := &fakeSituation{}
	srv := startSituationServer(t, f)
	var res SituationResult
	params := SituationParams{Subscriber: "ov", Push: true, Force: true}
	if err := Call(context.Background(), srv.Path(), MethodSituation, params, &res); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if f.got != params {
		t.Errorf("params = %+v, want %+v", f.got, params)
	}
	if !res.View.NothingNeedsYou || !res.Pushed {
		t.Errorf("result = %+v", res)
	}
	// No params at all is the plain read.
	f.calls = 0
	if err := Call(context.Background(), srv.Path(), MethodSituation, nil, &res); err != nil || f.calls != 1 {
		t.Fatalf("plain read: err=%v calls=%d", err, f.calls)
	}
	// A backend *Error is passed through as-is.
	err := Call(context.Background(), srv.Path(), MethodSituation, SituationParams{Subscriber: "bad"}, &res)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeBadRequest {
		t.Fatalf("err = %v, want bad_request", err)
	}
	// Params of the wrong shape are a bad_request, not a panic.
	err = Call(context.Background(), srv.Path(), MethodSituation, map[string]any{"push": "yes"}, &res)
	if !errors.As(err, &pe) || pe.Code != CodeBadRequest {
		t.Fatalf("bad params err = %v, want bad_request", err)
	}
}

func TestSituationViewMarshalsUnknownAsText(t *testing.T) {
	v := SituationView{}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	h := back["health"].(map[string]any)["poll"].(map[string]any)
	if h["last_attempt_at"] != UnknownText {
		t.Errorf("an unset poll time must marshal as %q, got %v", UnknownText, h["last_attempt_at"])
	}
	if back["merge_decisions"].(map[string]any)["omitted"] != float64(0) {
		t.Errorf("merge_decisions = %v", back["merge_decisions"])
	}
}
