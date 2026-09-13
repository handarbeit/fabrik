package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// This file implements `fabrik init --github-app` (#1715): the guided setup
// flow that registers (or adopts) a GitHub App, resolves its installation on
// the target org, verifies the installation's actually-granted permissions
// against the same required set the engine's own startup check enforces
// (engine.RequiredGitHubAppPermissions — R3), and refuses a user-owned
// target (engine.RefuseUserOwnedBoardForAppAuth — R4) before handing the
// resolved client back to the caller (cmd/init.go), which persists the
// three github_app_* config fields and, when --create-board is also given,
// hands the same client to createBoardCore. See
// adrs/1715-github-app-setup-flow.md.

// defaultGitHubAppPrivateKeyPath is where a freshly manifest-created App's
// private key is written when --github-app-private-key-path isn't given
// (the create path — --github-app-id/--github-app-private-key-path together
// select the adopt path instead, using whatever path the operator names).
// No such default existed anywhere in engine or internal/githubauth before
// this issue — every prior caller required the operator to supply a path.
const defaultGitHubAppPrivateKeyPath = ".fabrik/github-app-key.pem"

// githubAppSetupOptions configures runGitHubAppSetup.
type githubAppSetupOptions struct {
	// Owner is the org to register/adopt the App on, discover its
	// installation for, and verify permissions against. Required.
	Owner string
	// AppID and PrivateKeyPath are both non-zero/non-empty to adopt an
	// existing App (cmd/init.go's flag validation enforces all-or-nothing);
	// both zero/empty runs the manifest flow to create a new one.
	AppID          int64
	PrivateKeyPath string
	// InstallationID, when non-zero, pins the installation and skips
	// discovery entirely.
	InstallationID int64
	// Webhooks mirrors the engine's own --webhooks flag: whether the
	// manifest/verification should include webhook-management permission.
	Webhooks bool
	// NoBrowser is forwarded to the manifest flow (R7).
	NoBrowser bool
	// BaseURL selects GitHub's API host. "" = production; tests point it at
	// an httptest server.
	BaseURL string
}

// githubAppSetupResult is runGitHubAppSetup's success return: everything
// cmd/init.go needs to persist config and (optionally) hand off to
// --create-board.
type githubAppSetupResult struct {
	AppID          int64
	PrivateKeyPath string
	InstallationID int64
	// Client is scoped to Owner via the resolved installation — ready to
	// use directly for board creation (createBoardCore), needing no
	// separate --token.
	Client     *gh.Client
	Reconciler *githubauth.Reconciler
}

