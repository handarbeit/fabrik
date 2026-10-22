package events

import "sync"

// SignatureDriftThreshold is the number of consecutive signature-check
// failures (with zero interleaved successes) that mark a transport as being in
// signature drift — see ADR-1563. Chosen as a plain count rather than a
// time-based rolling rate: deterministic to test (a count crosses its
// threshold on exactly one delivery) and a healthy mix never triggers it for
// any interleaving. Shared by the Hookdeck source and the direct App webhook
// transport (ADR 2142).
const SignatureDriftThreshold = 20

// SigDrift counts consecutive signature-verification failures and reports the
// two transitions of a drift episode: the failure that first reaches
// SignatureDriftThreshold, and the first success after it. It is safe for
// concurrent use (an HTTP transport verifies on one goroutine per request);
// the transition decision is taken under the lock and returned, so the caller
// fires its callbacks outside it and each streak yields exactly one
// activation and one recovery.
type SigDrift struct {
	mu          sync.Mutex
	consecutive int
	active      bool
}

// RecordFailure counts one signature failure and reports whether this failure
// moved the tracker into drift (true exactly once per streak).
func (d *SigDrift) RecordFailure() (becameActive bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.consecutive++
	if d.consecutive >= SignatureDriftThreshold && !d.active {
		d.active = true
		return true
	}
	return false
}

// RecordSuccess resets the streak and reports whether a drift episode was
// active (true exactly once per episode: the recovery signal).
func (d *SigDrift) RecordSuccess() (recovered bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.consecutive = 0
	if d.active {
		d.active = false
		return true
	}
	return false
}

// Active reports whether a drift episode is currently open.
func (d *SigDrift) Active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}
