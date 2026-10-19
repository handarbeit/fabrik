package github

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// writeStamps records, per issue and per project item, the latest GitHub-reported
// time of a write this client completed (#1945, ADR 1945). The engine reads it
// right after a successful self-write to anchor the probe staleness baseline
// (LastSeenSourceUpdatedAt) to GitHub's clock instead of the daemon's.
//
// It holds only stamps of completed engine writes and only ever raises a value,
// so a reader that races a concurrent write to the same item can at worst see
// another engine write's stamp — which is exactly what a self-write anchor is.
// A write that yields no stamp records nothing. Display-field writes
// (UpdateProjectItemTextField, the date field writes, ClearProjectItemField)
// never record: they must not anchor the baseline (#2180).
type writeStamps struct {
	mu     sync.Mutex
	issues map[string]time.Time // "owner/repo#N"
	items  map[string]time.Time // project item node ID
}

func issueStampKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

func (w *writeStamps) raise(m *map[string]time.Time, key string, t time.Time) {
	if t.IsZero() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if *m == nil {
		*m = make(map[string]time.Time)
	}
	if t.After((*m)[key]) {
		(*m)[key] = t
	}
}

// recordIssueWrite raises the stamp of owner/repo#number. Zero is ignored.
func (c *Client) recordIssueWrite(owner, repo string, number int, t time.Time) {
	c.stamps.raise(&c.stamps.issues, issueStampKey(owner+"/"+repo, number), t)
}

// recordItemWrite raises the stamp of a project item. Zero is ignored.
func (c *Client) recordItemWrite(itemID string, t time.Time) {
	if itemID == "" {
		return
	}
	c.stamps.raise(&c.stamps.items, itemID, t)
}

// IssueWriteStamp returns GitHub's latest reported time for a write this client
// made to repo ("owner/repo") #number, or the zero time when none is known.
func (c *Client) IssueWriteStamp(repo string, number int) time.Time {
	c.stamps.mu.Lock()
	defer c.stamps.mu.Unlock()
	return c.stamps.issues[issueStampKey(repo, number)]
}

// ProjectItemWriteStamp returns GitHub's latest reported time for a Status write
// this client made to the project item, or the zero time when none is known.
func (c *Client) ProjectItemWriteStamp(itemID string) time.Time {
	c.stamps.mu.Lock()
	defer c.stamps.mu.Unlock()
	return c.stamps.items[itemID]
}

// responseDate parses the HTTP Date header — GitHub's own clock, one-second
// granularity. Zero when absent or unparseable. Never the local clock.
func responseDate(h http.Header) time.Time {
	if h == nil {
		return time.Time{}
	}
	v := strings.TrimSpace(h.Get("Date"))
	if v == "" {
		return time.Time{}
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// latestOf returns the latest of the given times (zero when all are zero).
func latestOf(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
