---
layout: default
title: Automated Claude Code SDLC Orchestration
description: >-
  Fabrik watches your GitHub Project board and drives Claude Code through a
  configurable SDLC pipeline. File an issue, drag it across the board, let the
  factory run.
---

<!-- ============================================================ -->
<!-- HERO -->
<!-- ============================================================ -->
<section class="hero">
  <div class="container">
    <div class="hero-eyebrow">🏭 Free CLI Tool</div>
    <h1>Your SDLC,<br>on <span class="accent">autopilot</span></h1>
    <p class="hero-tagline">
      Fabrik watches your GitHub Project board and drives Claude Code through
      a full software development pipeline — Specify, Research, Plan, Implement,
      Review, Validate — automatically. File an issue. Drag a card. Ship.
    </p>
    <div class="hero-actions">
      <a href="https://github.com/handarbeit/fabrik" class="btn btn-primary" target="_blank" rel="noopener">
        ★ View on GitHub
      </a>
      <a href="#quickstart" class="btn btn-secondary">
        Get Started →
      </a>
    </div>

    <div class="hero-diagram">
      <div class="diagram-line">
        <span class="dl-icon">📋</span>
        <span class="dl-label">GitHub Project Board</span>
        <span class="dl-desc">source of truth</span>
      </div>
      <div class="diagram-arrow">↓ GraphQL poll</div>
      <div class="diagram-line">
        <span class="dl-icon">🏭</span>
        <span class="dl-label">Fabrik</span>
        <span class="dl-desc">Go CLI, runs locally</span>
      </div>
      <div class="diagram-arrow">↓ stage config match</div>
      <div class="diagram-line">
        <span class="dl-icon">🤖</span>
        <span class="dl-label">Claude Code</span>
        <span class="dl-desc">invoked per stage, isolated worktree</span>
      </div>
      <div class="diagram-arrow">↓ output</div>
      <div class="diagram-line">
        <span class="dl-icon">💬</span>
        <span class="dl-label">GitHub Comments + Labels + PRs</span>
        <span class="dl-desc">durable state</span>
      </div>
    </div>
  </div>
</section>

<!-- ============================================================ -->
<!-- HOW IT WORKS + DEMO VIDEOS -->
<!-- ============================================================ -->
<section class="how-it-works" id="how-it-works">
  <div class="container">
    <p class="section-label">How It Works</p>
    <h2 class="section-title">Issues in. Code out.</h2>
    <p class="section-subtitle">
      Each issue moves through board columns that map to pipeline stages.
      Fabrik polls the board, matches the issue's column to a stage config,
      spins up an isolated git worktree, and invokes Claude Code with the
      stage's prompt.
    </p>

    <div class="pipeline">
      <div class="pipeline-stage">
        <div class="stage-num">01</div>
        <div class="stage-name">Specify</div>
        <div class="stage-desc">Refines rough issues into clear specs via Q&amp;A</div>
      </div>
      <div class="pipeline-stage">
        <div class="stage-num">02</div>
        <div class="stage-name">Research</div>
        <div class="stage-desc">Explores codebase, identifies scope</div>
      </div>
      <div class="pipeline-stage">
        <div class="stage-num">03</div>
        <div class="stage-name">Plan</div>
        <div class="stage-desc">Designs approach, breaks into tasks</div>
      </div>
      <div class="pipeline-stage stage-active">
        <div class="stage-num">04</div>
        <div class="stage-name">Implement</div>
        <div class="stage-desc">Writes code, tests, and commits to branch</div>
      </div>
      <div class="pipeline-stage">
        <div class="stage-num">05</div>
        <div class="stage-name">Review</div>
        <div class="stage-desc">Rebases, reviews, and fixes the PR</div>
      </div>
      <div class="pipeline-stage">
        <div class="stage-num">06</div>
        <div class="stage-name">Validate</div>
        <div class="stage-desc">Runs tests, verifies requirements</div>
      </div>
    </div>

    <div class="demo-videos">
      <div class="video-container">
        <img src="{{ '/assets/images/fabrik-tui.png' | relative_url }}" alt="Fabrik TUI showing active pipeline jobs" style="width:100%; border-radius:8px;">
        <div class="video-caption">
          <div class="caption-title">The Fabrik TUI Control Panel</div>
          <div class="caption-desc">Active jobs, history, cost tracking — live in your terminal</div>
        </div>
      </div>
      <div class="video-container">
        <img src="{{ '/assets/images/fabrik-board.png' | relative_url }}" alt="GitHub Project Board with Fabrik pipeline stages" style="width:100%; border-radius:8px;">
        <div class="video-caption">
          <div class="caption-title">GitHub Project Board in Action</div>
          <div class="caption-desc">Drag issues across columns to control the pipeline; comment to steer Claude</div>
        </div>
      </div>
    </div>
  </div>
