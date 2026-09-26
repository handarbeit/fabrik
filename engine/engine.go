package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/selfupgrade"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tui"
)

type Config struct {
	Owner                     string
	Repo                      string
	ProjectNum                int
	OwnerType                 string
	User                      string
	Token                     string
	Version                   string
	Yolo                      bool
	AutoUpgrade               bool
	GitSSH                    bool
	PollSeconds               int
	RetryBackoff              time.Duration // Delay before re-dispatching a stage after an incomplete attempt; independent of PollSeconds (#1831). Zero = unset → falls back to githubRecheckInterval (see retry_backoff.go). The CLI always sets it.
	MaxConcurrent             int
	MaxRetries                int
	MaxSliceRetries           int                 // Max turn-cap preemption ("slice") cycles per stage before pausing (default 10; #1199) — bounds a non-converging job independently of MaxRetries, which counts only genuine failures
	MaxResumeFailures         int                 // Max consecutive failed --resume attempts for one (issue, stage) session before discarding the session pointer and cold-starting (default 2; #1414) — independent of MaxRetries, mirroring the fabrik:claude-limit StageAttempted-without-StageRetryIncremented exemption
	MaxToolsDeniedRetries     int                 // Max consecutive tool-permission-denial exits per stage before pausing (default 3; #1523) — bounds a non-converging permission misconfiguration independently of MaxRetries, mirroring the fabrik:claude-limit StageAttempted-without-StageRetryIncremented exemption
	ReviewWaitTimeout         time.Duration       // How long to wait for PR reviewers before auto-advancing anyway (default 15m)
	ReconcileInterval         time.Duration       // Reconcile ticker cadence (0 = use lightReconcileInterval default of 3m)
	MaxReviewCycles           int                 // Max review re-invocation cycles per issue before pausing (default 5)
	CIWaitTimeout             time.Duration       // CI-gate liveness-stall dwell: how long CI may show no observable progress before pausing (default 30m; ADR-1410 — no longer a total-wait bound, see CIBackstopTimeout)
	CIBackstopTimeout         time.Duration       // Absolute cap on how long an item may sit in fabrik:awaiting-ci under any classification, bounding per-poll cost independent of CI duration (default 4h; ADR-1410, R5)
	RequiredStatusContexts    map[string][]string // Per "owner/repo" required status/check-run context names the ci-gate must confirm success on before clearing (ADR-933); unconfigured repos = no behavior change
	PostPushDwell             time.Duration       // How long to wait after a PR push before clearing CI gate as 'no CI configured' (default 90s)
	MaxCiFixCycles            int                 // Max CI-fix re-invocation cycles per issue before pausing (default 5)
	MaxRebaseCycles           int                 // Max rebase re-invocation cycles per issue before pausing (default 3)
	MaxEnqueueCycles          int                 // Max merge-queue re-enqueue cycles per issue before pausing (default 5; ADR-058 D4)
	ConvergenceBudget         time.Duration       // Wall-clock budget for post-Validate yolo convergence (default 30m; 0 = disabled, waits indefinitely)
	AutoMergeStrategy         string              // Merge method for enablePullRequestAutoMerge: MERGE, SQUASH, or REBASE (default MERGE)
	MergeQueue                string              // Merge queue routing for yolo path: "auto" (enqueue when repo uses merge queue) or "off" (skip enqueue)
	MergeTrain                string              // Fabrik-internal merge train: "on" (advance yolo Validate completions to Queued) or "off" (default; existing auto-merge path unchanged)
	MaxMergeTrainEjections    int                 // Max merge-train ejections before pausing a member (default 3; ADR-059)
	MaxBatchSize              int                 // Max Queued items snapshotted into one merge-train batch (0 = derive default 5; ADR-059 D4/D-f)
	MaxBisectValidations      int                 // Max combined validations per red batch before the one-at-a-time fallback (0 = derive 2·⌈log₂(MaxBatchSize)⌉+1; ADR-059 D4/D-f)
	MaxTrainRebaseCycles      int                 // Max main-moved rebase+revalidate cycles per merge-train batch before dissolving (0 = default 3; ADR-059 D5)
	MaxTrainTrialsPerWindow   int                 // Runaway guard: max trial-branch creations with zero successful lands before pausing all Queued members (0 = default 20; ADR-059 D8)
	TrainTrialWindowDuration  time.Duration       // Runaway guard: rolling window over which MaxTrainTrialsPerWindow is measured (0 = default 60m; ADR-059 D8)
	MaxCommentCyclesPerWindow int                 // Comment-processing circuit breaker: max non-advancing comment-processing invocations per issue before pausing (0 = default 10; #1089)
	CommentCycleWindow        time.Duration       // Comment-processing circuit breaker: rolling window over which MaxCommentCyclesPerWindow is measured (0 = default 30m; #1089)
	MaxNoOpCommentCycles      int                 // Success-agnostic comment-processing circuit breaker: max consecutive no-progress comment-processing invocations per issue+stage before pausing, regardless of whether each invocation itself exited successfully. Deliberately higher than MaxReviewCycles's default — see effectiveMaxNoOpCommentCycles (0 = default 10; #1555, sibling of #1089/#1382)
	KillGraceSigInt           time.Duration       // Grace window after SIGINT before SIGTERM (default 10s; 0 = skip SIGINT step)
	KillGraceSigTerm          time.Duration       // Grace window after SIGTERM before SIGKILL (default 10s)
	DrainDeadline             time.Duration       // Bound on a clean stop's worker drain, covering both the kill escalation and the shutdown pause-write phase (default 30s; ADR-1393). <= 0 falls back to the default in drainDeadline() — unlike kill_grace, a clean stop has no "0 = wait forever" mode.
	ClaudeWaitDelay           time.Duration       // How long to wait after Claude exits before giving up on pipe drain and recovering output (default 30s)
	WorkerStaleTimeout        time.Duration       // How long a worker heartbeat can be stale before PID-liveness is checked (default 5m; must be > HeartbeatInterval×2)
	DebugOutput               bool
	SymlinkEnv                bool
	WorktreeBoundaryAudit     bool
	PluginDir                 string
	Stages                    []*stages.Stage
	Webhooks                  bool
	WebhookPort               int
	WebhookEvents             []string
	// EventSource selects the ingestion transport: EventSourcePoll (default,
	// "" or "poll") or EventSourceHookdeck ("hookdeck"). Deliberately a
	// separate axis from Webhooks above — event_source: hookdeck is an
	// App-auth-only opt-in that can never be combined with Webhooks (refused
	// at startup by RefuseHookdeckWithWebhooks) or used without App auth
	// (refused by RefuseHookdeckWithoutGitHubApp), and #1752's
	// RefuseWebhooksWithGitHubApp never has to know this field exists. See
	// engine/github_app_auth.go and adrs/1142-hookdeck-ingestion-for-app-auth.md.
	EventSource string
	// HookdeckAPIKeyEnv/HookdeckWebhookSecretEnv each name an environment
	// variable holding the actual secret — indirection, not the secret
	// itself — mirroring pruefer/config.go's hookdeck.api_key_env /
	// hookdeck.webhook_secret_env convention. Only consulted when
	// EventSource == EventSourceHookdeck; empty means use
	// DefaultHookdeckAPIKeyEnv / DefaultHookdeckWebhookSecretEnv.
	HookdeckAPIKeyEnv        string
	HookdeckWebhookSecretEnv string
	ProjectStatusPollSeconds int           // Layer 2 status-only sweep cadence in seconds; default 15 s (gate runs every poll cycle; field retained for config compatibility)
	JanitorIntervalHours     int           // Periodic worktree janitor cadence in hours; 0 disables the janitor (default 1)
	LogRetentionDays         int           // Log files older than this many days are pruned; 0 disables age-based pruning (default 14)
	LogMaxBytes              int64         // Total size cap for .fabrik/logs/; oldest files deleted first after age prune; 0 disables (default 2 GiB)
	SessionRetentionDays     int           // .session files older than this many days are pruned; 0 disables age-based pruning (default 14)
	ArchiveAfter             time.Duration // Grace period since stage:<Done>:complete was applied before a Done item is archived (default 168h = 1 week; ADR-068)
	ArchiveDone              string        // "on" (default) or "off" to fully disable Done-item auto-archival (also FABRIK_ARCHIVE_DONE; ADR-068)
	GHESHost                 string        // GitHub Enterprise Server hostname (e.g. "github.example.com"); "" (default) = github.com, byte-identical to pre-GHES behavior (also FABRIK_GHES_HOST; ADR-1391)
	// GitHubAppID, GitHubAppPrivateKeyPath, and GitHubAppInstallationID
	// together configure a GitHub App installation as a second, co-equal
	// authentication path alongside Token (#1713) — compat-mode only,
	// consuming an App ID + private key + installation ID that already
	// exist (an operator has already created and installed the App by
	// whatever means). All three must be set together or none at all
	// (enforced at startup — see validateGitHubAppConfig); the zero value
	// (all unset, the default) means PAT mode, byte-identical to pre-#1713
	// behavior. Also FABRIK_GITHUB_APP_ID / FABRIK_GITHUB_APP_PRIVATE_KEY_PATH
	// / FABRIK_GITHUB_APP_INSTALLATION_ID and their config.yaml equivalents.
	// See engine/github_app_auth.go and adrs/1713-engine-github-app-auth.md.
	GitHubAppID             int64
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID int64
	// NoBrowser suppresses githubauth's guided-install browser-open
	// (Options.NoBrowser) when App auth's non-pinned discovery path is
	// reached. Defaults to true (suppressed) for the engine — unlike
	// Pruefer's own NoBrowser, whose default is false because its
	// first-run setup flow makes an automatic browser-open the point. A
	// long-running daemon is frequently headless, containerized, or on a
	// remote box, so opening a browser must be opt-in here (#1763, R2).
	// Also --no-browser / FABRIK_NO_BROWSER / config.yaml's no_browser
	// (AC2, re-enables it). See engine/github_app_auth.go.
	NoBrowser bool
	// ReadyCh is closed once Run() has registered signal handlers. Tests use
	// this to avoid sending SIGINT before signal.Notify is installed.
	ReadyCh chan struct{}
}

