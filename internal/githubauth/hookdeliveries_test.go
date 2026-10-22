package githubauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReconcilerHookDeliveries_NoCredentialsMakesNoCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	for name, r := range map[string]*Reconciler{
		"nil":       nil,
		"zero":      {},
		"no key":    {appID: 7, baseURL: srv.URL},
		"no app id": {baseURL: srv.URL},
	} {
		if _, _, err := r.ListAppHookDeliveries("", 100); err == nil {
			t.Errorf("%s: List: expected error", name)
		}
		if err := r.RedeliverAppHookDelivery(1); err == nil {
			t.Errorf("%s: Redeliver: expected error", name)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("network calls = %d, want 0", calls.Load())
	}
}

func TestReconcilerHookDeliveries_AgainstStub(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	var listed, redelivered atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing Authorization")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/app/hook/deliveries":
			listed.Add(1)
			json.NewEncoder(w).Encode([]map[string]any{{"id": 9, "guid": "g", "status_code": 500}})
		case r.Method == "POST" && r.URL.Path == "/app/hook/deliveries/9/attempts":
			redelivered.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	r := &Reconciler{appID: 7, privateKey: key, baseURL: srv.URL}
	items, next, err := r.ListAppHookDeliveries("", 100)
	if err != nil || next != "" || len(items) != 1 || items[0].ID != 9 {
		t.Fatalf("List: items=%+v next=%q err=%v", items, next, err)
	}
	if err := r.RedeliverAppHookDelivery(9); err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if listed.Load() != 1 || redelivered.Load() != 1 {
		t.Errorf("listed=%d redelivered=%d", listed.Load(), redelivered.Load())
	}
}
