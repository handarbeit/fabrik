# Feature Specification: Companion tools page for Pruefer and Werkbank, and Pruefer's reference on the docs site

**Feature Branch**: `fabrik/issue-2535`
**Created**: 2026-10-30
**Status**: Specified
**Input**: User description: "## Problem

The docs site (fabrik.handarbeit.io, built from `docs/`) presents Fabrik alone.

- **Werkbank** (https://github.com/handarbeit/werkbank, public) is not mentioned anywhere in this repo. It is handarbeit's tooling for self-hosted GitHub Actions runners: ephemeral Linux containers on a Mac (OrbStack or Docker Desktop) or in WSL2, native macOS and Windows runners, a priority overflow controller, and a host watchdog.
- **Pruefer** is documented only inside `docs/USER_GUIDE.md`, mainly the \"Pruefer review notes\" block under Declaring Expected Reviewers (line ~3044), which links to `cmd/pruefer/README.md` on GitHub. That README (~158 KB) is not on the site. `index.md`, `faq.md`, `core-idea.md` and `docs/llms.txt` don't mention Pruefer. The site navigation (`docs/_layouts/default.html` and `docs/_layouts/docs.html`, each with a desktop and a mobile nav block) has no entry for either tool.

## Requirements

1. **Companion tools page.** Add `docs/companion-tools.md` (published; not in `_config.yml`'s `exclude` list), linked from both layouts' navigation (all four nav blocks) and from the landing page `index.md`. One short entry each:
   - **Pruefer:** self-hosted Claude Code PR review bot (GitHub App daemon). Say what it does, and how to declare it as a reviewer with `expected_reviewers: [<your Pruefer app's handle>]`, linking to the User Guide's Declaring Expected Reviewers section.
   - **Werkbank:** self-hosted Actions runners, and when you'd want them with Fabrik: Fabrik-driven CI (CI gate, CI-fix rounds, merge-train trials) uses many runner minutes on private repos. Repeat Werkbank's own warning: keep public repos on GitHub-hosted runners.
   Each entry links to its repo (Pruefer: `cmd/pruefer/` in this repo).
2. **Pruefer reference on the site.** Add a curated `docs/pruefer.md`: what Pruefer is, install, minimal configuration, the main operator keys, and how it works with Fabrik's review gate. It links to `cmd/pruefer/README.md` for the full reference. Do not copy the whole README, because a copy would drift from it. Point the User Guide's Pruefer links at this page instead of GitHub.
3. **Werkbank in the CI sections.** In the User Guide's CI Gate and CI-Fix Workflow section (it already mentions self-hosted runners, e.g. the cancelled-check note near line ~3141), add one sentence naming Werkbank as one way to run self-hosted runners.
4. **LLM index.** Add Pruefer and Werkbank entries to `docs/llms.txt`. Because `docs/USER_GUIDE.md` changes, regenerate `docs/llms-full.txt` with `bash scripts/generate-llms-full.sh` in the same commit.

## Scope

Docs only, with no engine or Pruefer code changes. Nothing private: anything said about Werkbank must come from its public README and `docs/`. Don't link the marketing site (handarbeit.io); that is tracked separately.

## Acceptance

- fabrik.handarbeit.io has a Companion tools page, reachable from the navigation on desktop and mobile and from the landing page, that describes both tools and links to their repos.
- `docs/pruefer.md` is published, and the User Guide's Pruefer links point to it.
- `docs/llms.txt` lists both tools.
- The `docs-drift` workflow passes.

## Notes

Source: community report #2534. Mention #2534 in the PR body by bare number only. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it. The PR closes only this work issue."

## Background

The Fabrik docs site presents Fabrik on its own. Two handarbeit tools that people commonly run alongside it are invisible there:

- **Pruefer**, the self-hosted Claude Code PR review bot, shipped in this repo under `cmd/pruefer/`. Its only coverage on the site is the "Pruefer review notes" block in the User Guide, under Declaring Expected Reviewers. That block links out to the ~158 KB `cmd/pruefer/README.md` on GitHub, which is not published on the site. The landing page, FAQ, core-idea page and `docs/llms.txt` don't mention it, and the site navigation has no entry for it.
- **Werkbank** (public repo, https://github.com/handarbeit/werkbank), handarbeit's tooling for self-hosted GitHub Actions runners, is mentioned nowhere in this repo. Fabrik-driven CI (the CI gate, CI-fix rounds, merge-train trials) can use many runner minutes on private repos, which is when self-hosted runners become attractive. The User Guide's CI sections already discuss self-hosted runners (for example the cancelled-check note) without naming any tooling for them.

This work came from community report #2534. It makes both tools discoverable from the site, gives Pruefer a curated on-site reference page, and adds both to the LLM index. It is documentation only.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Discover the companion tools (Priority: P1)

A visitor to fabrik.handarbeit.io, on desktop or mobile, finds a Companion tools page from the main navigation or the landing page. It describes Pruefer and Werkbank in one short entry each, with links to their repos.

**Why this priority**: This is the core ask. Without it, neither tool is discoverable from the site.

**Independent Test**: Build the site (or inspect the rendered HTML). Confirm a Companion tools link in all four nav blocks and on the landing page, and that the page lists both tools with working repo links.

**Acceptance Scenarios**:

1. **Given** a visitor on any page using either layout, **When** they open the desktop or mobile navigation, **Then** a link to the Companion tools page is present.
2. **Given** a visitor on the landing page, **When** they look for related tools, **Then** a link to the Companion tools page is present in the page body.
3. **Given** the Companion tools page, **When** they read the Pruefer entry, **Then** it says what Pruefer does, shows how to declare it with `expected_reviewers`, links to the User Guide's Declaring Expected Reviewers section, and links to `cmd/pruefer/` in this repo.
4. **Given** the Companion tools page, **When** they read the Werkbank entry, **Then** it says what Werkbank is, explains when to want it with Fabrik, repeats Werkbank's own warning to keep public repos on GitHub-hosted runners, and links to the Werkbank repo.

---

### User Story 2 - Read a curated Pruefer reference on the site (Priority: P1)

An operator who wants to run Pruefer reads a published Pruefer page covering what it is, how to install it, a minimal configuration, the main operator keys and how it interacts with Fabrik's review gate. For the exhaustive reference it follows a link to the full README. The User Guide's Pruefer links lead to this page rather than GitHub.

**Why this priority**: Pruefer is otherwise documented only by a GitHub-hosted README outside the site.

**Independent Test**: Open the published `pruefer.md` and follow the User Guide's Pruefer link; confirm it lands on that page and that the page links onward to `cmd/pruefer/README.md`.

**Acceptance Scenarios**:

1. **Given** the published Pruefer page, **When** an operator reads it, **Then** they find what Pruefer is, install steps, minimal configuration, the main operator keys, and how it works with Fabrik's review gate.
2. **Given** the Pruefer page, **When** they need more detail, **Then** a link to `cmd/pruefer/README.md` is present.
3. **Given** the User Guide's Pruefer review notes, **When** the reader follows its Pruefer link, **Then** it opens the on-site Pruefer page, not GitHub.

---

### User Story 3 - Find Werkbank from the CI guidance (Priority: P2)

A reader of the User Guide's CI Gate and CI-Fix Workflow section who is considering self-hosted runners sees Werkbank named as one way to run them.

**Why this priority**: A small contextual pointer. It is useful but not essential.

**Independent Test**: Read the CI Gate and CI-Fix Workflow section and confirm the Werkbank sentence with its link.

**Acceptance Scenarios**:

1. **Given** the CI Gate and CI-Fix Workflow section, **When** a reader looks for self-hosted runner guidance, **Then** one sentence names Werkbank as one way to run self-hosted runners and links to it.

---

### User Story 4 - LLM-readable index lists both tools (Priority: P2)

An LLM or tool consuming `docs/llms.txt` finds Pruefer and Werkbank entries, and `docs/llms-full.txt` stays consistent with the changed User Guide.

**Why this priority**: Keeps the machine-readable index complete and the docs-drift check passing.

**Independent Test**: Read `docs/llms.txt`; run `bash scripts/generate-llms-full.sh` and confirm the committed bundle equals its output.

**Acceptance Scenarios**:

1. **Given** `docs/llms.txt`, **When** it is read, **Then** it lists Pruefer and Werkbank each with a one-line description and a link.
2. **Given** the changed User Guide, **When** the bundle is regenerated, **Then** the committed `docs/llms-full.txt` matches and the `docs-drift` workflow passes.

---

### Edge Cases

- `docs/companion-tools.md` and `docs/pruefer.md` must not be caught by the `_config.yml` `exclude` list, so both must actually be published.
- The nav change must reach all four blocks: desktop and mobile in `default.html`, and desktop and mobile in `docs.html`. Missing one leaves the link absent on some pages.
- Werkbank facts (what it does, the public-repo warning) must come only from its public README and docs. Anything not stated there must not be asserted.
- The `expected_reviewers` handle is the operator's own Pruefer app handle, as a bare handle with no `@` and no `[bot]` suffix, per the User Guide's identity format rules. The page must not present `handarbeit-pruefer` as a handle everyone should use.
- The Pruefer page is curated and must not duplicate large parts of the README, so it can't drift from it. Any key described there must match the README's current meaning.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A published page `docs/companion-tools.md` exists, is not excluded by `_config.yml`, and has one short entry each for Pruefer and Werkbank.
- **FR-002**: The Companion tools page is linked from all four navigation blocks (desktop and mobile, in both `_layouts/default.html` and `_layouts/docs.html`) and from the landing page `index.md`.
- **FR-003**: The Pruefer entry says Pruefer is a self-hosted Claude Code PR review bot run as a GitHub App daemon, and shows declaring it with `expected_reviewers: [<your Pruefer app's handle>]`. It links to the User Guide's Declaring Expected Reviewers section and to `cmd/pruefer/` in this repo.
- **FR-004**: The Werkbank entry describes it as tooling for self-hosted GitHub Actions runners. It explains that Fabrik-driven CI (CI gate, CI-fix rounds, merge-train trials) uses many runner minutes on private repos, which is when self-hosted runners help. It repeats Werkbank's own warning to keep public repos on GitHub-hosted runners, and links to https://github.com/handarbeit/werkbank.
- **FR-005**: A published, curated page `docs/pruefer.md` covers what Pruefer is, install, minimal configuration, the main operator keys, and how it works with Fabrik's review gate. It links to `cmd/pruefer/README.md` for the full reference and does not reproduce the README wholesale.
- **FR-006**: The User Guide's Pruefer link or links that currently point to `cmd/pruefer/README.md` on GitHub (the Pruefer review notes block) point to the on-site Pruefer page instead. Links in the new Pruefer page that point to the full README remain.
- **FR-007**: The User Guide's CI Gate and CI-Fix Workflow section gains one sentence naming Werkbank as one way to run self-hosted runners, with a link to its repo.
- **FR-008**: `docs/llms.txt` gains entries for Pruefer and Werkbank, consistent with the existing entry style.
- **FR-009**: `docs/llms-full.txt` is regenerated with `bash scripts/generate-llms-full.sh` and committed together with the User Guide change, so the `docs-drift` workflow passes.
- **FR-010**: All Werkbank statements are sourced from Werkbank's public README and docs. No private information is included, and the marketing site (handarbeit.io) is not linked.
- **FR-011**: The change touches documentation, site layouts and the LLM index only. It makes no engine or Pruefer code changes.

### Key Entities *(if applicable)*

- **Companion tools page**: a short, published page introducing Pruefer and Werkbank as optional tools used alongside Fabrik.
- **Pruefer reference page**: a curated on-site overview of Pruefer that defers to `cmd/pruefer/README.md` for the full reference.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A Companion tools page is reachable by a single link from every site page's navigation (desktop and mobile) and from the landing page.
- **SC-002**: Both tools are described on the Companion tools page, each with a working link to its repo.
- **SC-003**: Zero User Guide links remain that send a reader to `cmd/pruefer/README.md` on GitHub as the primary Pruefer reference; they go to the on-site Pruefer page, which links onward to the full README.
- **SC-004**: `docs/llms.txt` lists both tools.
- **SC-005**: The `docs-drift` workflow passes.

## Assumptions

- Page layout, front matter and nav label wording follow the conventions of the existing pages (for example `faq.md` and `core-idea.md`). The nav label is "Companion Tools", placed next to the existing items.
- The Pruefer page's "main operator keys" are a short curated set taken from the current `cmd/pruefer/README.md`, with the README as the authority.
- The Companion tools page links the Pruefer entry to the on-site Pruefer page as well as to `cmd/pruefer/` in the repo. The issue requires only the repo link, and the extra on-site link is low-impact and reversible.
- The anchor for the User Guide's Declaring Expected Reviewers section is whatever the rendered User Guide produces for that heading.

## Out of Scope *(optional)*

- Any engine or Pruefer code changes.
- Copying the full Pruefer README onto the site.
- Linking the handarbeit.io marketing site, which is tracked separately.
- Documenting Werkbank beyond a short entry and the one-sentence pointer in the CI section.
- Changes to `index.md`, `faq.md` or `core-idea.md` beyond the landing-page link to the Companion tools page.

## Source References *(optional)*

- Community report #2534 (mentioned for context only; no closing keyword)
- https://github.com/handarbeit/werkbank (public README and docs)
- `cmd/pruefer/README.md`
- `docs/USER_GUIDE.md` (Declaring Expected Reviewers; CI Gate and CI-Fix Workflow)