// EventSource values and defaults for the Hookdeck (App-auth-only) ingestion
// transport (#1142). Mirrors pruefer/config.go's own EventSourcePoll/
// EventSourceHookdeck/Default* constants by name and shape — the same
// indirection (a config field naming an env var, not holding the secret
// itself) — so an operator already running Pruefer with Hookdeck recognizes
// this immediately. DefaultHookdeckWebhookSecretEnv is FABRIK_-prefixed,
// since it names Fabrik's own GitHub App webhook secret, not a value
// Pruefer's process also reads; DefaultHookdeckAPIKeyEnv reuses Hookdeck's
// own conventional env var name unprefixed, matching Pruefer's own
// default — see adrs/1142-hookdeck-ingestion-for-app-auth.md.
const (
	EventSourcePoll     = "poll"
	EventSourceHookdeck = "hookdeck"

	DefaultEventSource              = EventSourcePoll
	DefaultHookdeckAPIKeyEnv        = "HOOKDECK_API_KEY"
	DefaultHookdeckWebhookSecretEnv = "FABRIK_GITHUB_WEBHOOK_SECRET"
)

// cloneCall coordinates concurrent bare-clone attempts for the same repo.
// The first caller to store one in cloneInFlight performs the clone; subsequent
// callers wait on done and share the result.
type cloneCall struct {
	done chan struct{} // closed when clone completes (success or failure)
	dir  string        // bareDir on success; empty on failure
	err  error         // clone error on failure; nil on success
	// ownerKey identifies the item (issueKey format, "owner/repo#N") that owned
	// a *failed* clone attempt. Set only on failure, before done is closed, so
	// it's safely visible to waiters via the channel-close happens-before edge.
	// It is the identity half of the retry-boundary gate (ADR-1543): a failed
	// entry is only ever cleared by a caller whose own issueKey matches
	// ownerKey and whose own (already-fetched) Labels no longer carry
	// fabrik:paused — i.e. the specific item that owned the failure, redispatched
	// after an operator has cleared the pause. See ensureRepoReady and
	// ensureSpawnTargetReady.
	ownerKey string
}

