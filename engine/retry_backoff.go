package engine

import "time"

// Two timing concepts used to share one expression, 10 × PollSeconds, and so
// were silently coupled (#1831). They answer different questions and must not
// be conflated again:
//
//   - stageRetryBackoff: how long to wait before re-dispatching a stage after
//     an attempt that did not complete. Its only job is to stop a stage that
//     exits incomplete immediately from hot-looping. It is NOT a fairness or
//     runaway control — those are max_turns (per attempt, which also releases
//     the worker slot) and max_retries (across attempts). It costs no GitHub
//     API calls beyond the retry itself, so it must not scale with poll.
//
//   - githubRecheckInterval: how often to re-read from GitHub an item we will
//     not be notified about (a dependency closing, a silent bot reviewer, a
//     terminal item's periodic re-eval), and how long to back off after a
//     failed read. This one genuinely is about API cost, so it scales with
//     poll — which operators raise precisely to protect a shared rate limit.
//
// Before #1831, raising poll from 30 to 180 to protect the GraphQL budget
// silently turned a 5-minute stage retry into a 30-minute one.

// stageRetryBackoff returns the delay before a stage is re-dispatched after an
// incomplete attempt. It is read by the dispatch gate, processItem's gate, and
// the "will retry after" message, so all three always agree.
//
// A zero Config.RetryBackoff means unset: the CLI always supplies a value
// (default 60s), so this fallback is reached only by code that builds a Config
// directly (tests, embedders), which keeps their existing timing unchanged.
func (e *Engine) stageRetryBackoff() time.Duration {
	if e.cfg.RetryBackoff > 0 {
		return e.cfg.RetryBackoff
	}
	return e.githubRecheckInterval()
}

// githubRecheckInterval returns the cadence for re-reading an item from GitHub
// when nothing will notify us of a change, and the back-off after a failed
// read. Deliberately coupled to PollSeconds — see the file comment.
func (e *Engine) githubRecheckInterval() time.Duration {
	return time.Duration(e.cfg.PollSeconds*10) * time.Second
}
