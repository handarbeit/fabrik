package engine

import (
	"context"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// workerSlotsSampleInterval is how often the worker-slot publisher samples occupancy.
const workerSlotsSampleInterval = time.Second

// runWorkerSlotsPublisher samples the general pool and the reserved merge-train slot
// (#2115, ADR 2115) and emits a tui.WorkerSlotsEvent whenever the reading changes,
// until ctx ends. Sampling rather than hooking acquire/release keeps the dispatch code
// untouched and counts every general-slot holder, including a train that fell back to
// a general slot. Display only: nothing reads the value back.
func (e *Engine) runWorkerSlotsPublisher(ctx context.Context, interval time.Duration) {
	var last tui.WorkerSlotsEvent
	first := true
	emit := func() {
		trainInUse, trainMax := e.trainSlotUsage()
		cur := tui.WorkerSlotsEvent{InUse: len(e.sem), Max: cap(e.sem), TrainInUse: trainInUse, TrainMax: trainMax}
		if e.drainActive() {
			// #2259: the drain status rides the same sampled event.
			cur.Draining, cur.DrainLeft = true, len(e.drainWorkers())
		}
		if !first && cur == last {
			return
		}
		first, last = false, cur
		e.emitStructural(cur)
	}
	emit()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit()
		}
	}
}
