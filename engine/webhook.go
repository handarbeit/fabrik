package engine

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/verveguy/fabrik/tui"
)

// WebhookHealthState indicates the health of the gh webhook forward stream.
type WebhookHealthState string

const (
	WebhookStreamStartingUp WebhookHealthState = "starting_up"
	WebhookStreamHealthy    WebhookHealthState = "healthy"
	WebhookStreamUnhealthy  WebhookHealthState = "unhealthy"
)

// Internal constants — not user-configurable in v1.
const (
	webhookIdleCap           = 60 * time.Minute
	webhookHealthWindow      = 10 * time.Minute
	webhookStartupGrace      = 30 * time.Second
	webhookMaxRestartBackoff = 60 * time.Second
	webhookRotationFailures  = 5
	webhookRotationWindow    = 2 * time.Minute
	webhookRotationMaxCycles = 2
	// orgModeProbeTimeout: if subprocess exits within this duration of starting
	// in org mode, treat it as an org-permission failure and fall back to per-repo.
	orgModeProbeTimeout = 10 * time.Second
)

// defaultWebhookEvents is the canonical event list from the spec (R6).
var defaultWebhookEvents = []string{
	"issue_comment",
	"issues",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
	"check_run",
	"check_suite",
	"projects_v2_item",
}

// webhookManager manages the gh webhook forward subprocess lifecycle,
// the local HTTP listener, HMAC verification, health tracking, and secret rotation.
// It is self-contained: no back-reference to Engine; all dependencies are injected.
type webhookManager struct {
	mu sync.Mutex

	// injected dependencies
	logFn  func(issueNumber int, tag, format string, args ...any)
	wakeCh chan struct{}
	emitFn func(tui.Event)

	// listener — bound once before first subprocess start, reused across restarts
	listener net.Listener
	port     int

	// subprocess state (protected by mu)
	currentCmd      *exec.Cmd
	secret          string
	repos           map[string]bool
	events          []string
	orgModeFailed   bool // true after org-level probe fails; use per-repo thereafter
	stopOnce        sync.Once
	stopCh          chan struct{}

	// health (protected by mu)
	state         WebhookHealthState
	startupTime   time.Time // when current subprocess started
	lastEventTime time.Time // zero until first verified event received

	// secret rotation (protected by mu)
	consecutiveFailures int
	firstFailureAt      time.Time
	rotateCycleCount    int
	disabled            bool

	// per-type event counters (protected by mu)
	eventCounts map[string]int
}

func newWebhookManager(
	logFn func(int, string, string, ...any),
	wakeCh chan struct{},
	emitFn func(tui.Event),
	repos map[string]bool,
	events []string,
) *webhookManager {
	evts := events
	if len(evts) == 0 {
		evts = make([]string, len(defaultWebhookEvents))
		copy(evts, defaultWebhookEvents)
	}
	return &webhookManager{
		logFn:       logFn,
		wakeCh:      wakeCh,
		emitFn:      emitFn,
		repos:       copyRepoSet(repos),
		events:      evts,
		state:       WebhookStreamUnhealthy, // becomes StartingUp when subprocess launches
		eventCounts: make(map[string]int),
		stopCh:      make(chan struct{}),
	}
}

func copyRepoSet(repos map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(repos))
	for r := range repos {
		cp[r] = true
	}
	return cp
}

// generateSecret creates a random 32-byte hex-encoded webhook secret.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating webhook secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// verifySignature checks the HMAC-SHA256 signature on a webhook payload.
// sig is the value of the X-Hub-Signature-256 header (format: "sha256=<hex>").
func verifySignature(body []byte, sig, secret string) bool {
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	sigHex := sig[len("sha256="):]
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), sigBytes)
}

