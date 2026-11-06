package appwebhook

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/warnings"
)

// SC-006: a consumer written only against this package's interfaces (doubles for
// the sink, the panel and the health sink; no engine code) runs the receiver end
// to end: a signed delivery reaches the sink and the health sink sees it.
type recordingPanel struct {
	mu       sync.Mutex
	reported []warnings.Condition
	resolved []string
}

func (p *recordingPanel) Report(c warnings.Condition) {
	p.mu.Lock()
	p.reported = append(p.reported, c)
	p.mu.Unlock()
}
func (p *recordingPanel) Resolve(key string) {
	p.mu.Lock()
	p.resolved = append(p.resolved, key)
	p.mu.Unlock()
}
func (p *recordingPanel) Reconcile(string, []warnings.Condition) {}

var _ Panel = (*recordingPanel)(nil)

func TestConsumerDoublesRunTheReceiverEndToEnd(t *testing.T) {
	var (
		mu     sync.Mutex
		got    []Delivery
		states []string
		counts map[string]int
	)
	var sink Sink = func(d Delivery) {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
	}
	var health HealthSink = func(state string, c map[string]int, _ string) {
		mu.Lock()
		states = append(states, state)
		counts = c
		mu.Unlock()
	}
	r := New(Config{
		Name:   "double",
		Path:   directTestPath,
		Listen: DirectListener("127.0.0.1:0"),
		Secret: directTestSecret,
		Logf:   func(int, string, string, ...any) {},
		Health: health,
	})
	r.SetSink(sink)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)

	body := []byte(`{"action":"opened"}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.Addr()+directTestPath, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", signDirect(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "d-double")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].ID != "d-double" || got[0].Transport != "double" || got[0].EventType != "issues" {
		t.Fatalf("sink got %#v", got)
	}
	if len(states) == 0 || counts["issues"] != 1 {
		t.Fatalf("health sink states=%v counts=%v", states, counts)
	}
}

// A consumer that supplies no Logf must not panic: Start logs the state change
// and a bad-signature request logs too.
func TestNilLogfDoesNotPanic(t *testing.T) {
	r := New(Config{
		Name:   "nolog",
		Path:   directTestPath,
		Listen: DirectListener("127.0.0.1:0"),
		Secret: directTestSecret,
	})
	r.SetSink(func(Delivery) {})
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.Addr()+directTestPath, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