type Engine struct {
	cfg                   Config
	client                GitHubClient
	releaseClient         GitHubClient           // always github.com, regardless of cfg.GHESHost — Fabrik's own self-upgrade release lives on github.com/handarbeit/fabrik, never on a customer's GHES instance (see checkReleaseUpgrade). Equal to client whenever no GHES host is configured (including all NewWithDeps-constructed test engines), so this is a no-op on the default path.
	hostClient            *gh.Client             // same host as client, concretely typed; used by the GHES-only startup version-floor preflight (checkGHESVersionFloor), which needs FetchInstalledVersion and isn't worth adding to the GitHubClient interface for one startup-only call, and by checkHookdeckInstallationCoverage (#1142), which needs a live installation token via Token() for the R5 App-mode coverage check. nil outside New() (e.g. NewWithDeps-constructed test engines); checkGHESVersionFloor is a standalone function tested directly against a *gh.Client, not through the Engine.
	ghAppAuth             *githubauth.Reconciler // non-nil only when Config.GitHubApp* fields configure App-auth (#1713); nil in PAT mode (the default). Run() starts and, on shutdown, joins its refresh-loop goroutines when non-nil — see poll.go's Run().
	readClient            boardcache.ReadClient  // read-only GitHub calls; may be CacheImpl or GitHubAdapter
	claude                ClaudeInvoker
	statusField           *gh.StatusField
	worktreeManagers      map[string]*WorktreeManager // key: "owner/repo"; one WM per discovered repo
	fabrikDir             string                      // directory containing .fabrik/ (always os.Getwd() at startup)
	mu                    sync.Mutex
	store                 *itemstate.Store         // per-item engine state (locks, invocation outcomes, deep-fetch, CI-gate); see ADR-036
	totalTokens           TokenUsage               // accumulated token usage since process start
	lastReportedCost      float64                  // cost at last [stats] report; skip repeat prints when unchanged
	mayNeedWork           map[string]bool          // key: issueKey; items that have changed since the last poll cycle
	mayNeedWorkMu         sync.Mutex               // guards mayNeedWork
	seededRepos           map[string]bool          // key: "owner/repo"; in-memory guard to avoid re-seeding on every poll
	checkedAutoMergeRepos map[string]bool          // key: "owner/repo"; guard to emit allow_auto_merge warning at most once per run
	repoAccess            map[string]gh.RepoAccess // key: "owner/repo"; resolveRepoAccess's cache — single source of truth for seeding, the allow_auto_merge check, and itemMayNeedWork's dispatch gate (ADR-1347)
	// appAccessibleRepos, appAccessibleReposTrunc, and appAccessibleReposReady
	// are the App-auth counterpart of the PAT-mode permissions.push signal
	// resolveRepoAccess otherwise reads (#1750): under App auth, permissions.push
	// is always false for an installation token (it describes a user's access,
	// not an installation's), so resolveAppRepoAccess consults these instead.
	// Populated exactly once, eagerly, in New() — before any worker goroutine
	// exists — by a single GET /installation/repositories call
	// (Reconciler.AccessibleRepos), so they need no mutex: write-once during
	// single-threaded startup, read-only for the rest of the process. nil/false
	// zero values (PAT mode, or an App-mode fetch that failed at startup) make
	// resolveAppRepoAccess treat every repo as ambiguous, which resolveRepoAccess's
	// existing fail-open branch then admits (R3) — never a silent fail-closed.
	appAccessibleRepos       map[string]bool // key: lower-cased "owner/repo"; the installation's own accessible-repo list
	appAccessibleReposTrunc  bool            // true if the accessible-repo list hit FetchInstallationRepositories' pagination ceiling — a repo missing from a truncated list is ambiguous, not confirmed-excluded
	appAccessibleReposReady  bool            // true once the single startup fetch above has succeeded; false (the zero value) means "unknown" — never treated as "no repos"
	idleCount                int             // consecutive idle polls; triggers self-upgrade at threshold
	idleStart                time.Time       // when consecutive idle polls began; zero value = not idle
	pollsUntilStalenessCheck int             // countdown to next checkSourceStaleness; 0 fires on next poll (#1464)
	// backoffPrevMultiplier, backoffRateLimitLow, backoffRateLimitRatio,
	// backoffLastRemaining, and backoffRestPaused are PollWithBackoff's
	// persistent state (engine/poll.go) — promoted from doPollCycle closure
	// locals to Engine fields (ADR-1592) so a test driving successive
	// PollWithBackoff calls sees correctly-persisted backoff state, mirroring
	// idleStart's own precedent. Unlike idleStart's zero-value ("not idle"),
	// backoffPrevMultiplier and backoffRateLimitRatio have non-zero starting
	// values that both New() and NewWithDeps() set explicitly.
	backoffPrevMultiplier int
	backoffRateLimitLow   bool
	backoffRateLimitRatio float64
	backoffLastRemaining  int
	backoffRestPaused     bool
	// lastPollAttemptAt records when PollWithBackoff last actually attempted a
	// poll (i.e. reached the REST gate check) — R3's minimum-interval floor
	// (see backoff.go's minPollInterval), guarding against any wake-path bypass
	// (present or future) driving unbounded immediate polls. Zero value means
	// "no attempt yet", so the very first call is never floor-blocked.
	// Unguarded like its backoff* siblings above — single-goroutine-only in
	// production (only Run()'s own goroutine calls PollWithBackoff there).
	lastPollAttemptAt time.Time
	// logThrottle is the shared dedup state behind logfThrottled (R4,
	// logthrottle.go) — collapses repeated identical log lines (e.g. the
	// per-poll rate-limit stats lines) into one emission per throttle window
	// instead of one per poll cycle. Zero-value ready.
	logThrottle logThrottleState
	// stalenessCompareFn overrides selfupgrade.CompareDevBuild when non-nil.
	// Used by tests to inject a synthetic DevBuildStatus without real git
	// subprocesses. Production leaves this nil.
	stalenessCompareFn          func(selfupgrade.DevBuildConfig) (selfupgrade.DevBuildStatus, error)
	lastProjectUpdatedAt        time.Time                     // last seen project.updatedAt from FetchProjectUpdatedAt gate; zero = not yet checked
	wakeCh                      chan struct{}                 // TUI sends on this to wake the poll loop immediately; nil if no TUI
	stopCh                      chan tui.StopRequest          // TUI sends on this to stop a specific in-flight issue; nil if no TUI
	sem                         chan struct{}                 // semaphore bounding concurrent workers across poll cycles
	wg                          sync.WaitGroup                // tracks in-flight workers for graceful shutdown; also tracks the shutdown pause-write phase (runShutdownPause, shutdown.go) so one waitGroupTimeout call bounds both (ADR-1393)
	cloneInFlight               sync.Map                      // key: "owner/repo" string, value: *cloneCall; per-repo bare-clone coordination
	mergeTrainInFlight          sync.Map                      // key: trainKey ("owner/repo:baseBranch", mergeTrainKey — since #1648, was bare "owner/repo"), value: *mergeTrainWorkerState; per-(repo,base) train dispatch guard, so one base's train cannot block or be mistaken for another base's train in the same repo
	mergeTrainEjectionsMu       sync.Mutex                    // guards mergeTrainEjectionCounts
	mergeTrainEjectionCounts    map[string]int                // key: "owner/repo#N", ejection count per member — deliberately stays issue-scoped, not re-keyed by base (#1648): an issue belongs to exactly one partition at a time
	mergeTrainCIDeferredMu      sync.Mutex                    // guards mergeTrainCIDeferred
	mergeTrainCIDeferred        map[string]string             // key: "owner/repo#N", value: head SHA last deferred at by the #1821 admission gate — suppresses a repeat comment when the same SHA is re-deferred (R9 ping-pong backstop); in-memory only, cleared when the member is next admitted non-red
	mergeTrainCloneSkipMu       sync.Mutex                    // guards mergeTrainCloneSkipCounts
	mergeTrainCloneSkipCounts   map[string]int                // key: "owner/repo"; consecutive ensureRepoReady ErrSkipItem streak for prepareTrainWorker's batch[0] anchor call — batch[0] can differ across polls, so this is repo-keyed rather than item-keyed like mergeTrainEjectionCounts (#1543 follow-up: identity-gated retry boundary can wedge behind a since-rotated anchor). Deliberately NOT re-keyed by base (#1648): a bare-clone failure is a property of the repo's git remote, shared by every base partition — re-keying would fragment one genuine repo-level failure signal into N spurious per-base ones.
	mergeTrainTrialsMu          sync.Mutex                    // guards mergeTrainTrials
	mergeTrainTrials            map[string][]time.Time        // key: trainKey ("owner/repo:baseBranch", mergeTrainKey — since #1648, was bare "owner/repo"), trial timestamps for runaway guard (ADR-059 D8); one base's trials never count toward a sibling base's threshold in the same repo
	mergeTrainRunawayMu         sync.Mutex                    // guards mergeTrainRunawayAlerted AND serializes fireRunawayGuard's pause+alert critical section across all three call sites (Hook 1 x2, Hook 2) — see fireRunawayGuard (#1533). Still a single engine-wide mutex, not sharded per (repo,base) — #1648 widens its blast radius to also serialize concurrent per-base workers within the same repo, not just across repos, but the trade-off (rare/exceptional event) is unchanged.
	mergeTrainRunawayAlerted    map[string]int                // key: "trainKey#N" (trainKey a mergeTrainKey "owner/repo:baseBranch" since #1648, was bare "owner/repo#N"); value: the trial count in effect when this member was last alerted. A later call is treated as already-alerted only while its own count is <= the recorded value — trials cannot increase while the guard keeps the queue paused, so an increase can only mean an operator manually resumed the member (removing fabrik:paused) and it genuinely tripped again, which must produce a fresh alert (#1533 review, finding 2). Also cleared wholesale per-trainKey by resetTrialCounter (the guard's own "episode ends" signal — a successful land) (#1533)
	queuedReviewEjectsMu        sync.Mutex                    // guards queuedReviewEjects
	queuedReviewEjects          map[string]map[int]int        // key: "owner/repo" -> issue number -> unresolved finding count; pending-eject signal a settle scan leaves for an in-flight merge-train worker to consume at its own checkpoints (#1208). Deliberately NOT re-keyed by base (#1648): keyed by issue number within the repo bucket, and an issue belongs to exactly one partition's live batch at a time, so two workers sharing this repo-level map never collide.
	queuedCommentEjects         map[string]map[int]struct{}   // key: "owner/repo" -> issue number; pending-eject signal for an unprocessed human comment on a Queued member (#1863), the comment-cause sibling of queuedReviewEjects. Guarded by queuedReviewEjectsMu, keyed by bare repo for the same ADR-1648 reason.
	sentinelProbeFailuresMu     sync.Mutex                    // guards sentinelProbeFailures
	sentinelProbeFailures       map[string]int                // key: "owner/repo#N"; consecutive scan cycles in which probeSentinelLive itself failed (R4, #1779) for a PID<=0 worker whose sentinel could not be verified either way. Bounded by sentinelProbeUnverifiableCycleLimit before falling back to the plain timeout clear, logged as unverified. Scan-goroutine-local bookkeeping, mirroring mergeTrainRunawayAlerted's per-episode map shape rather than itemstate.Store state — nothing outside the scan needs to observe it. Cleared whenever the worker leaves the unverifiable state: found live (PID adopted), found dead (cleared), or itself cleared.
	issueCtxs                   sync.Map                      // key: issueKey string, value: issueCtxEntry; per-issue context for kill-reason propagation
	pauseIssueMuGuard           sync.Mutex                    // guards pauseIssueMu itself (creation, refcounting, deletion) — distinct from the per-issue *sync.Mutex each entry embeds
	pauseIssueMu                map[string]*pauseIssueMuEntry // key: issueKey string; refcounted per-issue mutex serializing concurrent pauseInterruptedIssue calls for the same issue (ADR-1393 — a TUI stop and a daemon shutdown pause can race for the same in-flight issue). Entries are deleted once no caller holds a reference, so this does not grow unboundedly over the daemon's lifetime (review finding on #1393).
	baseBranchWarnedSet         sync.Map                      // key: "owner/repo#N:branch"; prevents repeated fallback comments for bad base: labels
	ciGateCoverageWarnedSet     sync.Map                      // key: "owner/repo|stage"; dedups the R4 degenerate-CI-gate-coverage log warning (ADR-1441)
	mergeTrainBatchSnapshotSeen sync.Map                      // key: trainKey ("owner/repo:baseBranch", mergeTrainKey — since #1648, was bare "owner/repo"), value: string signature (sorted item numbers) of the last-logged Queued batch snapshot
	claudeSuspendMu             sync.Mutex                    // guards claudeSuspendedUntil
	claudeSuspendedUntil        time.Time                     // zero = not suspended; account-wide Claude dispatch suspension deadline (see usage_limit_backoff.go)
	events                      chan tui.Event                // nil in tests / plain-text mode; TUI goroutine consumes
	logFile                     *os.File                      // persistent log file at .fabrik/fabrik.log; nil if not opened
	logMu                       sync.Mutex                    // serializes concurrent writes to logFile
	webhookMgr                  eventIngestionManager         // nil when no event-ingestion transport is active (#1142)
	// heartbeatIntervalOverride overrides the package-level heartbeatInterval constant
	// when non-zero. Used by tests to reduce the heartbeat period to sub-millisecond.
	heartbeatIntervalOverride time.Duration
	// upgradeCheckFn is called instead of checkAndUpgrade() when non-nil.
	// Used by tests to observe the startup upgrade check without invoking the real upgrade path.
	upgradeCheckFn func()
	// trainValidateFn overrides the real assemble+combined-Validate path when non-nil.
	// Tests inject a membership-keyed function so the merge-train bisection / re-form /
	// fallback control flow (ADR-059 D4) can be exercised without real git or CI. The
	// diagnostic return mirrors pollTrainCI's (TrainCIResult, *trainCIDiagnostic) shape
	// (#1420 R1) so seam-based tests can exercise the ejection-comment diagnostic content,
	// not only ejection sequencing. Production leaves this nil. See assembleAndValidate.
	trainValidateFn func(ctx context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic)
	// trainRedBatchHook, when non-nil, is called as the first line of handleRedBatch — a
	// test-only call-observation seam (#1440 AC1/AC6) proving handleRedBatch (multi-member
	// bisection) is never reached for a red batch of exactly one member, which
	// runMergeTrainWorker's arity guard now intercepts before handleRedBatch is called.
	// A trial-count-only assertion doesn't distinguish this: bisect's own base case for a
	// single red member already makes zero extra validate calls, so without this hook a
	// test proving the guard exists would pass unmodified against pre-#1440 code too.
	// Nil in production (zero cost).
	trainRedBatchHook func()
	// cloneAttemptHook, when non-nil, is called once per genuine clone owner
	// attempt — right before ensureBareClone is invoked in both ensureRepoReady
	// and ensureSpawnTargetReady — a test-only call-observation seam (ADR-1543,
	// #1543 R5) mirroring trainRedBatchHook's pattern above. It gives tests a
	// direct count of real clone attempts independent of AddComment counting,
	// which isn't a valid duplicate-clone proxy for ensureSpawnTargetReady
	// (every distinct parent legitimately posts its own comment by design).
	// Nil in production (zero cost).
	cloneAttemptHook func(nameWithOwner string)
	// generatedFilesOverride overrides the package-level generatedFiles mapping when
	// non-nil. Tests inject a synthetic path + fake regen command, since the throwaway
	// repos built by setupTrainRepo have none of docs/llms-full.txt's real source files
	// for scripts/generate-llms-full.sh to read. Production leaves this nil and uses
	// generatedFiles. See (e *Engine) generatedFileSet.
	generatedFilesOverride []generatedFileSpec
	// sighupRequested is set by the SIGHUP handler goroutine to signal that the
	// main loop should re-exec after draining workers (Unix only).
	sighupRequested atomic.Bool
	// sighupExecFn overrides syscall.Exec in tests to prevent the test process
	// from actually being replaced. Production code leaves this nil.
	sighupExecFn func(argv0 string, argv []string, envv []string) error
	// cleanupHook is called before any syscall.Exec or os.Exit(1) on force-quit
	// paths to release the terminal (e.g. p.ReleaseTerminal() in TUI mode).
	// Nil in non-TUI mode. Set via SetCleanupHook; wrapped in sync.Once so
	// concurrent force-quit goroutines can't call it simultaneously.
	cleanupHook func()
	// clock is the source e.now() reads from. Nil in production (New never
	// sets it) — now() falls back to time.Now(), byte-identical to the
	// pre-seam behavior. Tests substitute it via SetClock to control
	// itemstate.CooldownAt timing deterministically (ADR-1449).
	clock Clock
	// trainCIPollInterval overrides pollTrainCI/pollForMergeable's hardcoded
	// 30s retry interval when non-zero. Both functions poll real GitHub state
	// (check runs, mergeable fields) on a real-wall-clock ticker unrelated to
	// the Clock seam above — trial-branch CI genuinely takes real time to run
	// in production, so there is nothing to advance a virtual clock past.
	// tests/sim's merge-train scenarios (#1452) still need this real sleep to
	// be short: a single red-batch bisection matrix drives a dozen-plus
	// trials, and at 30s/retry that either blows past the sim harness's
	// worker-quiescence bound or makes the suite take minutes. Zero (the
	// production default — New never sets this) preserves the exact 30s
	// literal. See SetTrainCIPollIntervalForTest and adrs/1452-mergetrain-sim-
	// harness-seams.md.
	trainCIPollInterval time.Duration
	// mergeTrainQueueSortDisabledForTest disables groupQueuedByRepoAndBase's
	// deterministic (StatusEnteredAt, Number) ordering (#1833) when true, falling
	// back to whatever order the board-state source produced — i.e. exactly the
	// pre-#1833 behavior. Exists solely so a sim scenario can reproduce the
	// historical churn (a different arbitrary Queued subset selected every poll
	// once membership exceeds max_batch_size) and then demonstrate it gone with
	// the sort left enabled (the production default; New never sets this). See
	// SetMergeTrainQueueSortDisabledForTest and ADR-1833.
	mergeTrainQueueSortDisabledForTest bool
	// mergeTrainPrefixReuseDisabledForTest disables assembleTrialBranch's trial-prefix
	// reuse (#1835) when true, falling back to always forking fresh off the pinned base
	// SHA and re-merging every member — i.e. exactly the pre-#1835 behavior. Exists
	// solely so a test can demonstrate the reuse is non-vacuous (Acceptance 6): re-run
	// an AC1/AC2-style scenario with this set and observe the previously-suppressed
	// merges and resolveConflictWithClaude invocations return. Production leaves this
	// false (New never sets it); see newTrainPrefixCache and ADR-1835.
	mergeTrainPrefixReuseDisabledForTest bool
	// trainPrefixLookupHookFn, when non-nil, is called at the end of every
	// assembleTrialBranch prefix lookup with the matched prefix length, the total
	// member count, and the member numbers being assembled — a test-only
	// call-observation seam (mirrors trainRedBatchHook's pattern) letting a test assert
	// exactly how much of a given trial's chain was reused, not just the end-to-end
	// outcome. Nil in production (zero cost). See ADR-1835.
	trainPrefixLookupHookFn func(matchedLen, totalLen int, memberNumbers []int)
	// trainMergeAttemptHookFn, when non-nil, is called immediately before
	// assembleTrialBranch actually runs `git merge` for a member — i.e. once per member
	// NOT covered by a reused prefix. This is a merge-count observation independent of
	// resolveConflictWithClaude's own invocation count, which git rerere's autoupdate
	// replay (ADR-1834) can satisfy without ever calling Claude — so "zero Claude
	// invocations" alone cannot distinguish "the merge was skipped entirely" (#1835's
	// prefix reuse) from "the merge ran but its conflict was resolved for free by
	// rerere" (already-existing, unrelated behavior). This hook fires for every member
	// actually merged regardless of outcome (clean or conflicted), letting a test assert
	// the literal "zero merges" half of Acceptance 1 directly. Nil in production (zero
	// cost). See ADR-1835.
	trainMergeAttemptHookFn func(memberNumber int)
}

