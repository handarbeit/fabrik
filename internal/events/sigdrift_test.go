package events

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSigDrift_FiresOnExactlyThresholdOnce(t *testing.T) {
	var d SigDrift
	for i := 0; i < SignatureDriftThreshold-1; i++ {
		if d.RecordFailure() {
			t.Fatalf("failure %d fired before the threshold", i+1)
		}
	}
	if d.Active() {
		t.Fatal("active before threshold")
	}
	if !d.RecordFailure() {
		t.Fatal("threshold failure did not activate drift")
	}
	if !d.Active() {
		t.Fatal("not active after threshold")
	}
	for i := 0; i < 5; i++ {
		if d.RecordFailure() {
			t.Fatal("activation fired more than once per streak")
		}
	}
}

func TestSigDrift_SuccessResetsStreak(t *testing.T) {
	var d SigDrift
	for i := 0; i < SignatureDriftThreshold-1; i++ {
		d.RecordFailure()
	}
	if d.RecordSuccess() {
		t.Fatal("recovery reported without an active drift")
	}
	for i := 0; i < SignatureDriftThreshold-1; i++ {
		if d.RecordFailure() {
			t.Fatal("streak was not reset by the success")
		}
	}
}

func TestSigDrift_RecoveryFiresOnce(t *testing.T) {
	var d SigDrift
	for i := 0; i < SignatureDriftThreshold; i++ {
		d.RecordFailure()
	}
	if !d.RecordSuccess() {
		t.Fatal("first success after drift did not report recovery")
	}
	if d.RecordSuccess() {
		t.Fatal("recovery reported twice")
	}
	if d.Active() {
		t.Fatal("still active after recovery")
	}
}

func TestSigDrift_ConcurrentTransitionsFireOncePerStreak(t *testing.T) {
	var d SigDrift
	var activations int32
	var wg sync.WaitGroup
	for i := 0; i < SignatureDriftThreshold*10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.RecordFailure() {
				atomic.AddInt32(&activations, 1)
			}
		}()
	}
	wg.Wait()
	if activations != 1 {
		t.Fatalf("activations = %d, want exactly 1", activations)
	}
	var recoveries int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.RecordSuccess() {
				atomic.AddInt32(&recoveries, 1)
			}
		}()
	}
	wg.Wait()
	if recoveries != 1 {
		t.Fatalf("recoveries = %d, want exactly 1", recoveries)
	}
}