</section>

<!-- ============================================================ -->
<!-- FEATURES -->
<!-- ============================================================ -->
<section id="features">
  <div class="container">
    <p class="section-label">Features</p>
    <h2 class="section-title">Everything you need to automate your SDLC</h2>
    <p class="section-subtitle">
      Fabrik gives Claude Code the structure, context, and tooling to work
      reliably through a full software lifecycle — with you in the loop at
      every step.
    </p>

    <div class="features-grid">
      <div class="feature-card">
        <span class="feature-icon">📋</span>
        <div class="feature-title">GitHub-Native Pipeline</div>
        <div class="feature-desc">
          Board columns <em>are</em> stages. Move a card to trigger a stage.
          Labels track completion state. No external CI glue required.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🌿</span>
        <div class="feature-title">Isolated Git Worktrees</div>
        <div class="feature-desc">
          Each issue gets its own worktree at <code>.fabrik/worktrees/&lt;owner&gt;-&lt;repo&gt;/issue-N/</code>
          on branch <code>fabrik/issue-N</code>. Multiple issues run in parallel,
          zero cross-contamination. The <code>base:&lt;branch&gt;</code> label overrides
          the fork and PR target branch per issue — useful for feature branches and release trains.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">💬</span>
        <div class="feature-title">Comment-Driven Steering</div>
        <div class="feature-desc">
          Comment on an issue mid-stage to redirect the work.
          Fabrik reacts with 👀, processes the comment, and continues.
          Comments from any author trigger processing — colleagues, code-review bots,
          and other Fabrik instances alike. Only Fabrik's own output and
          rocket-reacted (already-processed) comments are skipped.
          When Claude needs input, it signals <code>FABRIK_BLOCKED_ON_INPUT</code> —
          the issue pauses and automatically resumes the moment you reply.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">⚡</span>
        <div class="feature-title">Yolo Mode &amp; Auto-Merge</div>
        <div class="feature-desc">
          Enable <code>--yolo</code> and Fabrik auto-advances issues through
          every stage without human approval — and auto-merges the PR when
          Validate completes. Apply the <code>fabrik:yolo</code> label to a
          single issue to get the same behavior scoped to just that issue.
          For lighter automation without auto-merge, use <code>fabrik:cruise</code> —
          it auto-advances through all stages but stops at Validate, leaving
          the merge decision to you.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🔧</span>
        <div class="feature-title">Configurable Stages</div>
        <div class="feature-desc">
          Each stage is a YAML file: custom prompt, model choice, tool restrictions,
          max turns, PR posting, and more. Ship the default pipeline or build your own.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">👥</span>
        <div class="feature-title">Multi-User Safe</div>
        <div class="feature-desc">
          Run multiple Fabrik instances against the same board.
          <code>fabrik:locked:&lt;user&gt;</code> labels prevent conflicts.
          Stage work is scoped to each instance's <code>--user</code>;
          comment processing handles comments from any author.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">✅</span>
        <div class="feature-title">Startup Board Validation</div>
        <div class="feature-desc">
          On every startup, Fabrik validates that stage names in your YAML configs
          match board column names exactly. Mismatches are reported with a clear
          error before any work begins — no silent misconfiguration.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🔄</span>
        <div class="feature-title">Self-Upgrade</div>
        <div class="feature-desc">
          Pass <code>--auto-upgrade</code> and Fabrik keeps itself current when idle.
          Release binaries check <code>handarbeit/fabrik</code> GitHub Releases and
          download the latest version. Dev builds (built from source) detect local or
          remote commits ahead of the running binary and rebuild in place — both paths
          re-exec automatically with no manual deploys.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🔀</span>
        <div class="feature-title">PR Lifecycle Management</div>
        <div class="feature-desc">
          Implement creates a draft PR. Review rebases, fixes conflicts, posts
          detailed output. Validate marks the PR ready. Full lifecycle, zero
          manual steps.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🖥️</span>
        <div class="feature-title">Terminal UI</div>
        <div class="feature-desc">
          Fabrik launches a bubbletea control panel by default: active jobs,
          stage progress, token costs, and history across all issues. Press
          <code>l</code> to open <code>fabrik watch</code> for the selected issue
          (live log streaming, stage tabs, CI/PR status), <code>enter</code> to
          toggle an inline detail panel, <code>r</code> to resume a Claude session
          from history, <code>?</code> to open a help panel (all keybindings and
          labels reference), and <code>q</code> to quit. In supported terminals (Ghostty, iTerm2, WezTerm, Kitty), <strong>Cmd+click</strong> (macOS) or <strong>Ctrl+click</strong> (Linux) on issue numbers to open them in your browser, or on the board title in the footer to open the project board.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🗂️</span>
        <div class="feature-title">Multi-Repo Support</div>
        <div class="feature-desc">
          Fabrik always bare-clones every managed repository on first access.
          Run it from any directory — no checkout needed. Repos are discovered
          lazily from the project board; worktrees are created per repo and per
          issue automatically. Enable multi-repo mode by omitting (or commenting
          out) <code>repo:</code> in <code>.fabrik/config.yaml</code> — Fabrik
          then processes issues from all repositories on the board.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🧩</span>
        <div class="feature-title">Plugin &amp; Skills</div>
        <div class="feature-desc">
          Customize Claude's methodology per stage with markdown skill files in
          <code>.fabrik/plugin/skills/</code>. Reference a skill by name in your
          stage YAML — Fabrik injects it into Claude's prompt. Default skills
          ship with Fabrik; <code>fabrik init</code> installs them and
          <code>fabrik upgrade</code> refreshes them.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🔗</span>
        <div class="feature-title">Formations</div>
        <div class="feature-desc">
          Coordinate multi-issue projects using GitHub's native "Blocked by"
          relationships. Independent issues execute in parallel; dependent issues
          gate automatically. The <code>fabrik:blocked</code> label is managed
          entirely by the engine — no pre-creation needed. See the
          <a href="{{ '/USER_GUIDE' | relative_url }}#dependency-based-sequencing-formations">USER_GUIDE §3</a>
          for the full recipe and real-world validation numbers.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">👁️</span>
        <div class="feature-title">Pending Reviewer Gate</div>
        <div class="feature-desc">
          Set <code>wait_for_reviews: true</code> on a stage and Fabrik uses
          a three-phase gate: (1) immediately apply <code>fabrik:awaiting-review</code>
          on completion, (2) poll until all requested reviewers submit, (3)
          unconditionally re-invoke the stage agent to address any unresolved
          inline PR review thread comments — regardless of whether auto-advance
          is active. Reviews with only top-level text (e.g., bot approvals or
          summary-only comments) skip re-invocation. Auto-advancement to the
          next stage after re-invocation still requires auto-advance to be
          active. Controlled by <code>--review-wait-timeout</code> /
          <code>FABRIK_REVIEW_WAIT_TIMEOUT</code> (default 15
          min per cycle) and <code>--max-review-cycles</code> /
          <code>FABRIK_MAX_REVIEW_CYCLES</code> (default 5
          cycles). Timeout or cycle-limit exceeded pauses the issue with
          <code>fabrik:awaiting-input</code> for human review.
        </div>
      </div>
    </div>

    <div class="factory-callout">
      <div class="callout-icon">🔁</div>
      <div>
        <div class="callout-label">The Self-Evolving Factory</div>
        <div class="callout-title">Fabrik is built with Fabrik</div>
        <div class="callout-body">
          Issues filed against this repository go through the same Specify → Research →
          Plan → Implement → Review → Validate pipeline that Fabrik orchestrates. When we
          filed an issue to add <code>fabrik watch</code>, Fabrik researched its own log
          format, designed the per-issue monitoring command, and implemented the live log
          streaming and CI check UI — building the observatory it now uses to watch itself
          build features. This page was written by Fabrik too.
          <br><br>
          The human's role is product manager: file issues, answer clarifying
          questions, drag cards, and occasionally comment to redirect the work.
          The factory does the rest.
        </div>
      </div>
    </div>
  </div>