func New(cfg Config) (*Engine, error) {
	// fabrikDir is the directory containing .fabrik/ (stages, plugin, config).
	// Always use the current working directory — Fabrik bare-clones each managed
	// repo to .fabrik/repos/<owner>-<repo>.git and uses worktrees from there.
	fabrikDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolving working directory: %w", err)
	}

	// Default to .fabrik/plugin in the fabrik dir (created by fabrik init).
	// --plugin-dir flag overrides this for development.
	// Path must be absolute since Claude runs in the worktree, not the repo root.
	pluginDir := cfg.PluginDir
	if pluginDir == "" {
		defaultPluginDir := filepath.Join(fabrikDir, ".fabrik", "plugin")
		if fi, err := os.Stat(defaultPluginDir); err == nil && fi.IsDir() {
			pluginDir = defaultPluginDir
		}
	}
	if pluginDir != "" {
		if abs, err := filepath.Abs(pluginDir); err == nil {
			pluginDir = abs
		}
		claudePluginDir = pluginDir
	}
	claudeWaitDelay = cfg.ClaudeWaitDelay
	// KillGraceSigInt >= 0: 0 = skip SIGINT step, positive = use that duration.
	// Execute() maps "" → 10s, so 0 only arrives when user explicitly passed 0s.
	// Negative is guarded defensively (killGraceSigInt() never returns negative).
	if cfg.KillGraceSigInt >= 0 {
		claudeKillGraceSigInt = cfg.KillGraceSigInt
	} else {
		claudeKillGraceSigInt = 10 * time.Second
	}
	if cfg.KillGraceSigTerm >= 0 {
		claudeKillGraceSigTerm = cfg.KillGraceSigTerm
	} else {
		claudeKillGraceSigTerm = 10 * time.Second
	}
	claudeGHToken = cfg.Token
	claudeGHHost = cfg.GHESHost
	claudeAnthropicAPIKey = os.Getenv("FABRIK_ANTHROPIC_API_KEY")
	claudeAnthropicEnvPassthrough = parseAnthropicEnvPassthrough(os.Getenv("FABRIK_ANTHROPIC_ENV_PASSTHROUGH"))

	// One-time, process-lifetime capability probe (see claudeNameFlagSupported):
	// older claude binaries reject unknown flags outright, which would kill
	// every worker on the fleet, so support must be confirmed once up front
	// rather than assumed or probed per invocation. New() runs once per
	// long-lived fabrik daemon process start (this is the poll-loop engine,
	// not a per-invocation interactive CLI path), so the probe's 5s worst-case
	// timeout is a bounded, one-time cost against the daemon's whole lifetime
	// — in practice `claude --help` returns in well under a second.
	claudeNameFlagSupported = probeClaudeNameFlagSupport()
	if !claudeNameFlagSupported {
		fmt.Printf("[startup] worker session naming (--name) unavailable: installed claude binary does not advertise --name; upgrade claude to enable session names in ps/session-picker output\n")
	}

	worktreeRoot := filepath.Join(fabrikDir, ".fabrik", "worktrees")
	sharedStore := itemstate.NewStore(nil)

	// validateGitHubAppConfig runs first, ahead of the event_source:
	// hookdeck checks below: it is the more specific diagnosis for a
	// partially-configured GitHub App (e.g. github_app_id set with no
	// private key), naming exactly which field is missing. Without this
	// ordering, a partial App config combined with event_source: hookdeck
	// would instead fail on RefuseHookdeckWithoutGitHubApp's coarser
	// "requires GitHub App authentication" message — technically correct
	// (gitHubAppAuthConfigured requires all three fields) but less
	// actionable than naming the specific missing field (PR review
	// finding). resolveGitHubAppAuth calls validateGitHubAppConfig again
	// itself a few lines below; calling it here too is intentionally
	// redundant — cheap, local, and returns the identical error, so the
	// only user-visible effect is which of the two checks reports first.
	if err := validateGitHubAppConfig(cfg); err != nil {
		return nil, err
	}

	// event_source: hookdeck (#1142) validation runs here, alongside (not
	// inside) resolveGitHubAppAuth's own App-auth-specific checks below —
	// deliberately a separate call so RefuseWebhooksWithGitHubApp (#1752)
	// never has to know EventSource exists, and vice versa. Both refusals
	// are cheap, local config checks, so they run before any network call.
	// RefuseUnknownEventSource runs first: a typo'd value must fail loud
	// here rather than silently comparing unequal to EventSourceHookdeck in
	// every check below and in poll.go's dispatch, which would otherwise
	// degrade to plain polling with no error and no log message (PR review
	// finding).
	if err := RefuseUnknownEventSource(cfg.EventSource); err != nil {
		return nil, err
	}
	if err := RefuseHookdeckWithoutGitHubApp(cfg.EventSource, gitHubAppAuthConfigured(cfg)); err != nil {
		return nil, err
	}
	if err := RefuseHookdeckWithWebhooks(cfg.EventSource, cfg.Webhooks); err != nil {
		return nil, err
	}

	// GitHub App auth (#1713): resolved before the ordinary PAT-based client
	// construction below, since a configured App-auth client takes its
	// place entirely. Runs synchronously here, in New() rather than Run(),
	// because e.readClient (further below) captures whichever concrete
	// client is live at construction time — doing this later would leave it
	// wrapping a stale PAT-based client. Only the resulting Reconciler's
	// refresh-loop goroutines need a real, cancellable ctx; that part is
	// started later, from Run(). See engine/github_app_auth.go.
	ghAppClient, ghAppReconciler, err := resolveGitHubAppAuth(context.Background(), cfg, fabrikDir, "")
	if err != nil {
		return nil, err
	}

	var ghClient *gh.Client
	if ghAppClient != nil {
		ghClient = ghAppClient
	} else if cfg.GHESHost != "" {
		ghClient = gh.NewClientForHost(cfg.Token, cfg.GHESHost)
	} else {
		ghClient = gh.NewClient(cfg.Token)
	}
	ghClient.SetMergeStrategy(cfg.AutoMergeStrategy)
	// Worker gh CLI auth (constraint from #1713's research): in App-auth
	// mode there is no static token to inject — read the live, already-
	// refreshed installation token off the minted client instead, riding
	// the same background refresh loop that keeps the engine's own API
	// calls fresh. nil in PAT mode, leaving buildClaudeEnv's existing
	// claudeGHToken path exactly as it was (R1/AC2).
	if ghAppClient != nil {
		claudeGHTokenOverrideFn = ghAppClient.Token
	} else {
		claudeGHTokenOverrideFn = nil
	}
	// Fabrik's own release always lives on github.com/handarbeit/fabrik, never
	// on a customer's GHES instance, so self-upgrade needs a dedicated client
	// pinned to github.com regardless of cfg.GHESHost (see checkReleaseUpgrade).
	// cfg.Token is dropped (releaseUpgradeToken returns "") when a GHES host
	// is configured — it authenticates the GHES instance, not github.com, and
	// would be rejected outright rather than falling back to unauthenticated
	// (see releaseUpgradeToken's doc comment).
	releaseClient := gh.NewClient(releaseUpgradeToken(cfg))
	eng := &Engine{
		cfg:                       cfg,
		client:                    ghClient,
		releaseClient:             releaseClient,
		hostClient:                ghClient,
		ghAppAuth:                 ghAppReconciler,
		claude:                    &RealClaudeInvoker{DebugOutput: cfg.DebugOutput},
		worktreeManagers:          make(map[string]*WorktreeManager),
		fabrikDir:                 fabrikDir,
		store:                     sharedStore,
		mayNeedWork:               make(map[string]bool),
		seededRepos:               make(map[string]bool),
		checkedAutoMergeRepos:     make(map[string]bool),
		repoAccess:                make(map[string]gh.RepoAccess),
		sem:                       make(chan struct{}, cfg.MaxConcurrent),
		mergeTrainEjectionCounts:  make(map[string]int),
		mergeTrainCIDeferred:      make(map[string]string),
		mergeTrainCloneSkipCounts: make(map[string]int),
		mergeTrainTrials:          make(map[string][]time.Time),
		mergeTrainRunawayAlerted:  make(map[string]int),
		queuedReviewEjects:        make(map[string]map[int]int),
		queuedCommentEjects:       make(map[string]map[int]struct{}),
		pauseIssueMu:              make(map[string]*pauseIssueMuEntry),
		sentinelProbeFailures:     make(map[string]int),
		backoffPrevMultiplier:     1,
		backoffRateLimitRatio:     1.0,
	}

	// App-auth's per-repo access signal (#1750 R1): fetched once, eagerly,
	// here — rather than lazily inside resolveRepoAccess — so this single API
	// call can both prime the process-lifetime cache and serve as the one
	// place resolveAppAccessibleRepos' R4 zero-repos hard refusal can run
	// without a second round trip. Skipped entirely in PAT mode
	// (ghAppReconciler == nil), in which case appAccessibleReposReady stays
	// false — resolveAppRepoAccess is never called there.
	if ghAppReconciler != nil {
		result, err := resolveAppAccessibleRepos(ghAppReconciler, cfg.GitHubAppInstallationID)
		if err != nil {
			var noRepos *noAccessibleReposError
			if errors.As(err, &noRepos) {
				// Confirmed, unambiguous misconfiguration (R4): nothing this
				// installation covers will ever dispatch. Refuse startup
				// outright rather than let it run and silently do nothing —
				// mirrors RefuseUserOwnedBoardForAppAuth's precedent for a
				// different structural App-auth misconfiguration.
				return nil, err
			}
			// Any other failure (network error, transient 5xx) is ambiguous,
			// not a definitive "no access" — fail open (R3) by leaving
			// appAccessibleReposReady false, exactly like resolveRepoAccess's
			// own probe-error branch does for PAT mode. Loud rather than a
			// single startup line (R4): this is the entire signal
			// resolveAppRepoAccess will have for every repo until the next
			// restart.
			fmt.Printf("[startup] WARNING: could not list GitHub App installation %d's accessible repositories: %v — "+
				"falling back to fail-open dispatch (assuming every repo is writable) for the rest of this process run\n",
				cfg.GitHubAppInstallationID, err)
		} else {
			eng.appAccessibleRepos = result.repos
			eng.appAccessibleReposTrunc = result.truncated
			eng.appAccessibleReposReady = true
			fmt.Printf("[startup] github-app: installation %d covers %d accessible repositories\n",
				cfg.GitHubAppInstallationID, len(result.repos))
		}
	}

	// Migrate any old-style worktrees (issue-N/) to the new per-repo layout.
	migrateWorktrees(worktreeRoot, func(msg string) { fmt.Printf("[startup] %s", msg) })

	// Migrate any old-style session files (issue-N/) to the new per-repo layout.
	// Must run after migrateWorktrees so namespaced worktree paths exist for remote lookup.
	home, _ := os.UserHomeDir()
	migrateSessions(
		filepath.Join(home, ".fabrik", "sessions"),
		worktreeRoot,
		func(msg string) { fmt.Printf("[startup] %s", msg) },
	)

	// Migrate sessions and logs from ~/.fabrik/ to <cwd>/.fabrik/ (cross-root migration).
	// Must run after migrateSessions so home-dir sessions are already in namespaced layout.
	migrateHomeToProject(fabrikDir, func(msg string) { fmt.Printf("[startup] %s", msg) })

	// Wire the github package's diagnostic logger to engine.logf so retry/
	// degradation warnings (e.g. project board indexer mismatches) reach
	// fabrik.log in both TUI and plain-text modes.
	gh.Logf = eng.logf

	// Initialize the read client. CacheImpl is the unified source of truth: the
	// shared store is passed so all mutations — poll-driven Reconcile, engine-side,
	// and (when enabled) webhook-delta-side — flow through one Store instance and
	// reach the same observers (PushUnblockObserver, etc.).
	adapter := boardcache.NewGitHubAdapter(eng.client)
	cacheLogFn := func(format string, args ...any) { eng.logf(0, "cache", format, args...) }
	eng.readClient = boardcache.NewCacheImpl(adapter, sharedStore, cacheLogFn)

	return eng, nil
}

