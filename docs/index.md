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
    <div class="hero-eyebrow">🏭 Open Source CLI</div>
    <h1>Your SDLC,<br>on <span class="accent">autopilot</span></h1>
    <p class="hero-tagline">
      Fabrik watches your GitHub Project board and drives Claude Code through
      a full software development pipeline — Research, Plan, Implement, Review,
      Validate — automatically. File an issue. Drag a card. Ship.
    </p>
    <div class="hero-actions">
      <a href="https://github.com/tenaciousvc/fabrik" class="btn btn-primary" target="_blank" rel="noopener">
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
        {% include video-embed.html
           title="The Fabrik TUI"
           desc="Watch Fabrik's terminal UI manage active pipeline jobs, track costs, and show real-time progress across issues." %}
        <div class="video-caption">
          <div class="caption-title">The Fabrik TUI Control Panel</div>
          <div class="caption-desc">Active jobs, history, cost tracking — live in your terminal</div>
        </div>
      </div>
      <div class="video-container">
        {% include video-embed.html
           title="GitHub Project Board"
           desc="See issues move automatically through columns as Fabrik advances them through the pipeline. Comment to steer; drag to control." %}
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
          Each issue gets its own worktree at <code>.fabrik/worktrees/issue-N/</code>
          on branch <code>fabrik/issue-N</code>. Multiple issues run in parallel,
          zero cross-contamination.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">💬</span>
        <div class="feature-title">Comment-Driven Steering</div>
        <div class="feature-desc">
          Comment on an issue mid-stage to redirect the work.
          Fabrik reacts with 👀, processes your comment, and continues.
          The full conversation history is always available to Claude.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">⚡</span>
        <div class="feature-title">Yolo Mode</div>
        <div class="feature-desc">
          Enable <code>--yolo</code> and Fabrik auto-advances issues through
          every stage without human approval. Great for low-risk work or
          trusted pipelines.
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
          Each instance only processes its own user's issues.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🔄</span>
        <div class="feature-title">Self-Upgrade</div>
        <div class="feature-desc">
          Pass <code>--auto-upgrade</code> and Fabrik watches <code>origin/main</code>.
          When idle and new commits appear, it rebuilds itself and re-execs —
          no manual deploys.
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
          A bubbletea-powered TUI shows active jobs, stage progress, token
          costs, and history — everything you need to supervise the factory
          at a glance.
        </div>
      </div>
      <div class="feature-card">
        <span class="feature-icon">🗂️</span>
        <div class="feature-title">Multi-Repo Support</div>
        <div class="feature-desc">
          Run Fabrik from outside any git repo to manage issues across multiple
          repositories from a single GitHub Project board. Each repo is cloned
          lazily; worktrees are created per repo and per issue automatically.
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
          filed an issue to add PR comment processing, Fabrik researched its own
          codebase, planned the GraphQL changes, and implemented the feature that
          lets it read PR comments — gaining a capability it needed by building it
          for itself. This page was written by Fabrik too.
          <br><br>
          The human's role is product manager: file issues, answer clarifying
          questions, drag cards, and occasionally comment "please process PR feedback."
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
<span style="color:#8b949e"># Requires: gh auth login with access to tenaciousvc/fabrik</span>
<span style="color:#8b949e"># Extracts to current directory — cd to ~/bin first, or move binary afterwards</span>
cd ~/bin
gh release download --repo tenaciousvc/fabrik --pattern '*.tar.gz' -O - | tar xz

<span style="color:#56d364"># Option B: Build from source (requires Go)</span>
git clone https://github.com/tenaciousvc/fabrik
cd fabrik
go build -o fabrik .

<span style="color:#56d364"># 2. Initialize your project</span>
./fabrik init
<span style="color:#8b949e"># Creates .fabrik/stages/, .fabrik/config.yaml</span>
<span style="color:#8b949e"># Prompts for owner/repo/project/user</span>

<span style="color:#56d364"># 3. Add your GitHub token</span>
echo 'FABRIK_TOKEN=ghp_...' >> .env
echo '.env' >> .gitignore

<span style="color:#56d364"># 4. Run</span>
./fabrik

<span style="color:#56d364"># Optional: yolo mode (auto-advance all stages)</span>
./fabrik --yolo

<span style="color:#56d364"># Optional: self-upgrade from origin/main when idle</span>
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
      <a href="https://github.com/tenaciousvc/fabrik" class="link-card" target="_blank" rel="noopener">
        <span class="link-icon">⭐</span>
        <div>
          <div class="link-title">GitHub Repository</div>
          <div class="link-desc">Source code, issues, and releases</div>
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
      <a href="https://github.com/tenaciousvc/fabrik/issues/new" class="link-card" target="_blank" rel="noopener">
        <span class="link-icon">🐛</span>
        <div>
          <div class="link-title">File an Issue</div>
          <div class="link-desc">Bug reports, feature requests, questions</div>
        </div>
      </a>
    </div>
  </div>
</section>