// ghVersionCheck verifies gh is installed and meets the minimum version (≥ 2.32.0).
func ghVersionCheck() error {
	path, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh CLI not found in PATH: webhooks require gh ≥ 2.32.0 (install from https://cli.github.com)")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return fmt.Errorf("gh --version failed: %w", err)
	}
	// Output: "gh version 2.40.1 (2023-12-13)\n..."
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	for i, p := range strings.Fields(line) {
		if p == "version" && i+1 < len(strings.Fields(line)) {
			ver := strings.Fields(line)[i+1]
			if !semverAtLeast(ver, 2, 32, 0) {
				return fmt.Errorf("gh version %s is too old; webhooks require gh ≥ 2.32.0 (upgrade from https://cli.github.com)", ver)
			}
			return nil
		}
	}
	return fmt.Errorf("could not parse gh version from: %q", line)
}

// semverAtLeast returns true when vStr (e.g. "2.40.1") is ≥ major.minor.patch.
func semverAtLeast(vStr string, major, minor, patch int) bool {
	vStr = strings.TrimPrefix(vStr, "v")
	parts := strings.SplitN(vStr, ".", 3)
	vals := make([]int, 3)
	for i, p := range parts {
		// strip any non-numeric suffix (e.g. "-rc1")
		for j, c := range p {
			if c < '0' || c > '9' {
				p = p[:j]
				break
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		vals[i] = n
	}
	threshold := []int{major, minor, patch}
	for i := 0; i < 3; i++ {
		if vals[i] > threshold[i] {
			return true
		}
		if vals[i] < threshold[i] {
			return false
		}
	}
	return true // equal
}

// detectOrgMode returns the common owner when all repos share the same GitHub org/user,
// enabling --org=<org> subscription instead of per-repo subscriptions.
func detectOrgMode(repos map[string]bool) (string, bool) {
	var org string
	for r := range repos {
		owner := strings.SplitN(r, "/", 2)[0]
		if org == "" {
			org = owner
		} else if org != owner {
			return "", false
		}
	}
	return org, org != ""
}

// buildGhArgs constructs the argument list for `gh webhook forward`.
func buildGhArgs(org string, repos []string, port int, secret string, events []string) []string {
	args := []string{
		"webhook", "forward",
		"--secret=" + secret,
		"--url=http://127.0.0.1:" + strconv.Itoa(port) + "/",
	}
	if org != "" {
		args = append(args, "--org="+org)
	} else {
		for _, r := range repos {
			args = append(args, "--repo="+r)
		}
	}
	args = append(args, "--events="+strings.Join(events, ","))
	return args
}

// startListener binds the HTTP listener on 127.0.0.1:<port> (0 = OS-assigned).
func startListener(port int) (net.Listener, int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, 0, fmt.Errorf("binding webhook listener on %s: %w", addr, err)
	}
	return l, l.Addr().(*net.TCPAddr).Port, nil
}

// Start initializes the webhook manager: binds the HTTP listener and starts the supervisor.
// On any hard failure (gh not found, listener bind fails), returns a non-nil error so the
// caller can fall back to polling-only. All other failures (subprocess crashes, etc.) are
// handled internally and are non-fatal.
func (wm *webhookManager) Start(ctx context.Context, port int) error {
	if err := ghVersionCheck(); err != nil {
		wm.logFn(0, "webhook", "prerequisite check failed (falling back to polling only): %v\n", err)
		return err
	}

	l, actualPort, err := startListener(port)
	if err != nil {
		wm.logFn(0, "webhook", "could not bind listener (falling back to polling only): %v\n", err)
		return err
	}

	wm.mu.Lock()
	wm.listener = l
	wm.port = actualPort
	wm.mu.Unlock()

	// Start HTTP server; lives until listener is closed in Stop().
	mux := http.NewServeMux()
	mux.HandleFunc("/", wm.handleWebhook)
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
			wm.logFn(0, "webhook", "HTTP server error: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	// Generate initial secret.
	secret, err := generateSecret()
	if err != nil {
		l.Close()
		return fmt.Errorf("generating webhook secret: %w", err)
	}
	wm.mu.Lock()
	wm.secret = secret
	wm.mu.Unlock()

	go wm.supervise(ctx)
	go wm.runHealthMonitor(ctx)

	wm.logFn(0, "webhook", "webhook listener started on port %d\n", actualPort)
	return nil
}

// Stop shuts down the webhook manager: closes the listener and kills the subprocess.
func (wm *webhookManager) Stop() {
	wm.stopOnce.Do(func() {
		close(wm.stopCh)
	})
	wm.mu.Lock()
	cmd := wm.currentCmd
	l := wm.listener
	wm.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		killProcGroup(cmd)
	}
	if l != nil {
		l.Close()
	}
}

// IsHealthyOrStartingUp returns true when the extended idle cap (60 min) should apply.
func (wm *webhookManager) IsHealthyOrStartingUp() bool {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	return wm.state == WebhookStreamStartingUp || wm.state == WebhookStreamHealthy
}

// UpdateRepos is called after each board poll. When new repos appear, the subprocess
// is restarted with the updated repo set so it subscribes to new repos.
func (wm *webhookManager) UpdateRepos(repos map[string]bool) {
	wm.mu.Lock()
	if wm.disabled {
		wm.mu.Unlock()
		return
	}
	hasNew := false
	for r := range repos {
		if !wm.repos[r] {
			wm.logFn(0, "webhook", "new repo discovered: %s — restarting webhook subscription\n", r)
			hasNew = true
		}
	}
	if !hasNew {
		wm.mu.Unlock()
		return
	}
	wm.repos = copyRepoSet(repos)
	cmd := wm.currentCmd
	wm.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		killProcGroup(cmd)
	}
}

