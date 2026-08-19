# Entwurf

Turn a project into buildable specifications, one expert role at a time — the product owner, the technical architect, and the experience lead. Each is interviewed about their own side of the product; nobody is asked to answer for a role they don't hold.

The product-owner path needs no terminal, no git, and no technical background.

A **specification** — a spec — is a written description of what a piece of software should do and why, in enough detail that someone can build it without guessing. This plugin writes them with you, by asking questions.

### The product owner's skills

No technical background needed. Start here — everything else reads what these produce.

- **onboard** — a 30–45 minute interview about the project as a whole: the problem, who it serves, what's in and out of scope, the hard constraints, and the words the business uses. Writes a short document — the project *constitution* — recording the ground rules every later spec has to respect, so you only answer these questions once.
- **write-spec** — turns a feature described in plain language into a structured spec: the things people need to do with it (*user stories*), what it must do (*requirements*), how you'd know it worked (*success criteria*), and a checklist that grades the result.
- **clarify** — reads a spec, finds what's vague or missing, asks up to five targeted questions one at a time, and writes each answer back into the document.

### The architect's and designer's skills

For the two expert roles, run after the specifications exist. These assume the person being interviewed *is* the expert — they don't simplify, and they don't ask questions someone outside the discipline could answer.

- **architecture-foundations** — the immovable facts, plus the shared data model and who owns each part of it, the contracts between features, and who is allowed to do what. Runs early and is re-run as specifications accumulate, because that shared model is what stops eleven features drifting apart.
- **experience-baseline** — conditions of use, the states every screen must handle, the component vocabulary, the words, the accessibility floor, and the interaction budgets. Decided once for the whole product.
- **architecture-baseline** — converts what the product promises users into what the system guarantees developers: the domain model, contracts, budgets, build order, and the fitness functions that make it checkable rather than advisory.

They run in that order for a reason: what the user must experience determines what the system must guarantee, which determines how it is built. Reverse it and the architect ends up inventing the guarantees.

The files it writes follow a widely-used open format called [Spec Kit](https://github.com/github/spec-kit), so any development team can pick them up without translation — as can Fabrik, the tool that builds software from specs like these automatically.

## Install

In the Claude desktop app: **Cowork** → **Customize** → **Plugins** → **Add marketplace**, enter `handarbeit/fabrik`, then install **Entwurf**. Updates appear as an **Update** button in the same place.

You'll need the desktop app on a computer. The phone app can't connect to a folder, and these skills need somewhere to save your specs.

**[GETTING-STARTED.md](GETTING-STARTED.md) is the full walkthrough**, including creating a Cowork project so your folder stays connected between sessions. Start there if this is your first time.

## Using it

Connect the folder where specs should live — in practice, by making a Cowork project with that folder attached, so you don't reconnect it every session. Any folder works; Documents is fine. If someone's already building the software, ask which folder they'd like you to use; a folder that syncs automatically to them, like a shared Dropbox, Google Drive, or OneDrive folder, saves you emailing anything to anyone. If it's just you for now, make a new folder anywhere and share it later.

**Start the project.** "Let's set up the project" or "onboard me". You'll be interviewed about goals, users, scope, constraints, and vocabulary, then shown a short constitution to check. This happens once.

**Write a spec.** Describe a feature in your own words, with as much or as little detail as you have. You'll get a spec plus a quality checklist, and at most three questions about things that genuinely couldn't be guessed. Everything else gets a sensible default, recorded under Assumptions where you can correct it.

**Sharpen it.** Ask to clarify the spec. Up to five questions, one at a time, each with a recommended answer you can accept by saying "yes". Answers are written straight into the spec — the decision lives in the document, not the chat.

Repeat the last two per feature. The constitution keeps them consistent.

## What you're asked for, and what you aren't

**You're asked for:** what the software should do, who it's for, why it matters, and how anyone would know it's working. That's the whole job, and you're the person who knows it.

**You aren't asked for:** how it gets built. No architecture, databases, or frameworks — that's deferred to the build stage. If you have a technical opinion, say it anyway; it gets recorded for the build team rather than thrown away, and it won't distort the spec.

Two things are worth knowing, because they're where specs usually go wrong:

**Say what "good" looks like in numbers.** "The system should be fast" can't be checked by anyone. "A manager can log an entry in under 30 seconds" can. Every vague adjective — robust, intuitive, seamless — is a decision someone downstream ends up making on your behalf.

**Rank the features honestly.** Each user story is written so it could ship on its own. P1 means "must be in the first version"; P2 and P3 can come later. If everything is P1, nothing is, and build order gets chosen by someone with less context than you.

## What it produces

```
.specify/memory/constitution.md      project goals, scope, constraints, glossary
specs/001-feature-name/
├── spec.md                          the specification
└── checklists/requirements.md       quality checks, with pass/fail state
```

`.md` files are plain text documents — they open in anything, including Notepad or TextEdit. The folder starting with a dot (`.specify`) is hidden by your computer by default; that's normal, and Claude can open it for you any time.

Not sure where to start? Install the plugin, connect a folder, and say "let's set up the project".

## For developers

Install into Claude Code instead:

```
/plugin marketplace add handarbeit/fabrik
/plugin install entwurf@fabrik
```

`NOTICE.md`, alongside this file in the plugin source, records what was adapted from upstream Spec Kit and what was deliberately left out.
