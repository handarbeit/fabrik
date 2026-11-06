package engine

import (
	"context"
	"fmt"
	"os"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/appwebhook"
)

// The R7 check itself lives in internal/appwebhook (#2475, ADR 2475; design
// #2143). This file is the engine's side: the config read and the fault flag.

// fetchAppHookConfigFn is the test seam for the App webhook-config read: the
// real call needs an App JWT and the live GitHub API.
var fetchAppHookConfigFn = func(ctx context.Context, e *Engine) (gh.AppHookConfig, error) {
	keyPEM, err := os.ReadFile(e.cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		return gh.AppHookConfig{}, fmt.Errorf("reading the App private key: %w", err)
	}
	key, err := gh.ParseAppPrivateKey(keyPEM)
	if err != nil {
		return gh.AppHookConfig{}, err
	}
	jwt, err := gh.BuildAppJWT(e.cfg.GitHubAppID, key)
	if err != nil {
		return gh.AppHookConfig{}, err
	}
	return gh.FetchAppHookConfigContext(ctx, "", jwt)
}

// checkAppWebhookConfig is the R7 check (#2143): the App's configured webhook
// URL must be this node's Funnel URL and its content type json. wantURL ""
// (the node name is not known) skips the check.
func (e *Engine) checkAppWebhookConfig(tag, wantURL string) {
	if wantURL == "" || e.faults.active(FaultNoHookConfigCheck) {
		return
	}
	e.hookCheck.Check(appwebhook.HookCheckInput{
		Tag:     tag,
		WantURL: wantURL,
		Fetch:   func(ctx context.Context) (gh.AppHookConfig, error) { return fetchAppHookConfigFn(ctx, e) },
		Panel:   enginePanel{e: e, tag: tag},
		Logf:    e.logf,
	})
}
