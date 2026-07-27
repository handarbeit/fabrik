package pruefer

import (
	"context"
	"sync"
)

// mockClaudeInvoker implements ClaudeInvoker for testing, mirroring
// engine/mocks_test.go's mockClaudeInvoker shape: an injectable fn plus a
// mutex-protected call log, safe for use from concurrent tests.
type mockClaudeInvoker struct {
	mu    sync.Mutex
	fn    func(req ReviewRequest) (string, error)
	calls []ReviewRequest
}

func (m *mockClaudeInvoker) Review(ctx context.Context, req ReviewRequest) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, req)
	fn := m.fn
	m.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return "mock review", nil
}

func (m *mockClaudeInvoker) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *mockClaudeInvoker) callsSnapshot() []ReviewRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ReviewRequest, len(m.calls))
	copy(out, m.calls)
	return out
}
