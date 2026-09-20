package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// useWorkerRecordsDir scopes the worker record file to a fresh temp dir for
// one test and restores the previous override afterwards.
func useWorkerRecordsDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workers.json")
	prev := workerRecordsPathOverride
	workerRecordsPathOverride = path
	t.Cleanup(func() { workerRecordsPathOverride = prev })
	return path
}

func TestWorkerRecords_MissingFileIsEmpty(t *testing.T) {
	useWorkerRecordsDir(t)
	recs, err := allWorkerRecords()
	if err != nil || len(recs) != 0 {
		t.Fatalf("expected empty, got %v, %v", recs, err)
	}
}

func TestWorkerRecords_SchemaIsJSONArrayAndRoundTrips(t *testing.T) {
	path := useWorkerRecordsDir(t)
	in := workerRecord{ID: "42-1", PID: 42, Comm: "claude", LStart: "Sun Sep 20 22:09:00 2026",
		IssueNumber: 1814, Repo: "o/r", Stage: "Implement", SpawnedAt: time.Now().UTC().Truncate(time.Second)}
	if err := appendWorkerRecord(in); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("file is not a JSON array of objects: %v", err)
	}
	for _, k := range []string{"id", "pid", "issue_number", "stage", "spawned_at"} {
		if _, ok := raw[0][k]; !ok {
			t.Errorf("missing key %q in %s", k, data)
		}
	}
	out, err := allWorkerRecords()
	if err != nil || len(out) != 1 || out[0] != in {
		t.Fatalf("round trip mismatch: %+v, %v", out, err)
	}
}

func TestWorkerRecords_SamePIDDoesNotOverwrite(t *testing.T) {
	useWorkerRecordsDir(t)
	for _, id := range []string{"7-1", "7-2"} {
		if err := appendWorkerRecord(workerRecord{ID: id, PID: 7}); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := allWorkerRecords()
	if len(recs) != 2 {
		t.Fatalf("a recycled PID must get its own record, got %+v", recs)
	}
}

func TestWorkerRecords_RemoveAndUpdateByID(t *testing.T) {
	useWorkerRecordsDir(t)
	for i := 0; i < 3; i++ {
		if err := appendWorkerRecord(workerRecord{ID: fmt.Sprintf("r%d", i), PID: 100 + i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := updateWorkerRecord("r1", func(r *workerRecord) { r.Comm = "x"; r.LStart = "y" }); err != nil {
		t.Fatal(err)
	}
	if err := removeWorkerRecords([]string{"r0", "r2"}); err != nil {
		t.Fatal(err)
	}
	recs, _ := allWorkerRecords()
	if len(recs) != 1 || recs[0].ID != "r1" || recs[0].Comm != "x" || recs[0].LStart != "y" {
		t.Fatalf("unexpected records: %+v", recs)
	}
}

func TestWorkerRecords_ConcurrentAppends(t *testing.T) {
	useWorkerRecordsDir(t)
	var wg sync.WaitGroup
	const n = 25
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := appendWorkerRecord(workerRecord{ID: fmt.Sprintf("c%d", i), PID: i + 1}); err != nil {
				t.Errorf("append: %v", err)
			}
		}(i)
	}
	wg.Wait()
	recs, err := allWorkerRecords()
	if err != nil || len(recs) != n {
		t.Fatalf("expected %d records, got %d (%v)", n, len(recs), err)
	}
}

// A corrupt workers.json must be quarantined, not left to fail every append and
// every janitor pass forever.
func TestWorkerRecords_CorruptFileIsQuarantined(t *testing.T) {
	path := useWorkerRecordsDir(t)
	if err := os.WriteFile(path, []byte(`[{"id": "1-1", "pid"`), 0600); err != nil {
		t.Fatal(err)
	}
	recs, err := allWorkerRecords()
	if err != nil || len(recs) != 0 {
		t.Fatalf("expected empty set after quarantine, got %v, %v", recs, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file was not preserved for inspection: %v", err)
	}
	in := workerRecord{ID: "7-1", PID: 7, Stage: "Implement"}
	if err := appendWorkerRecord(in); err != nil {
		t.Fatalf("append after quarantine: %v", err)
	}
	recs, err = allWorkerRecords()
	if err != nil || len(recs) != 1 || recs[0].ID != in.ID {
		t.Fatalf("expected the new record to persist, got %v, %v", recs, err)
	}
}
