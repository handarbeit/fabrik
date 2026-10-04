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