// runGitHubAppSetup drives R1/R2/R3/R4 end to end. It composes exactly two
// possible githubauth.Reconcile calls, never a hand-rolled bootstrap/
// discovery of its own:
//
//  1. A first Reconcile call (pinned only if opts.InstallationID was given
//     explicitly) either bootstraps a fresh App via the manifest flow,
//     adopts an existing one (opts.AppID set), or reuses a prior run's
//     already-persisted state — Reconcile's own loadOrBootstrapCredentials
//     already makes this retry-safe (a downstream failure below never
//     re-runs the manifest flow and orphans a second App on retry).
//  2. When opts.InstallationID was NOT given, that first call runs in
//     non-pinned discovery mode; its LastDerived().Installations is
//     searched for an entry matching opts.Owner, and a second Reconcile
//     call pins the discovered installation ID — VerifyGrants only works on
//     a pinned Reconciler. When opts.InstallationID WAS given up front, the
//     first call is already pinned and this second call never happens —
//     the shape then matches the engine's own setUpGitHubAppAuth exactly.
func runGitHubAppSetup(ctx context.Context, opts githubAppSetupOptions) (*githubAppSetupResult, error) {
	privateKeyPath := opts.PrivateKeyPath
	if privateKeyPath == "" {
		privateKeyPath = defaultGitHubAppPrivateKeyPath
	}

	logf := func(format string, args ...any) { fmt.Printf("[github-app] "+format+"\n", args...) }

	baseOpts := githubauth.Options{
		AppID:             opts.AppID,
		AppInstallationID: opts.InstallationID,
		AppPrivateKeyPath: privateKeyPath,
		AppStatePath:      engine.GitHubAppStatePath("."),
		WatchedRepos:      []string{opts.Owner + "/*"},
		NoBrowser:         opts.NoBrowser,
		BaseURL:           opts.BaseURL,
		AppName:           engine.GitHubAppName,
		AppHomepageURL:    engine.GitHubAppHomepageURL,
		Logf:              logf,
	}

	reconciler, err := githubauth.Reconcile(ctx, baseOpts)
	if err != nil {
		return nil, fmt.Errorf("setting up GitHub App: %w", err)
	}

	installationID := opts.InstallationID
	if installationID == 0 {
		derived := reconciler.LastDerived()
		var found *githubauth.DerivedInstallation
		for i := range derived.Installations {
			if strings.EqualFold(derived.Installations[i].Account, opts.Owner) {
				found = &derived.Installations[i]
				break
			}
		}
		if found == nil {
			slug := strings.TrimSuffix(reconciler.BotLogin(), "[bot]")
			installURL := fmt.Sprintf("https://github.com/apps/%s/installations/new", slug)
			pagWarning := ""
			if derived.Truncated {
				pagWarning = " — the App may have more installations than could be listed (pagination ceiling hit); re-run to confirm before assuming it needs installing"
			}
			return nil, fmt.Errorf("no GitHub App installation found for owner %q%s — install the App at %s, "+
				"then re-run `fabrik init --github-app` (safely re-runnable: it reuses the App just created/adopted "+
				"rather than creating a duplicate)", opts.Owner, pagWarning, installURL)
		}
		installationID = found.InstallationID

		pinnedOpts := baseOpts
		pinnedOpts.AppInstallationID = installationID
		reconciler, err = githubauth.Reconcile(ctx, pinnedOpts)
		if err != nil {
			return nil, fmt.Errorf("pinning discovered installation %d for owner %q: %w", installationID, opts.Owner, err)
		}
	}

	client, err := reconciler.ClientForRepo(ctx, opts.Owner, "")
	if err != nil {
		return nil, fmt.Errorf("obtaining GitHub App installation client for owner %q: %w", opts.Owner, err)
	}

	// R4 before R3 (matching the engine's own setUpGitHubAppAuth ordering):
	// a user-owned installation would also fail the permission check below,
	// but with a confusing "missing organization_projects" message instead
	// of naming the actual, more fundamental cause.
	if err := engine.RefuseUserOwnedBoardForAppAuth(client, opts.Owner); err != nil {
		return nil, err
	}

	required := engine.RequiredGitHubAppPermissions(opts.Webhooks)
	shortfalls, err := reconciler.VerifyGrants(required)
	if err != nil {
		return nil, fmt.Errorf("verifying GitHub App installation's granted permissions: %w", err)
	}
	if len(shortfalls) > 0 {
		// R5: a permission change never reaches an already-installed
		// installation automatically — name the per-installation approval
		// page rather than appearing to succeed.
		approvalURL := fmt.Sprintf("https://github.com/settings/installations/%d", installationID)
		return nil, fmt.Errorf("GitHub App installation %d is missing required permissions: %s — approve the "+
			"permission change at %s (an org admin may be required), then re-run `fabrik init --github-app`",
			installationID, engine.FormatPermissionShortfalls(shortfalls), approvalURL)
	}

	fmt.Printf("  github-app: authenticated as %s (installation %d, organization %q)\n", reconciler.BotLogin(), installationID, opts.Owner)

	return &githubAppSetupResult{
		AppID:          reconciler.AppID(),
		PrivateKeyPath: privateKeyPath,
		InstallationID: installationID,
		Client:         client,
		Reconciler:     reconciler,
	}, nil
}