// NewWithDeps creates an Engine with explicit dependencies (for testing).
// worktrees is a convenience parameter: if non-nil, it is registered as the WM
// for cfg.Owner+"/"+cfg.Repo (or "_test/_test" when cfg is empty).
func NewWithDeps(cfg Config, client GitHubClient, claude ClaudeInvoker, worktrees *WorktreeManager) *Engine {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	wms := make(map[string]*WorktreeManager)
	eng := &Engine{
		cfg:                       cfg,
		client:                    client,
		releaseClient:             client,
		claude:                    claude,
		worktreeManagers:          wms,
		store:                     itemstate.NewStore(nil),
		mayNeedWork:               make(map[string]bool),
		seededRepos:               make(map[string]bool),
		checkedAutoMergeRepos:     make(map[string]bool),
		repoAccess:                make(map[string]gh.RepoAccess),
		sem:                       make(chan struct{}, maxConcurrent),
		mergeTrainEjectionCounts:  make(map[string]int),
		mergeTrainCIDeferred:      make(map[string]string),
		mergeTrainCloneSkipCounts: make(map[string]int),
		mergeTrainTrials:          make(map[string][]time.Time),
		mergeTrainRunawayAlerted:  make(map[string]int),
		queuedReviewEjects:        make(map[string]map[int]int),
		queuedCommentEjects:       make(map[string]map[int]struct{}),
		pauseIssueMu:              make(map[string]*pauseIssueMuEntry),
		sentinelProbeFailures:     make(map[string]int),
		backoffPrevMultiplier:     1,
		backoffRateLimitRatio:     1.0,
	}
	if worktrees != nil {
		worktrees.logfFn = eng.logf
		key := cfg.Owner + "/" + cfg.Repo
		if key == "/" {
			key = "_test/_test"
		}
		wms[key] = worktrees
	}
	// Tests use the pass-through GitHub adapter directly so they exercise engine
	// logic without going through CacheImpl's Reconcile/observer machinery — those
	// are covered by boardcache's own tests. Production wiring (in New) always uses
	// CacheImpl as the unified source of truth.
	eng.readClient = boardcache.NewGitHubAdapter(client)
	return eng
}

