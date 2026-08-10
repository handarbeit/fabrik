package github

// Loader for the recorded-fixture format described in
// github/testdata/README.md (#1453 R2): each file under
// github/testdata/recordings/ is a JSON object of the shape
//
//	{"provenance": {...}, "response": {...}}
//
// where .response is the exact byte-for-byte GraphQL/REST response body
// GitHub returned (after scrubbing). loadRecording returns just the
// .response bytes, ready for an httptest handler to write back verbatim —
// tests that migrate to recordings don't hand-author response JSON anymore,
// they replay what GitHub actually sent.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// recordingProvenance documents where a fixture came from. Scrubbed must
// be true for every committed recording — see TestRecordings_AreScrubbed
// in scrub_recordings_test.go, which enforces this independent of what a
// fixture's author claims by re-running wirescrub.Findings over the raw
// file.
type recordingProvenance struct {
	Operation  string `json:"operation"`
	Endpoint   string `json:"endpoint"`
	RecordedAt string `json:"recorded_at"`
	SourceRepo string `json:"source_repo"`
	Scrubbed   bool   `json:"scrubbed"`
}

type recordingFile struct {
	Provenance recordingProvenance `json:"provenance"`
	Response   json.RawMessage     `json:"response"`
}

// loadRecording reads github/testdata/recordings/<operation>.json and
// returns its .response payload verbatim, for an httptest handler to serve
// in place of a hand-authored fixture literal.
func loadRecording(t *testing.T, operation string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "recordings", operation+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("loading recording %s: %v (run scripts/wire-contract/record-fixtures.sh to regenerate)", path, err)
	}
	var rec recordingFile
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parsing recording %s: %v", path, err)
	}
	if !rec.Provenance.Scrubbed {
		t.Fatalf("recording %s provenance.scrubbed is not true — refusing to serve an unverified fixture", path)
	}
	if rec.Provenance.Operation != operation {
		t.Fatalf("recording %s has provenance.operation %q, expected %q", path, rec.Provenance.Operation, operation)
	}
	return rec.Response
}
