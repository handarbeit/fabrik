package engine

import "github.com/handarbeit/fabrik/internal/localapi"

// OverseerActorForTest returns the overseer action implementation (#1969,
// ADR-1966-c) so tests/sim, which drives the engine through PollOnce and never
// reaches startLocalAPI, can call an action and then poll. Production wires the
// same value into the local API server from startLocalAPI; nothing else calls
// this.
func (e *Engine) OverseerActorForTest() localapi.Actor { return e.overseerActor() }