// supervise manages the subprocess lifecycle: start, wait for exit, restart with backoff.
func (wm *webhookManager) supervise(ctx context.Context) {
	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-wm.stopCh:
			return
		default:
		}

		wm.mu.Lock()
		if wm.disabled {
			wm.mu.Unlock()
			return
		}
		secret := wm.secret
		repos := make([]string, 0, len(wm.repos))
		for r := range wm.repos {
			repos = append(repos, r)
		}
		events := wm.events
		port := wm.port
		orgModeFailed := wm.orgModeFailed
		// Detect org mode inside the lock so wm.repos isn't read concurrently
		// with UpdateRepos mutations.
		org := ""
		if !orgModeFailed {
			if o, ok := detectOrgMode(wm.repos); ok {
				org = o
			}
		}
		wm.mu.Unlock()

		args := buildGhArgs(org, repos, port, secret, events)
		startedAt := time.Now()
		cmd, err := wm.startSubprocessInternal(ctx, args)
		if err != nil {
			wm.logFn(0, "webhook", "failed to start gh webhook forward: %v\n", err)
			select {
			case <-ctx.Done():
				return
			case <-wm.stopCh:
				return
			case <-time.After(backoff):
				backoff = minWebhookDuration(backoff*2, webhookMaxRestartBackoff)
				continue
			}
		}
		backoff = time.Second // reset on successful start

		wm.mu.Lock()
		wm.currentCmd = cmd
		wm.state = WebhookStreamStartingUp
		wm.startupTime = time.Now()
		wm.lastEventTime = time.Time{}
		wm.mu.Unlock()
		wm.emitCurrentState()

		// Wait for subprocess exit.
		waitErr := cmd.Wait()
		elapsed := time.Since(startedAt)

		select {
		case <-ctx.Done():
			return
		case <-wm.stopCh:
			return
		default:
		}

		// Detect org mode rejection: subprocess exits quickly when org perm is missing.
		if org != "" && elapsed < orgModeProbeTimeout {
			wm.logFn(0, "webhook", "org-level webhook exited in %v — falling back to per-repo subscription\n", elapsed.Round(time.Millisecond))
			wm.mu.Lock()
			wm.orgModeFailed = true
			wm.mu.Unlock()
			// No backoff for org mode probe failure — restart immediately.
			continue
		}

		if waitErr != nil {
			wm.logFn(0, "webhook", "gh webhook forward exited: %v — restarting in %v\n", waitErr, backoff)
		} else {
			wm.logFn(0, "webhook", "gh webhook forward exited — restarting in %v\n", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-wm.stopCh:
			return
		case <-time.After(backoff):
			backoff = minWebhookDuration(backoff*2, webhookMaxRestartBackoff)
		}
	}
}