// RegisterWorktreeManagerForTest registers wm as the WorktreeManager for
// nameWithOwner ("owner/repo") — bypassing the normal ensureRepoReady/
// ensureBareClone dynamic-clone path production always uses (New's engine
// starts with an empty worktreeManagers map and bare-clones every repo it
// touches, including its own, from e.fabrikDir on first access).
//
// Test seam only (ADR-1449, tests/sim). NewWithDeps's own worktrees
// parameter can register at most one repo, keyed off cfg.Owner+"/"+cfg.Repo
// — sufficient for a single-repo test Env, where that key equals the one
// real repo being tested. A multi-repo test Env (cfg.Repo == "", mirroring
// production's own multi-repo instance topology — see
// spawnTargetServedByThisInstance) has no such single key: NewWithDeps would
// register the one supplied wm under the nonsensical "owner/" key, matching
// no real repo, and every repo the engine actually touches would fall
// through to the dynamic ensureBareClone path — which fails outside a
// network-and-real-GitHub-connected environment, exactly the cost
// tests/sim exists to avoid. This lets a multi-repo Env register one
// pre-built, locally-backed WorktreeManager per repo directly instead.
func (e *Engine) RegisterWorktreeManagerForTest(nameWithOwner string, wm *WorktreeManager) {
	wm.logfFn = e.logf
	e.mu.Lock()
	defer e.mu.Unlock()
	e.worktreeManagers[nameWithOwner] = wm
}

// SetTrainCIPollIntervalForTest overrides pollTrainCI/pollForMergeable's
// hardcoded 30s retry interval — see the trainCIPollInterval field's doc
// comment for why this exists and why it's a duration override rather than a
// Clock-seam concern. Test seam only (ADR-1449, #1452, tests/sim); production
// never calls this, so trainCIPollIntervalOrDefault always returns the
// original 30s literal outside a test.
func (e *Engine) SetTrainCIPollIntervalForTest(d time.Duration) {
	e.trainCIPollInterval = d
}

// SimulateCacheStatusWriteThroughForTest applies the store mutation production's
// boardcache.CacheImpl.UpdateItemStatus performs after an engine-initiated board
// status move (rerouteQueuedMemberOffHolding, advanceToQueued, ...): a
// LocalStatusUpdated, whose StatusChanged flag admits the item to the next poll's
// cycleSet. tests/sim deliberately never wires CacheImpl in (see NewWithDeps), so a
// scenario that depends on the moved item being picked up by the very next poll —
// #1863's comment eject — calls this right after the ejecting poll. Test seam only;
// production never calls this.
func (e *Engine) SimulateCacheStatusWriteThroughForTest(repo string, number int, status string) {
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: repo, Number: number, NewStatus: status})
}

// SetMergeTrainQueueSortDisabledForTest disables groupQueuedByRepoAndBase's
// deterministic Queued-ordering sort (#1833) — see the
// mergeTrainQueueSortDisabledForTest field's doc comment. Test seam only
// (ADR-1833, tests/sim); production never calls this.
func (e *Engine) SetMergeTrainQueueSortDisabledForTest(disabled bool) {
	e.mergeTrainQueueSortDisabledForTest = disabled
}

// SetGitHubAppModeForTest puts e into App-auth mode for the App-auth
// dispatch-admission path (resolveRepoAccess/resolveAppRepoAccess,
// engine/startup.go) without a real JWT, a fake installation-repositories
// HTTP server, or any network call. It sets e.ghAppAuth to a zero-value
// *githubauth.Reconciler (legal from any package; the dispatch-admission
// path only ever checks it for non-nil — it never calls a Reconciler
// method at request time, see resolveAppRepoAccess) and writes
// accessibleRepos/truncated directly into e.appAccessibleRepos/
// e.appAccessibleReposTrunc, marking e.appAccessibleReposReady true —
// mirroring the exact field-write pattern engine/app_repo_access_test.go
// already uses from inside this package. accessibleRepos keys are
// lower-cased "owner/repo", matching appAccessibleRepos' own convention.
//
// Test seam only (ADR-1449, tests/sim, #1751); production only ever
// populates these fields via New()'s resolveGitHubAppAuth/
// resolveAppAccessibleRepos. Not re-applied by RestartEnv — a scenario
// that rebuilds the Engine across a restart must call this again on the
// new instance.
func (e *Engine) SetGitHubAppModeForTest(accessibleRepos map[string]bool, truncated bool) {
	e.ghAppAuth = &githubauth.Reconciler{}
	e.appAccessibleRepos = accessibleRepos
	e.appAccessibleReposTrunc = truncated
	e.appAccessibleReposReady = true
}

// trainCIPollIntervalOrDefault returns the test-overridden CI poll interval
// when set, otherwise the production default of 30 seconds.
func (e *Engine) trainCIPollIntervalOrDefault() time.Duration {
	if e.trainCIPollInterval > 0 {
		return e.trainCIPollInterval
	}
	return 30 * time.Second
}

// SetGeneratedFilesForTest overrides the declared generated-file mapping
// (generatedFiles) with a single synthetic entry: path regenerated by
// running command. Exposes the unexported generatedFileSpec machinery
// (engine/generated_files.go) to an external test package — needed because
// R4's mixed-generated-file conflict scenario (#1452) requires a generated
// path whose regeneration command is cheap and self-contained, unlike
// production's real docs/llms-full.txt regen script, which depends on
// source files no throwaway sim repo has. Test seam only (ADR-1449, #1452,
// tests/sim); production never calls this, so generatedFileSet always
// returns the real generatedFiles mapping outside a test.
func (e *Engine) SetGeneratedFilesForTest(path string, command []string) {
	e.generatedFilesOverride = []generatedFileSpec{{Path: path, Command: command}}
}

