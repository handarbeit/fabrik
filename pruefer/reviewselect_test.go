package pruefer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const selStream = `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"s1","name":"Skill","input":{"skill":"mine:deep"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"s1","is_error":false,"content":"loaded"}]}}
{"type":"result","result":"selected review","is_error":false,"num_turns":4,"total_cost_usd":0.04}`

const selLoadFail = `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"s1","name":"Skill","input":{"skill":"mine:deep"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"s1","is_error":true,"content":"Unknown skill /tmp/secret/path"}]}}
{"type":"result","result":"selected but unloaded","is_error":false,"num_turns":4,"total_cost_usd":0.04}`

const selNeverInvoked = `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"/x"}}]}}
{"type":"result","result":"never called the skill","is_error":false,"num_turns":4,"total_cost_usd":0.04}`

const defStream = `{"type":"system","subtype":"init"}
{"type":"assistant","parent_tool_use_id":null,"message":{"content":[{"type":"tool_use","id":"d1","name":"Skill","input":{"skill":"pruefer-review:review"}}]}}
{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":"d1","is_error":false,"content":"loaded"}]}}
{"type":"result","result":"default review","is_error":false,"num_turns":3,"total_cost_usd":0.03}`

// fakeClaudeBySkill installs a fake claude that appends one line of arguments to
// rec.calls per invocation and answers from selFile when the invocation grants
// Skill(mine:deep) (the selected skill), from defFile otherwise (the default).
func fakeClaudeBySkill(t *testing.T, rec, selected, def string) {
	t.Helper()
	selFile, defFile := rec+".sel", rec+".def"
	if err := os.WriteFile(selFile, []byte(selected+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defFile, []byte(def+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeClaude(t, `echo "$*" >> "`+rec+`.calls"
case "$*" in
  *"Skill(mine:deep)"*) cat "`+selFile+`" ;;
  *) cat "`+defFile+`" ;;
esac
`)
}

func recordedCalls(t *testing.T, rec string) []string {
	t.Helper()
	b, err := os.ReadFile(rec + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// selReqOp builds a request selecting skill, with an operator plugin "mine"
// (skill "deep") on disk; it returns that plugin's directory too.
func selReqOp(t *testing.T, skill string) (ReviewRequest, string) {
	t.Helper()
	op := writeTestPlugin(t, filepath.Join(t.TempDir(), "mine-dir"), "mine", "1.0.0", "deep")
	return ReviewRequest{PRNumber: 1, WorkDir: t.TempDir(), ReviewSkill: skill, BaseSHA: "basesha", OperatorPluginDirs: []string{op}}, op
}

func selReq(t *testing.T, skill string) ReviewRequest {
	req, _ := selReqOp(t, skill)
	return req
}

func TestReview_OperatorPluginSkillRuns_NoFallback(t *testing.T) {
	logs := capturePluginLogs(t)
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec, selStream, defStream)
	req, op := selReqOp(t, "mine:deep")
	res, err := (&RealClaudeInvoker{}).Review(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "selected review" || res.FallbackFrom != "" {
		t.Errorf("res = %+v", res)
	}
	calls := recordedCalls(t, rec)
	if len(calls) != 1 {
		t.Fatalf("claude ran %d times, want 1", len(calls))
	}
	if !strings.Contains(calls[0], "Skill(mine:deep)") || strings.Contains(calls[0], "Skill(pruefer-review:review)") {
		t.Errorf("the grant must name the selected skill only: %s", calls[0])
	}
	if strings.Count(calls[0], "--plugin-dir") != 2 || strings.Index(calls[0], "pruefer-review") > strings.Index(calls[0], op) {
		t.Errorf("want the embedded plugin then the operator plugin as --plugin-dir: %s", calls[0])
	}
	if countContaining(logs(), "review plugin: mine v1.0.0 (operator,") != 1 || countContaining(logs(), "review plugin: pruefer-review v") != 1 {
		t.Errorf("plugin name/version log lines missing: %q", logs())
	}
	if countContaining(logs(), "review skill: mine:deep (plugin mine v1.0.0, operator,") != 1 {
		t.Errorf("skill log line missing: %q", logs())
	}
}

func TestReview_UnresolvedOrMalformedSelectionFallsBackVisiblyWithoutAnExtraRun(t *testing.T) {
	for name, tc := range map[string]struct{ ref, reason string }{
		"unknown plugin": {"nowhere:deep", `plugin "nowhere" is not loaded`},
		"unknown skill":  {"mine:absent", `plugin "mine" has no skill "absent"`},
		"bare name":      {"deep", "not a plugin:skill reference"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "rec")
			fakeClaudeBySkill(t, rec, selStream, defStream)
			res, err := (&RealClaudeInvoker{}).Review(context.Background(), selReq(t, tc.ref))
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != "default review" || res.FallbackFrom != tc.ref || !strings.Contains(res.FallbackReason, tc.reason) {
				t.Errorf("res = %+v, want a default review falling back from %q because %q", res, tc.ref, tc.reason)
			}
			calls := recordedCalls(t, rec)
			if len(calls) != 1 || !strings.Contains(calls[0], "Skill(pruefer-review:review)") || strings.Contains(calls[0], "Skill(mine:deep)") {
				t.Errorf("want exactly one default-skill run: %q", calls)
			}
		})
	}
}

func TestReview_SelectedSkillThatDoesNotRunReRunsTheDefault(t *testing.T) {
	for name, stream := range map[string]struct{ stream, reason string }{
		"load failure": {selLoadFail, "it failed to load"},
		"never called": {selNeverInvoked, "the reviewer never invoked it"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := capturePluginLogs(t)
			rec := filepath.Join(t.TempDir(), "rec")
			fakeClaudeBySkill(t, rec, stream.stream, defStream)
			res, err := (&RealClaudeInvoker{}).Review(context.Background(), selReq(t, "mine:deep"))
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != "default review" || res.FallbackFrom != "mine:deep" || res.FallbackReason != stream.reason {
				t.Errorf("res = %+v", res)
			}
			if strings.Contains(res.FallbackReason, "secret") {
				t.Errorf("skill output leaked into the reason: %q", res.FallbackReason)
			}
			calls := recordedCalls(t, rec)
			if len(calls) != 2 || !strings.Contains(calls[0], "Skill(mine:deep)") || !strings.Contains(calls[1], "Skill(pruefer-review:review)") || strings.Contains(calls[1], "Skill(mine:deep)") {
				t.Fatalf("want the selected run then a default-only run: %q", calls)
			}
			if res.NumTurns != 7 || res.CostUSD < 0.0699 || res.CostUSD > 0.0701 {
				t.Errorf("turns/cost = %d/%v, want both runs summed (7, 0.07)", res.NumTurns, res.CostUSD)
			}
			if countContaining(logs(), `review skill "mine:deep" at base basesha not used`) != 1 {
				t.Errorf("fallback not logged: %q", logs())
			}
		})
	}
}

func TestReview_NoFallbackForNonSkillFailuresOrAFailingDefault(t *testing.T) {
	// A process error on the selected run is not a skill failure: nothing is
	// posted and no second run happens.
	rec := filepath.Join(t.TempDir(), "rec")
	writeFakeClaude(t, `echo "$*" >> "`+rec+`.calls"`+"\nexit 1\n")
	if _, err := (&RealClaudeInvoker{}).Review(context.Background(), selReq(t, "mine:deep")); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(recordedCalls(t, rec)); n != 1 {
		t.Errorf("a process error ran claude %d times, want 1", n)
	}

	// The default skill itself not running keeps ADR 2306 decision 8: an error,
	// no second run, no fallback.
	rec2 := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec2, selStream, selNeverInvoked)
	if res, err := (&RealClaudeInvoker{}).Review(context.Background(), selReq(t, "")); err == nil {
		t.Errorf("a default skill that never ran must fail the review, got %+v", res)
	}
	if n := len(recordedCalls(t, rec2)); n != 1 {
		t.Errorf("a failing default skill ran claude %d times, want 1", n)
	}
}

func TestReview_FallbackReasonCarriesOperatorPluginNotes(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec, selStream, defStream)
	req := selReq(t, "gone:deep")
	// An operator plugin declaring the embedded plugin's name is skipped and
	// reported, and the report reaches the fallback reason.
	req.OperatorPluginDirs = append(req.OperatorPluginDirs, writeTestPlugin(t, filepath.Join(t.TempDir(), "res"), "pruefer-review", "9", "review"))
	res, err := (&RealClaudeInvoker{}).Review(context.Background(), req)
	if err != nil || res.FallbackFrom == "" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if !strings.Contains(res.FallbackReason, "reserved for the embedded plugin") {
		t.Errorf("reason = %q lacks the plugin note", res.FallbackReason)
	}
}

func TestReview_PerReviewDirectoryRemovedAfterFallback(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	fakeClaudeBySkill(t, rec, selLoadFail, defStream)
	req, op := selReqOp(t, "mine:deep")
	if _, err := (&RealClaudeInvoker{}).Review(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, call := range recordedCalls(t, rec) {
		fields := strings.Fields(call)
		for i, f := range fields {
			if f == "--plugin-dir" && fields[i+1] != op { // an operator directory is used in place, never removed
				if _, err := os.Stat(fields[i+1]); !os.IsNotExist(err) {
					t.Errorf("plugin dir %s leaked after the review: %v", fields[i+1], err)
				}
			}
		}
	}
}