// startSubprocessInternal starts `gh webhook forward` with the given args.
// Stderr is captured to the engine log. Returns the started cmd.
func (wm *webhookManager) startSubprocessInternal(ctx context.Context, args []string) (*exec.Cmd, error) {
	_ = ctx // kept for future use (CommandContext replacement)
	cmd := exec.Command("gh", args...)
	setCmdProcAttr(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting gh webhook forward: %w", err)
	}

	// Drain stderr to engine log; detect projects_v2_item rejection.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				line := strings.TrimRight(string(buf[:n]), "\n\r")
				// Detect projects_v2_item rejection and remove it from the event list.
				if strings.Contains(line, "projects_v2_item") &&
					(strings.Contains(strings.ToLower(line), "invalid") || strings.Contains(strings.ToLower(line), "unknown")) {
					wm.logFn(0, "webhook", "WARNING: projects_v2_item event not supported by gh webhook forward — "+
						"board-column changes caught by safety-net poll only (up to 60 min delay)\n")
					wm.mu.Lock()
					filtered := make([]string, 0, len(wm.events))
					for _, e := range wm.events {
						if e != "projects_v2_item" {
							filtered = append(filtered, e)
						}
					}
					wm.events = filtered
					c := wm.currentCmd
					wm.mu.Unlock()
					// Kill subprocess so supervise restarts without projects_v2_item.
					if c != nil && c.Process != nil {
						killProcGroup(c)
					}
					return
				}
				wm.logFn(0, "webhook", "[gh] %s\n", line)
			}
			if err != nil {
				break
			}
		}
	}()

	return cmd, nil
}

// runHealthMonitor periodically checks time-based health state transitions.
func (wm *webhookManager) runHealthMonitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wm.stopCh:
			return
		case <-ticker.C:
			wm.checkHealthTransitions()
		}
	}
}

func (wm *webhookManager) checkHealthTransitions() {
	wm.mu.Lock()
	now := time.Now()
	state := wm.state
	var newState WebhookHealthState

	switch state {
	case WebhookStreamStartingUp:
		if !wm.lastEventTime.IsZero() {
			// Event received; HTTP handler already set state to Healthy.
			// This is a no-op catch.
		} else if !wm.startupTime.IsZero() && now.Sub(wm.startupTime) > webhookStartupGrace+webhookHealthWindow {
			newState = WebhookStreamUnhealthy
		}
	case WebhookStreamHealthy:
		if !wm.lastEventTime.IsZero() && now.Sub(wm.lastEventTime) > webhookHealthWindow {
			newState = WebhookStreamUnhealthy
		}
	}

	if newState != "" && newState != state {
		wm.state = newState
		wm.mu.Unlock()
		wm.logFn(0, "webhook", "health state: %s → %s\n", state, newState)
		wm.emitCurrentState()
		return
	}
	wm.mu.Unlock()
}

// emitCurrentState emits a WebhookStatusEvent with the current state and event counts.
func (wm *webhookManager) emitCurrentState() {
	if wm.emitFn == nil {
		return
	}
	wm.mu.Lock()
	state := string(wm.state)
	counts := make(map[string]int, len(wm.eventCounts))
	for k, v := range wm.eventCounts {
		counts[k] = v
	}
	wm.mu.Unlock()
	wm.emitFn(tui.WebhookStatusEvent{
		State:       state,
		EventCounts: counts,
	})
}

// minWebhookPayload is the minimal set of fields read from each incoming webhook payload.
type minWebhookPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue *struct {
		Number int `json:"number"`
	} `json:"issue"`
	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request"`
}