</section>

<!-- ============================================================ -->
<!-- QUICKSTART -->
<!-- ============================================================ -->
<section class="quickstart" id="quickstart">
  <div class="container">
    <p class="section-label">Quickstart</p>
    <h2 class="section-title">From zero to pipeline in minutes</h2>
    <p class="section-subtitle">
      Fabrik runs as a local CLI. You need Claude Code, a GitHub token, and either <code>gh</code> CLI or Go.
    </p>

    <div class="quickstart-steps">
      <div>
        <div class="step">
          <div class="step-num">1</div>
          <div class="step-content">
            <div class="step-title">Prerequisites</div>
            <div class="step-desc">For binary install: <code>gh</code> CLI with <code>gh auth login</code> (repo access required). For source build: Go 1.26.1+. Both paths need Claude Code CLI and a GitHub token with <code>repo</code> and <code>project</code> scopes.</div>
          </div>
        </div>
        <div class="step">
          <div class="step-num">2</div>
          <div class="step-content">
            <div class="step-title">Install &amp; initialize</div>
            <div class="step-desc">Download a pre-built binary via <code>gh</code>, or clone and build from source. Then run <code>fabrik init</code> to scaffold stage configs into your project.</div>
          </div>
        </div>
        <div class="step">
          <div class="step-num">3</div>
          <div class="step-content">
            <div class="step-title">Configure &amp; run</div>
            <div class="step-desc">Edit <code>.fabrik/config.yaml</code> with your project settings, add your token to <code>.env</code>, and run <code>./fabrik</code>.</div>
          </div>
        </div>
        <div class="step">
          <div class="step-num">4</div>
          <div class="step-content">
            <div class="step-title">File an issue, drag a card</div>
            <div class="step-desc">Add an issue to your GitHub Project board. Drag it to the first stage column. Watch the factory run.</div>
          </div>
        </div>
      </div>

      <div class="code-block">
        <div class="code-header">
          <div class="dots">
            <span class="dot red"></span>
            <span class="dot yellow"></span>
            <span class="dot green"></span>
          </div>
          <span>terminal</span>
        </div>
        <pre>