// defaultRepo returns "owner/repo" from cfg, or "" if both are empty.
func (e *Engine) defaultRepo() string {
	if e.cfg.Owner == "" && e.cfg.Repo == "" {
		return ""
	}
	return e.cfg.Owner + "/" + e.cfg.Repo
}

// worktreesFor returns the WorktreeManager for the given "owner/repo" key.
// Panics if no WM is registered for that repo — callers must call ensureRepoReady first.
func (e *Engine) worktreesFor(nameWithOwner string) *WorktreeManager {
	if nameWithOwner == "" {
		nameWithOwner = e.defaultRepo()
	}
	e.mu.Lock()
	wm, ok := e.worktreeManagers[nameWithOwner]
	e.mu.Unlock()
	if !ok {
		panic(fmt.Sprintf("engine: no WorktreeManager registered for repo %q — ensureRepoReady not called", nameWithOwner))
	}
	return wm
}

// registerWorktrees adds a WorktreeManager for nameWithOwner to the map.
// Idempotent: if a WM is already registered for this repo, returns the existing one.
func (e *Engine) registerWorktrees(nameWithOwner, baseDir, worktreeRoot string) *WorktreeManager {
	e.mu.Lock()
	defer e.mu.Unlock()
	if wm, ok := e.worktreeManagers[nameWithOwner]; ok {
		return wm
	}
	owner, rname := parseOwnerRepo(nameWithOwner)
	// Use "owner-repo" as directory segment to avoid cross-owner collisions.
	dirName := owner + "-" + rname
	wm := NewWorktreeManagerForRepo(baseDir, worktreeRoot, dirName)
	wm.logfFn = e.logf
	e.worktreeManagers[nameWithOwner] = wm
	return wm
}

// SetWakeCh configures the wake channel. The TUI sends on this channel to
// reset idle backoff and trigger an immediate poll. Must be called before Run().
func (e *Engine) SetWakeCh(ch chan struct{}) {
	e.wakeCh = ch
}

// SetStopCh configures the stop channel. The TUI sends on this channel to
// cancel a specific in-flight issue and apply fabrik:paused. Must be called before Run().
func (e *Engine) SetStopCh(ch chan tui.StopRequest) {
	e.stopCh = ch
}

// SetEvents configures the event channel. Must be called before Run().
// When set, direct stdout writes (pollStatus/pollStatusClear) are suppressed
// because the TUI owns the terminal.
func (e *Engine) SetEvents(ch chan tui.Event) {
	e.events = ch
	tuiMode = ch != nil
	claudeLogf = e.logf
	claudeTUI = ch != nil
	if ch != nil {
		claudeTurnProgress = func(issueNumber, turnsUsed, maxTurns int) {
			e.emit(tui.TurnProgressEvent{
				IssueNumber: issueNumber,
				TurnsUsed:   turnsUsed,
				MaxTurns:    maxTurns,
			})
		}
	} else {
		claudeTurnProgress = nil
	}
	// Update logfFn for all registered WorktreeManagers.
	e.mu.Lock()
	for _, wm := range e.worktreeManagers {
		wm.logfFn = e.logf
	}
	e.mu.Unlock()
}

// SetCleanupHook registers fn to be called exactly once before any force-quit
// path (SIGHUP re-exec, second SIGTERM, second SIGHUP). Wraps fn in sync.Once
// so concurrent force-quit goroutines can't invoke it simultaneously. Must be
// called before Run(). No-op when fn is nil.
func (e *Engine) SetCleanupHook(fn func()) {
	if fn == nil {
		return
	}
	var once sync.Once
	e.cleanupHook = func() { once.Do(fn) }
}

// emit sends an event to the channel without blocking. Dropped if the channel is full.
// Use for high-frequency log events where occasional drops are acceptable.
func (e *Engine) emit(ev tui.Event) {
	if e.events == nil {
		return
	}
	select {
	case e.events <- ev:
	default:
	}
}

// emitStructural sends a structural event (JobStarted, JobCompleted, PollStarted,
// PollCompleted) synchronously. Unlike emit/logf, this blocks if the channel is
// full, but events are never dropped. Use only for low-frequency events that must
// not be lost. Callers are worker goroutines or the poll goroutine; the 256-deep
// buffer ensures blocking is extremely rare in normal operation.
func (e *Engine) emitStructural(ev tui.Event) {
	if e.events == nil {
		return
	}
	e.events <- ev
}

// logf emits a LogEvent to the channel (if configured) or prints directly.
// issueNumber == 0 means a poll-level message; it prints as "[tag]" not "[#0 tag]".
func (e *Engine) logf(issueNumber int, tag, format string, args ...any) {
	e.logEvent(issueNumber, "", tag, format, args...)
}

// logfRepo emits a repo-level LogEvent (IssueNumber 0, Repo set to repoKey) —
// used by the merge train, whose activity has no single issue number but does
// have a repo identity, so it can be routed to that repo's job row in the TUI
// (see tui.LogEvent.Repo) instead of clobbering the header status line.
// Plain-text/log-file output is unchanged from logf's issueNumber==0 case
// (repo is a TUI-routing concern only, not a formatting one).
func (e *Engine) logfRepo(repoKey string, tag, format string, args ...any) {
	e.logEvent(0, repoKey, tag, format, args...)
}