// handleWebhook is the HTTP handler for incoming webhook POSTs from gh webhook forward.
func (wm *webhookManager) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		http.Error(w, "error reading body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	wm.mu.Lock()
	secret := wm.secret
	wm.mu.Unlock()

	sig := r.Header.Get("X-Hub-Signature-256")
	if !verifySignature(body, sig, secret) {
		wm.mu.Lock()
		now := time.Now()
		// Reset the failure window when it has elapsed so spread-out failures
		// don't anchor the window to a stale start time.
		if wm.consecutiveFailures == 0 || wm.firstFailureAt.IsZero() || now.Sub(wm.firstFailureAt) > webhookRotationWindow {
			wm.firstFailureAt = now
			wm.consecutiveFailures = 1
		} else {
			wm.consecutiveFailures++
		}
		failures := wm.consecutiveFailures
		firstAt := wm.firstFailureAt
		// Make the threshold check and state update atomic so concurrent
		// requests cannot both trigger a rotation or double-disable.
		shouldDisable := false
		shouldRotate := false
		if !wm.disabled && failures >= webhookRotationFailures && now.Sub(firstAt) <= webhookRotationWindow {
			if wm.rotateCycleCount >= webhookRotationMaxCycles {
				wm.disabled = true
				wm.state = WebhookStreamUnhealthy
				shouldDisable = true
			} else {
				// Reserve this threshold breach; reset counters so a concurrent
				// request doesn't also trigger rotation.
				wm.consecutiveFailures = 0
				wm.firstFailureAt = time.Time{}
				shouldRotate = true
			}
		}
		cmd := wm.currentCmd
		wm.mu.Unlock()

		wm.logFn(0, "webhook", "HMAC verification failed (consecutive: %d)\n", failures)

		if shouldDisable {
			wm.logFn(0, "webhook", "HMAC failures persist after %d rotation cycles — "+
				"disabling webhook mode for this session; run 'gh auth status' and restart Fabrik\n",
				webhookRotationMaxCycles)
			wm.emitCurrentState()
			if cmd != nil && cmd.Process != nil {
				killProcGroup(cmd)
			}
		} else if shouldRotate {
			wm.rotateSecret()
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Signature verified — reset failure counter.
	wm.mu.Lock()
	wm.consecutiveFailures = 0
	wm.firstFailureAt = time.Time{}
	wm.mu.Unlock()

	// Parse minimal fields for logging (no payload contents logged — PII risk).
	eventType := r.Header.Get("X-GitHub-Event")
	var payload minWebhookPayload
	_ = json.Unmarshal(body, &payload) // best-effort

	issueNum := 0
	if payload.Issue != nil {
		issueNum = payload.Issue.Number
	} else if payload.PullRequest != nil {
		issueNum = payload.PullRequest.Number
	}
	wm.logFn(0, "webhook", "event: type=%s action=%s repo=%s num=%d\n",
		eventType, payload.Action, payload.Repository.FullName, issueNum)

	// Update state and counters.
	wm.mu.Lock()
	wm.eventCounts[eventType]++
	prevState := wm.state
	if prevState != WebhookStreamHealthy {
		wm.state = WebhookStreamHealthy
	}
	wm.lastEventTime = time.Now()
	wm.mu.Unlock()
	if prevState != WebhookStreamHealthy {
		wm.logFn(0, "webhook", "health state: %s → %s\n", prevState, WebhookStreamHealthy)
	}

	wm.emitCurrentState()

	// Wake the poll loop immediately.
	select {
	case wm.wakeCh <- struct{}{}:
	default:
	}

	w.WriteHeader(http.StatusOK)
}

// rotateSecret generates a new secret, kills the current subprocess, and lets supervise restart.
func (wm *webhookManager) rotateSecret() {
	secret, err := generateSecret()
	if err != nil {
		wm.logFn(0, "webhook", "failed to generate replacement secret: %v\n", err)
		return
	}
	wm.mu.Lock()
	wm.secret = secret
	wm.consecutiveFailures = 0
	wm.firstFailureAt = time.Time{}
	wm.rotateCycleCount++
	cycle := wm.rotateCycleCount
	cmd := wm.currentCmd
	wm.mu.Unlock()

	wm.logFn(0, "webhook", "rotating webhook secret (cycle %d/%d) — restarting subprocess\n",
		cycle, webhookRotationMaxCycles)

	if cmd != nil && cmd.Process != nil {
		killProcGroup(cmd)
	}
}

func minWebhookDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
