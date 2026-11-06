package engine

import (
	"testing"

	"github.com/handarbeit/fabrik/internal/appwebhook"
)

// The shared receiver holds copies of two engine constants (it cannot import
// the engine). Nothing else ties them together, so pin them: a drift would make
// the replayer look back too far or too short, or the receiver cap differ from
// the gh-forward listener's.
func TestAppWebhookSharedConstantsMatchEngine(t *testing.T) {
	if appwebhook.LedgerWindow != deliveryLedgerWindow {
		t.Errorf("appwebhook.LedgerWindow = %v, engine deliveryLedgerWindow = %v", appwebhook.LedgerWindow, deliveryLedgerWindow)
	}
	if appwebhook.MaxBodyBytes != webhookMaxBodyBytes {
		t.Errorf("appwebhook.MaxBodyBytes = %d, engine webhookMaxBodyBytes = %d", appwebhook.MaxBodyBytes, webhookMaxBodyBytes)
	}
}
