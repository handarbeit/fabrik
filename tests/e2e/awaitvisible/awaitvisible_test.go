package awaitvisible

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock advances only when Poll sleeps, so a never-visible wait ends without
// real time passing.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
}

func (c *fakeClock) spec(what string, timeout, interval time.Duration) Spec {
	return Spec{What: what, Timeout: timeout, Interval: interval, Now: c.now, Sleep: c.sleep}
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestPollVisibleOnFirstRead(t *testing.T) {
	c := newClock()
	reads := 0
	res := Poll(c.spec("x", time.Minute, 10*time.Second), func() (bool, string, error) {
		reads++
		return true, "there", nil
	})
	if !res.Visible || res.Attempts != 1 || reads != 1 || len(c.sleeps) != 0 {
		t.Fatalf("want visible on first read with no sleep, got %+v reads=%d sleeps=%v", res, reads, c.sleeps)
	}
	if res.TimedOut() {
		t.Fatal("a visible result must not report TimedOut")
	}
}

func TestPollVisibleAfterNReads(t *testing.T) {
	c := newClock()
	reads := 0
	res := Poll(c.spec("x", time.Minute, 10*time.Second), func() (bool, string, error) {
		reads++
		return reads == 4, "read", nil
	})
	if !res.Visible || res.Attempts != 4 || len(c.sleeps) != 3 {
		t.Fatalf("want visible on 4th read after 3 sleeps, got %+v sleeps=%v", res, c.sleeps)
	}
	for _, s := range c.sleeps {
		if s != 10*time.Second {
			t.Fatalf("sleep = %s, want the explicit interval", s)
		}
	}
}

func TestPollNeverVisibleTimesOutWithReason(t *testing.T) {
	c := newClock()
	res := Poll(c.spec("board item o/r#7 in the ProjectV2 listing", 25*time.Second, 10*time.Second), func() (bool, string, error) {
		return false, "listing had 8 items, none was #7", nil
	})
	if res.Visible || !res.TimedOut() {
		t.Fatalf("want timed out, got %+v", res)
	}
	// 10s + 10s + clipped 5s, with a read before each sleep and a final read at the deadline.
	if res.Attempts != 4 || res.Elapsed != 25*time.Second {
		t.Fatalf("want 4 reads over 25s (the last sleep clipped to the deadline), got attempts=%d elapsed=%s sleeps=%v", res.Attempts, res.Elapsed, c.sleeps)
	}
	reason := res.Reason()
	for _, want := range []string{"board item o/r#7 in the ProjectV2 listing", "25s", "listing had 8 items, none was #7"} {
		if !strings.Contains(reason, want) {
			t.Errorf("Reason() = %q, missing %q", reason, want)
		}
	}
}

func TestPollTransientReadErrorIsRetriedNotTreatedAsNotVisible(t *testing.T) {
	c := newClock()
	reads := 0
	var retried []error
	spec := c.spec("x", time.Minute, 10*time.Second)
	spec.OnRetry = func(err error) { retried = append(retried, err) }
	res := Poll(spec, func() (bool, string, error) {
		reads++
		if reads < 3 {
			return false, "", errors.New("gh: HTTP 502")
		}
		return true, "ok", nil
	})
	if !res.Visible || res.Attempts != 3 || len(retried) != 2 {
		t.Fatalf("want visible after two retried errors, got %+v retried=%v", res, retried)
	}
	if res.LastErr != nil {
		t.Fatalf("a later successful read must clear LastErr, got %v", res.LastErr)
	}
}

func TestPollTimeoutReasonCarriesLastReadError(t *testing.T) {
	c := newClock()
	res := Poll(c.spec("x", 20*time.Second, 10*time.Second), func() (bool, string, error) {
		return false, "", errors.New("gh: HTTP 502")
	})
	if !res.TimedOut() || res.LastErr == nil {
		t.Fatalf("want a timeout that kept the last read error, got %+v", res)
	}
	if !strings.Contains(res.Reason(), "HTTP 502") {
		t.Errorf("Reason() = %q, want it to name the last read error", res.Reason())
	}
}

func TestPollAbortStopsAtOnceAndIsNotATimeout(t *testing.T) {
	c := newClock()
	boom := errors.New("state is dirty")
	reads := 0
	res := Poll(c.spec("x", time.Minute, 10*time.Second), func() (bool, string, error) {
		reads++
		return false, "dirty", Abort(boom)
	})
	if reads != 1 || !errors.Is(res.Aborted, boom) || res.TimedOut() || res.Visible {
		t.Fatalf("want one read and Aborted=%v, got %+v reads=%d", boom, res, reads)
	}
}

func TestPollRequiresExplicitTimeoutAndInterval(t *testing.T) {
	for _, s := range []Spec{{What: "x", Interval: time.Second}, {What: "x", Timeout: time.Second}} {
		called := false
		res := Poll(s, func() (bool, string, error) { called = true; return true, "", nil })
		if called || res.Aborted == nil || res.TimedOut() {
			t.Fatalf("spec %+v: want a configuration error without probing, got %+v called=%v", s, res, called)
		}
	}
}

func TestPollLastDetailTracksTheLatestObservation(t *testing.T) {
	c := newClock()
	n := 0
	res := Poll(c.spec("x", 15*time.Second, 10*time.Second), func() (bool, string, error) {
		n++
		return false, strings.Repeat("a", n), nil
	})
	if res.LastDetail != strings.Repeat("a", res.Attempts) {
		t.Fatalf("LastDetail = %q after %d reads", res.LastDetail, res.Attempts)
	}
}

func TestParseClosingLinkage(t *testing.T) {
	const both = `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":9},{"number":12}]}},"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":5}]}}}}}`
	got, err := ParseClosingLinkage([]byte(both), 5, 12)
	if err != nil || !got.Both() {
		t.Fatalf("both sides visible: got %+v err=%v", got, err)
	}
	const issueOnly = `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":12}]}},"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`
	got, err = ParseClosingLinkage([]byte(issueOnly), 5, 12)
	if err != nil || got.Both() || !got.IssueSide || got.PRSide {
		t.Fatalf("issue side only: got %+v err=%v", got, err)
	}
	const none = `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[]}},"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`
	got, err = ParseClosingLinkage([]byte(none), 5, 12)
	if err != nil || got.IssueSide || got.PRSide {
		t.Fatalf("neither side: got %+v err=%v", got, err)
	}
}

func TestParseClosingLinkageErrorsAreReadFailuresNotAbsence(t *testing.T) {
	for name, out := range map[string]string{
		"not json":     `HTTP 502 Bad Gateway`,
		"graphql err":  `{"errors":[{"message":"rate limited"}]}`,
		"null issue":   `{"data":{"repository":{"issue":null,"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`,
		"null repo":    `{"data":{"repository":null}}`,
		"null the PR":  `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[]}},"pullRequest":null}}}`,
		"empty output": ``,
	} {
		if _, err := ParseClosingLinkage([]byte(out), 5, 12); err == nil {
			t.Errorf("%s: want an error (a failed read is retried), got none", name)
		}
	}
}

func TestClassifyMergeable(t *testing.T) {
	cases := map[string]MergeableVerdict{
		"":         MergeablePending,
		"unknown":  MergeablePending,
		"clean":    MergeableSettled,
		"unstable": MergeableSettled,
		"blocked":  MergeableWaitable,
		"draft":    MergeableWaitable,
		"dirty":    MergeableFatal,
		"behind":   MergeableFatal,
	}
	for state, want := range cases {
		if got := ClassifyMergeable(state); got != want {
			t.Errorf("ClassifyMergeable(%q) = %v, want %v", state, got, want)
		}
	}
	if MergeableComputed("unknown") || !MergeableComputed("blocked") || !MergeableComputed("dirty") {
		t.Error("MergeableComputed must be true for every state except unknown/empty")
	}
}