// logEvent is the shared implementation behind logf and logfRepo.
func (e *Engine) logEvent(issueNumber int, repo, tag, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if e.events != nil {
		select {
		case e.events <- tui.LogEvent{IssueNumber: issueNumber, Repo: repo, Tag: tag, Message: msg}:
		default:
		}
	} else {
		// Plain-text mode: clear any transient status line before printing.
		pollStatusClear()
		if issueNumber == 0 {
			fmt.Printf("[%s] %s", tag, msg)
		} else {
			fmt.Printf("[#%d %s] %s", issueNumber, tag, msg)
		}
	}
	// Write to persistent log file in both TUI and plain-text modes.
	e.logMu.Lock()
	if e.logFile != nil {
		ts := time.Now().UTC().Format(time.RFC3339)
		if issueNumber == 0 {
			fmt.Fprintf(e.logFile, "%s [%s] %s", ts, tag, msg)
		} else {
			fmt.Fprintf(e.logFile, "%s [#%d %s] %s", ts, issueNumber, tag, msg)
		}
	}
	e.logMu.Unlock()
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ErrSkipItem is returned by ensureRepoReady when a repo cannot be cloned and
// the item should be skipped for this poll cycle (but not surfaced as an error).
var ErrSkipItem = errors.New("skip item")

// ensureRepoReady guarantees that a WorktreeManager exists for the repo that
// owns item. On first access it bare-clones the repo to .fabrik/repos/<owner>-<repo>.git.
// Subsequent calls are no-ops (idempotent WM registration). If the clone fails
// it posts a comment, adds fabrik:paused and fabrik:awaiting-input labels,
// records a history entry, and returns ErrSkipItem so the caller skips without
// treating it as a hard error.
//
// Concurrent callers for the same repo are serialized via cloneInFlight: the first
// caller performs the clone while others wait. On failure, only the first caller
// posts the comment/labels; waiters silently return ErrSkipItem — unless a waiter
// is the specific item that owned the failed attempt and its own (already-fetched)
// Labels show fabrik:paused has since been removed, in which case it clears the
// failed entry and becomes the new owner (see ADR-1543's identity-gated retry
// boundary, and the cloneCall.ownerKey doc comment).
func (e *Engine) ensureRepoReady(ctx context.Context, item gh.ProjectItem) error {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if owner == "" || repo == "" {
		return nil // cannot determine repo — let processItem handle it
	}
	nameWithOwner := owner + "/" + repo
	worktreeRoot := filepath.Join(e.fabrikDir, ".fabrik", "worktrees")
	callerKey := issueKey(item, e.defaultRepo())

	for {
		// Fast path: already registered (common case after first clone).
		e.mu.Lock()
		_, registered := e.worktreeManagers[nameWithOwner]
		e.mu.Unlock()
		if registered {
			return nil
		}

		// Singleflight-style coordination: elect one goroutine to perform the clone.
		call := &cloneCall{done: make(chan struct{})}
		actual, loaded := e.cloneInFlight.LoadOrStore(nameWithOwner, call)
		if loaded {
			// Another goroutine is already cloning (or has just cloned) this repo.
			existing := actual.(*cloneCall)
			select {
			case <-existing.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			if existing.err != nil {
				// Retry-boundary check: only the item that owned the failed attempt,
				// once its own pause label is confirmed gone, may retry. Any other
				// caller — including same-burst siblings of the original owner —
				// silently skips, exactly as before.
				if existing.ownerKey == callerKey && !hasLabel(item.Labels, "fabrik:paused") {
					// Clear the failed entry so this retry becomes a fresh owner.
					// CompareAndDelete may fail if another goroutine already
					// cleared/replaced it (e.g. a concurrent call for the same
					// item) — either way, re-loop and re-evaluate.
					e.cloneInFlight.CompareAndDelete(nameWithOwner, actual)
					continue
				}
				e.logf(item.Number, "warn", "bare clone of %s already failed for another worker — skipping\n", nameWithOwner)
				return ErrSkipItem
			}
			// Clone succeeded; register the WM using the winner's bareDir.
			e.registerWorktrees(nameWithOwner, existing.dir, worktreeRoot)
			return nil
		}

		// This goroutine is the owner: perform the clone.
		if e.cloneAttemptHook != nil {
			e.cloneAttemptHook(nameWithOwner)
		}
		bareDir, err := ensureBareClone(e.fabrikDir, owner, repo, e.cfg.User, e.cfg.GitSSH, e.cfg.GHESHost)
		call.dir = bareDir
		call.err = err

		if err != nil {
			// Record who owned this failure before releasing waiters, so the
			// retry-boundary check above can identify a legitimate retry later.
			call.ownerKey = callerKey
			// Signal waiters before cleanup so they can read call.err/ownerKey.
			close(call.done)
			// Deliberately NOT deleted here (ADR-1543): deleting before the failure
			// is fully handled let a same-burst sibling become a second owner and
			// duplicate the clone + comment (#1543). The entry now persists until a
			// caller matching ownerKey retries with fabrik:paused confirmed absent.

			msg := fmt.Sprintf("🏭 **Fabrik — cannot clone repo**\n\nFailed to clone `%s/%s`:\n```\n%v\n```\nHuman intervention required. Fix the clone issue and remove `fabrik:paused` to retry.", owner, repo, err)
			e.pauseIssue(item, msg, pauseOpts{
				awaitingInput: true,
				reactRocket:   true,
			})
			// Append a history entry so the TUI records the failure.
			hist := tui.LoadHistory()
			hist = append(hist, tui.HistoryEntry{
				IssueNumber: item.Number,
				Repo:        nameWithOwner,
				Title:       item.Title,
				StageName:   "clone",
				Success:     false,
				CompletedAt: time.Now(),
			})
			tui.SaveHistory(hist)
			e.logf(item.Number, "error", "cannot clone repo %s: %v — pausing issue\n", nameWithOwner, err)
			return ErrSkipItem
		}

		// Success: register the WM, then signal waiters.
		// Leave the cloneInFlight entry in place (closed channel, nil err); future callers
		// will exit at the fast-path registered check before reaching cloneInFlight.
		e.registerWorktrees(nameWithOwner, bareDir, worktreeRoot)
		close(call.done)
		return nil
	}
}

// ensureSpawnTargetReady guarantees that a WorktreeManager exists for
// targetOwner/targetRepo so the pre-Implement spawn step can create child
// issues there. On first access it bare-clones the repo to
// .fabrik/repos/<targetOwner>-<targetRepo>.git via the same singleflight
// coordination used by ensureRepoReady (cloneInFlight key:
// "targetOwner/targetRepo").
//
// Error reporting and label mutations always target parentItem (the spawning
// parent issue), not the target repo — the two repos are different in the
// cross-repo spawn path. Unlike ensureRepoReady's waiters (which silently
// return ErrSkipItem because the reporting item is the same), waiters here
// post their own error comment and labels on parentItem so no parent issue
// silently loses its failure notification when concurrent workers race on the
// same new target repo.
//
// As with ensureRepoReady, a failed cloneInFlight entry is only ever cleared
// by a caller whose own parentItem matches the entry's owner and whose own
// (already-fetched) Labels no longer carry fabrik:paused — the same
// identity-gated retry boundary (ADR-1543), closing the same-burst
// second-owner window that would otherwise run a second concurrent
// `git clone --bare` into the same destination directory.
func (e *Engine) ensureSpawnTargetReady(ctx context.Context, targetOwner, targetRepo string, parentItem gh.ProjectItem) error {
	parentOwner, parentRepo := itemOwnerRepo(parentItem, e.defaultRepo())
	if parentOwner == "" || parentRepo == "" {
		return fmt.Errorf("ensureSpawnTargetReady: cannot determine parent repo for item %d", parentItem.Number)
	}
	nameWithOwner := targetOwner + "/" + targetRepo
	worktreeRoot := filepath.Join(e.fabrikDir, ".fabrik", "worktrees")
	callerKey := issueKey(parentItem, e.defaultRepo())

	for {
		// Fast path: already registered.
		e.mu.Lock()
		_, registered := e.worktreeManagers[nameWithOwner]
		e.mu.Unlock()
		if registered {
			return nil
		}

		// Singleflight coordination: elect one goroutine to perform the clone.
		call := &cloneCall{done: make(chan struct{})}
		actual, loaded := e.cloneInFlight.LoadOrStore(nameWithOwner, call)
		if loaded {
			// Another goroutine is already cloning (or has just cloned) this repo.
			// Each parent item is independent here, so every waiter that observes a
			// clone failure must post its own error comment — unlike ensureRepoReady
			// waiters, which silently skip because the same item owns all call sites.
			existing := actual.(*cloneCall)
			select {
			case <-existing.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			if existing.err != nil {
				// Retry-boundary check: only the parent that owned the failed
				// attempt, once its own pause label is confirmed gone, may retry.
				if existing.ownerKey == callerKey && !hasLabel(parentItem.Labels, "fabrik:paused") {
					// Clear the failed entry so this retry becomes a fresh owner.
					// CompareAndDelete may fail if another goroutine already
					// cleared/replaced it — either way, re-loop and re-evaluate.
					e.cloneInFlight.CompareAndDelete(nameWithOwner, actual)
					continue
				}
				e.postSpawnCloneError(parentOwner, parentRepo, parentItem, targetOwner, targetRepo, existing.err)
				return fmt.Errorf("ensureSpawnTargetReady: clone of %s/%s failed: %w", targetOwner, targetRepo, existing.err)
			}
			e.registerWorktrees(nameWithOwner, existing.dir, worktreeRoot)
			return nil
		}

		// This goroutine is the owner: perform the clone.
		if e.cloneAttemptHook != nil {
			e.cloneAttemptHook(nameWithOwner)
		}
		bareDir, err := ensureBareClone(e.fabrikDir, targetOwner, targetRepo, e.cfg.User, e.cfg.GitSSH, e.cfg.GHESHost)
		call.dir = bareDir
		call.err = err

		if err != nil {
			// Record who owned this failure before releasing waiters, so the
			// retry-boundary check above can identify a legitimate retry later.
			call.ownerKey = callerKey
			// Signal waiters before cleanup so they can read call.err/ownerKey.
			close(call.done)
			// Deliberately NOT deleted here (ADR-1543): see ensureRepoReady's
			// identical comment. A second owner here would run a second concurrent
			// `git clone --bare` into the same directory — the exact corruption
			// race ADR-022 exists to prevent, so this matters more here, not less.
			e.postSpawnCloneError(parentOwner, parentRepo, parentItem, targetOwner, targetRepo, err)
			return fmt.Errorf("ensureSpawnTargetReady: clone of %s/%s: %w", targetOwner, targetRepo, err)
		}

		// Success: register WM, then signal waiters.
		e.registerWorktrees(nameWithOwner, bareDir, worktreeRoot)
		close(call.done)
		return nil
	}
}

// postSpawnCloneError posts an error comment and adds fabrik:paused +
// fabrik:awaiting-input to parentItem when an on-demand clone of a spawn
// target repo fails.
func (e *Engine) postSpawnCloneError(parentOwner, parentRepo string, parentItem gh.ProjectItem, targetOwner, targetRepo string, cloneErr error) {
	msg := fmt.Sprintf("🏭 **Fabrik — pre-Implement spawn failed**\n\nFailed to clone spawn target `%s/%s`:\n```\n%v\n```\nFix the clone issue (SSH key, PAT access) and remove `fabrik:paused` to retry.",
		targetOwner, targetRepo, cloneErr)
	e.pauseIssue(parentItem, msg, pauseOpts{
		awaitingInput: true,
	})
	e.logf(parentItem.Number, "error", "cannot clone spawn target %s/%s: %v — pausing parent\n", targetOwner, targetRepo, cloneErr)
}
