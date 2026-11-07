package engine

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
)

// quietClient is a mock client that records SetBoardRetryQuiet calls, the
// optional capability the real *gh.Client provides.
type quietClient struct {
	*mockGitHubClient
	quiet atomic.Bool
	flips atomic.Int32
}

func (q *quietClient) SetBoardRetryQuiet(v bool) {
	q.quiet.Store(v)
	q.flips.Add(1)
}

type emptyBoardEnv struct {
	eng    *Engine
	cache  *boardcache.CacheImpl
	client *quietClient
	items  atomic.Value // []gh.BoardProbeItem
	mu     sync.Mutex
	logs   []string
}

func (e *emptyBoardEnv) logLines() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.logs...)
}

func (e *emptyBoardEnv) count(sub string) int {
	n := 0
	for _, l := range e.logLines() {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func newEmptyBoardEnv(t *testing.T) *emptyBoardEnv {
	t.Helper()
	env := &emptyBoardEnv{}
	env.items.Store([]gh.BoardProbeItem(nil))
	mock := &mockGitHubClient{
		probeProjectBoardFn: func(owner, repo string, projectNum int, ownerType string) ([]gh.BoardProbeItem, string, error) {
			return env.items.Load().([]gh.BoardProbeItem), "PVT_1", nil
		},
	}
	env.client = &quietClient{mockGitHubClient: mock}
	env.eng = NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "u", Token: "t", MaxConcurrent: 5, Stages: testStagesWithCleanup()},
		env.client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()),
	)
	env.eng.SetLogSinkForTest(func(_ int, tag, msg string) {
		env.mu.Lock()
		env.logs = append(env.logs, tag+": "+msg)
		env.mu.Unlock()
	})
	env.cache = boardcache.NewCacheImpl(env.client, env.eng.store, func(string, ...any) {})
	env.eng.readClient = env.cache
	return env
}

func someItems() []gh.BoardProbeItem {
	return []gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Queued", EffectiveUpdatedAt: time.Now().Add(-time.Minute)},
	}
}

// A never-populated board that stays empty is concluded empty after N zero
// polls, with exactly one log line and no "deferring bootstrap" noise (#1894).
func TestEmptyBoard_ConcludesAfterConsecutiveZeroPolls(t *testing.T) {
	env := newEmptyBoardEnv(t)
	env.eng.refreshCacheForPoll(env.cache)
	if env.eng.boardConcludedEmpty() || env.client.quiet.Load() {
		t.Fatal("one zero poll must not conclude the board empty")
	}
	for i := 0; i < 5; i++ {
		env.eng.refreshCacheForPoll(env.cache)
	}
	if !env.eng.boardConcludedEmpty() {
		t.Fatal("board not concluded empty after consecutive zero polls")
	}
	if !env.client.quiet.Load() {
		t.Error("quiet single-attempt mode not enabled")
	}
	if n := env.count("board is empty — add issues to the board to start"); n != 1 {
		t.Errorf("empty-board line logged %d times, want exactly 1: %v", n, env.logLines())
	}
	if n := env.count("deferring bootstrap"); n != 0 {
		t.Errorf("deferral line logged %d times on a never-populated board, want 0", n)
	}
	if env.cache.IsBootstrapped() {
		t.Error("an empty board must not mark the cache bootstrapped")
	}
}

// An item added later is found with no restart; full retry is restored.
func TestEmptyBoard_ItemAddedLaterIsDiscovered(t *testing.T) {
	env := newEmptyBoardEnv(t)
	for i := 0; i < 3; i++ {
		env.eng.refreshCacheForPoll(env.cache)
	}
	if !env.eng.boardConcludedEmpty() {
		t.Fatal("precondition: concluded empty")
	}
	env.items.Store(someItems())
	env.eng.refreshCacheForPoll(env.cache)
	if !env.cache.IsBootstrapped() {
		t.Fatal("item added to a concluded-empty board was not discovered")
	}
	if env.eng.boardConcludedEmpty() || env.client.quiet.Load() {
		t.Error("full retry policy not restored after items appeared")
	}
	if n := env.count("board has items again"); n != 1 {
		t.Errorf("resume line logged %d times, want 1", n)
	}
}

// A board that held items never concludes empty: a later zero keeps the
// transient-fault protection and the deferral warning.
func TestEmptyBoard_NeverConcludedAfterItemsWereSeen(t *testing.T) {
	env := newEmptyBoardEnv(t)
	env.items.Store(someItems())
	env.eng.refreshCacheForPoll(env.cache)
	// Simulate a virgin cache again (e.g. store reset) but with items seen.
	env.eng.store.Reset(nil)
	env.items.Store([]gh.BoardProbeItem(nil))
	for i := 0; i < 6; i++ {
		env.eng.refreshCacheForPoll(env.cache)
	}
	if env.eng.boardConcludedEmpty() || env.client.quiet.Load() {
		t.Error("board that previously held items was concluded empty")
	}
	if env.count("board is empty") != 0 {
		t.Error("empty-board line logged for a previously populated board")
	}
	if env.count("deferring bootstrap") == 0 {
		t.Error("deferral warning missing for a previously populated board")
	}
}

// A transient zero at startup followed by real items bootstraps from the items
// and never leaves the board marked empty.
func TestEmptyBoard_TransientZeroThenItems(t *testing.T) {
	env := newEmptyBoardEnv(t)
	env.eng.refreshCacheForPoll(env.cache) // one zero
	env.items.Store(someItems())
	env.eng.refreshCacheForPoll(env.cache)
	if !env.cache.IsBootstrapped() {
		t.Fatal("cache not bootstrapped from the real items")
	}
	if env.eng.boardConcludedEmpty() || env.count("board is empty") != 0 {
		t.Error("board wrongly concluded empty")
	}
	// The zero-poll counter was reset: later zeros do not conclude.
	env.eng.store.Reset(nil)
	env.items.Store([]gh.BoardProbeItem(nil))
	env.eng.refreshCacheForPoll(env.cache)
	if env.eng.boardConcludedEmpty() {
		t.Error("concluded empty after items had been seen")
	}
}

func TestEmptyBoard_PollsOverride(t *testing.T) {
	env := newEmptyBoardEnv(t)
	env.eng.SetEmptyBoardPollsForTest(4)
	for i := 0; i < 3; i++ {
		env.eng.refreshCacheForPoll(env.cache)
	}
	if env.eng.boardConcludedEmpty() {
		t.Fatal("concluded before the overridden threshold")
	}
	env.eng.refreshCacheForPoll(env.cache)
	if !env.eng.boardConcludedEmpty() {
		t.Fatal("not concluded at the overridden threshold")
	}
}

// Concurrent bootstrap (transport goroutine) and poll goroutines must be
// race-free; run under -race.
func TestEmptyBoard_ConcurrentObservers(t *testing.T) {
	env := newEmptyBoardEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				env.eng.observeZeroItemPoll()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				env.eng.noteBoardItems()
				_ = env.eng.boardConcludedEmpty()
			}
		}()
	}
	wg.Wait()
}