<span style="color:#56d364"># Option A: Install binary (requires gh)</span>
<span style="color:#8b949e"># Requires: gh auth login with access to handarbeit/fabrik</span>
<span style="color:#8b949e"># Extracts to current directory — cd to ~/bin first, or move binary afterwards</span>
cd ~/bin
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
<span style="color:#8b949e"># Platform-specific alternatives:</span>
<span style="color:#8b949e"># darwin/arm64:  --pattern "fabrik_*_darwin_arm64.tar.gz"</span>
<span style="color:#8b949e"># darwin/amd64:  --pattern "fabrik_*_darwin_amd64.tar.gz"</span>
<span style="color:#8b949e"># linux/amd64:   --pattern "fabrik_*_linux_amd64.tar.gz"</span>
<span style="color:#8b949e"># linux/arm64:   --pattern "fabrik_*_linux_arm64.tar.gz"</span>

<span style="color:#56d364"># Option B: Build from source (requires Go)</span>
git clone https://github.com/handarbeit/fabrik
cd fabrik
go build -o fabrik .

<span style="color:#56d364"># 2. Initialize your project (pass your GitHub Project URL to skip manual config)</span>
./fabrik init --user you https://github.com/orgs/your-org/projects/5
<span style="color:#8b949e"># Creates .fabrik/stages/, .fabrik/config.yaml</span>
<span style="color:#8b949e"># URL auto-populates owner, project, and owner_type; --user sets your username</span>
<span style="color:#8b949e"># Without a URL: prompts interactively (TTY) or writes a blank template (non-TTY)</span>

<span style="color:#56d364"># 3. Add your GitHub token</span>
echo 'FABRIK_TOKEN=ghp_...' >> .env
echo '.env' >> .gitignore

<span style="color:#56d364"># 4. Run</span>
./fabrik

<span style="color:#56d364"># Optional: yolo mode (auto-advance all stages)</span>
./fabrik --yolo

<span style="color:#56d364"># Optional: self-upgrade from handarbeit/fabrik GitHub Releases when idle</span>
./fabrik --auto-upgrade</pre>
      </div>
    </div>
  </div>
</section>

<!-- ============================================================ -->
<!-- LINKS -->
<!-- ============================================================ -->
<section id="links">
  <div class="container">
    <p class="section-label">Resources</p>
    <h2 class="section-title">Learn more</h2>

    <div class="links-grid">
      <a href="https://github.com/handarbeit/fabrik" class="link-card" target="_blank" rel="noopener">
        <span class="link-icon">⭐</span>
        <div>
          <div class="link-title">GitHub Repository</div>
          <div class="link-desc">Releases (with announcements in GitHub Discussions), issue tracker, and community skills</div>
        </div>
      </a>
      <a href="{{ '/USER_GUIDE' | relative_url }}" class="link-card">
        <span class="link-icon">📖</span>
        <div>
          <div class="link-title">User Guide</div>
          <div class="link-desc">Full configuration reference and workflow patterns</div>
        </div>
      </a>
      <a href="{{ '/stage-lifecycle' | relative_url }}" class="link-card">
        <span class="link-icon">📄</span>
        <div>
          <div class="link-title">Stage Lifecycle</div>
          <div class="link-desc">Engine internals, markers, context files, and comment processing</div>
        </div>
      </a>
      <a href="{{ '/state-machine' | relative_url }}" class="link-card">
        <span class="link-icon">🔄</span>
        <div>
          <div class="link-title">State Machine</div>
          <div class="link-desc">Authoritative spec for engine state transitions, label semantics, and review gate behavior</div>
        </div>
      </a>
      <a href="{{ '/USER_GUIDE#10-troubleshooting' | relative_url }}" class="link-card">
        <span class="link-icon">🔧</span>
        <div>
          <div class="link-title">Troubleshooting</div>
          <div class="link-desc">Common issues and how to resolve them</div>
        </div>
      </a>
      <a href="https://github.com/handarbeit/fabrik/issues/new" class="link-card" target="_blank" rel="noopener">
        <span class="link-icon">🐛</span>
        <div>
          <div class="link-title">File an Issue</div>
          <div class="link-desc">Bug reports, feature requests, questions</div>
        </div>
      </a>
    </div>
  </div>
</section>
