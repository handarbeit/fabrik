package boardcache

import (
	"encoding/json"
	"testing"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

func labeledPayloadWithSender(action, label, sender string) []byte {
	p := issuesPayload{Action: action}
	p.Repository.FullName = "owner/repo"
	p.Issue.Number = 1
	p.Label.Name = label
	p.Sender.Login = sender
	b, _ := json.Marshal(p)
	return b
}

func TestLabelWebhookCarriesSenderAndEchoFlag(t *testing.T) {
	c := seedCache(t)
	var got []itemstate.Change
	c.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) { got = append(got, ch) }))

	echoed := map[string]bool{"issues:labeled:owner/repo#1+fabrik:paused": true}
	c.SetMatchEchoReportedFn(func(ev, action, key string) bool { return echoed[ev+":"+action+":"+key] })

	c.ApplyDelta("issues", labeledPayloadWithSender("labeled", "fabrik:paused", "bot[bot]"))
	c.ApplyDelta("issues", labeledPayloadWithSender("labeled", "human-label", "alice"))

	var paused, human *itemstate.Change
	for i := range got {
		for _, d := range got[i].LabelDeltas {
			switch d.Label {
			case "fabrik:paused":
				paused = &got[i]
			case "human-label":
				human = &got[i]
			}
		}
	}
	if paused == nil || !paused.EchoOfEngine || paused.Sender != "bot[bot]" {
		t.Fatalf("echo-matched label: %+v", paused)
	}
	if human == nil || human.EchoOfEngine || human.Sender != "alice" || human.Origin != itemstate.OriginWebhook {
		t.Fatalf("human label: %+v", human)
	}
}

func TestLabelWebhookEchoCarriesRequesterOnlyWhenEchoed(t *testing.T) {
	c := seedCache(t)
	var got []itemstate.Change
	c.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) { got = append(got, ch) }))

	// The via-fn reports a requester for every key, but only the matched one may be believed.
	c.SetMatchEchoViaFn(func(ev, action, key string) (bool, string) {
		return key == "owner/repo#1+overseer-label", "x"
	})

	c.ApplyDelta("issues", labeledPayloadWithSender("labeled", "overseer-label", "bot[bot]"))
	c.ApplyDelta("issues", labeledPayloadWithSender("labeled", "other-label", "alice"))

	if len(got) != 2 {
		t.Fatalf("saw %d changes, want 2: %+v", len(got), got)
	}
	if !got[0].EchoOfEngine || got[0].Requester != "x" {
		t.Fatalf("echoed label: %+v", got[0])
	}
	if got[1].EchoOfEngine || got[1].Requester != "" {
		t.Fatalf("unechoed label must carry no requester: %+v", got[1])
	}
}

func TestApplyLabelByCarriesRequester(t *testing.T) {
	c := seedCache(t)
	var got []itemstate.Change
	c.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) { got = append(got, ch) }))

	c.ApplyLabelAddedBy("owner/repo#1", "lbl", "x")
	c.ApplyLabelRemovedBy("owner/repo#1", "lbl", "x")
	c.ApplyLabelAdded("owner/repo#1", "lbl2")

	if len(got) != 3 {
		t.Fatalf("saw %d changes, want 3", len(got))
	}
	for i, want := range []string{"x", "x", ""} {
		if got[i].Origin != itemstate.OriginEngine || got[i].Requester != want {
			t.Errorf("change %d: %+v, want requester %q", i, got[i], want)
		}
	}
}
