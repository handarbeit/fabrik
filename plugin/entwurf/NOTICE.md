# Notices

The `write-spec` and `clarify` skills in this plugin are adapted from
[GitHub Spec Kit](https://github.com/github/spec-kit), specifically
`templates/commands/specify.md`, `templates/commands/clarify.md`, and
`templates/spec-template.md`.

Spec Kit is MIT licensed:

```
MIT License

Copyright GitHub, Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## What was changed

Spec Kit's commands assume a Claude Code / CLI environment and cover the whole
lifecycle from idea to implementation.

This plugin began as spec authoring only, for a product manager or business owner rather
than an engineer. It has since grown two further expert roles — a technical architect and
an experience lead — which are **not** adapted from Spec Kit, because Spec Kit has no
equivalent. They are original work, built to the same shape: a guided interview producing
a project-level baseline plus per-feature artifacts, each gated by a fixed checklist.
Everything past the specification is still out of scope: no `plan`, `tasks`, `analyze`
or `implement`.

**Kept:** the spec template and section structure, sequential `specs/NNN-name/`
numbering, `.specify/feature.json`, `.specify/memory/constitution.md`, the
requirements quality checklist (all sixteen items verbatim, in upstream's order)
and its re-validation pass, the three-marker `[NEEDS CLARIFICATION]` cap **for feature
documents** (project baselines are uncapped and counted instead), and clarify's full
coverage taxonomy with its five-question ceiling and incremental
write-back.

**Removed:** the `plan`, `tasks`, `analyze`, `implement`, and `checklist`
commands and everything that supports them; the `.specify/extensions.yml` hook
system; git branch creation; the `check-prerequisites` shell/PowerShell/Python
scripts; and the preset/template resolution stack.

**Renamed:** the `specify` command became **write-spec**. Fabrik's own pipeline
ships a separate, unrelated `fabrik-specify` skill that operates on an already-
filed GitHub issue; the two never load into the same session, but the distinct
name keeps "the skill a human uses to author a spec" from reading as a variant
of it. The output format is unchanged from upstream `specify`; the behavioural
differences are listed below.

**Adapted for a non-technical author.** Beyond the audience rewrite, these
behavioural changes were made deliberately and depart from upstream:

- **Questions take a position.** Upstream `specify` presents a neutral A/B/C
  menu; both skills here lead with a recommendation, a one-line "why it
  matters", and an explicit "say yes to accept" — a neutral menu handed to
  someone who came here precisely because they don't know the options just
  returns the decision to them. "You pick" is handled explicitly, and the
  result is recorded as an assumption rather than as the person's decision.
- **No invented facts.** Neither skill may propose a specific number, name, or
  threshold the person hasn't implied. Where upstream would fill a measurable
  success criterion with a plausible figure, this writes the shape and marks
  the figure `[NEEDS MEASUREMENT]`. That marker, and `[NEEDS DECISION]`,
  `[NEEDS LOOKUP]` and `[NEEDS DESIGN]`, are local to this plugin and do not count
  against upstream's three-marker cap. `[NEEDS BASELINE]` is a legacy spelling of
  `[NEEDS MEASUREMENT]`, kept as an alias so markers already written into existing specs
  are never rewritten.
- **No temporary-directory fallback.** Upstream assumes a repo checkout. These
  skills write only to a folder the person has connected, and hand the files
  back for download when there isn't one, rather than saving to a scratch
  directory that disappears when a cloud session ends.
- **Assumptions are surfaced in conversation**, not only written to the file —
  the correctability that justifies guessing doesn't work if it lives only in a
  document nobody opens.
- **The physical environment of use** is never silently defaulted.
- **The checklist `## Notes` section stays a single bullet**, as upstream has
  it. The repair log lives in the reply instead, because downstream tooling
  reads that file as a gate rather than a scratchpad. Two things inside the
  checklist file differ from upstream deliberately, both outside the sixteen
  graded items: the `**Feature**` header links to the spec rather than carrying
  upstream's bare placeholder, and the `## Notes` bullet says "before
  clarification or planning" where upstream names its `/speckit.*` slash
  commands — a Cowork reader has no way to run those.
- **No plan-stage handoffs in user-facing text.** Upstream closes by pointing at
  `/speckit.plan`; these skills never tell the reader to run a command, skill,
  or stage, since a Cowork user has no way to start one.

**Changed:** the `constitution` command became **onboard**, a guided interview
rather than a principles-authoring command. Its output keeps Spec Kit's
constitution shape and location, but the content is narrowed to what a
non-technical author can legitimately own and what actually constrains a
specification — project goals, users, scope boundaries, hard constraints,
domain glossary, and product principles. Engineering governance (testing
strategy, delivery process, architecture, tech stack) is deliberately excluded
and deferred to the build stage; technical preferences raised during the
interview are recorded in a clearly non-binding section instead.

## What was added to upstream's namespace

Spec Kit defines `.specify/` and `specs/NNN-name/`. This plugin writes seven paths that
upstream does **not** define, six of them inside `.specify/`:

```
.specify/memory/architecture.md                          technical baseline
.specify/memory/experience.md                            experience baseline
.specify/memory/decisions.md                             decision register
.specify/memory/checklists/architecture-foundations.md   stage 3 gate
.specify/memory/checklists/architecture-baseline.md      stage 5 gate
.specify/memory/checklists/experience.md                 stage 4 gate
adrs/NNN-short-title.md                                  decision records
```

These names are this plugin's own. Upstream assigns them no meaning and could later claim
any of them — if it does, these are the files that would need renaming, and nothing else
here depends on their names.

They are **additive**. No upstream file is modified: `spec.md`,
`checklists/requirements.md` and `.specify/feature.json` are written exactly as upstream
defines them, and `.specify/memory/constitution.md` keeps upstream's shape and location
with only its content narrowed (see **Changed** below). A Spec Kit project that has these
files added to it loses nothing and stays readable by upstream tooling.

Two further per-feature paths — `specs/NNN-*/technical.md` and `specs/NNN-*/experience.md`
— are reserved in the plugin's conventions but have no producer yet.

Spec output remains compatible with upstream Spec Kit, so anything that
consumes a Spec Kit `specs/NNN-name/` folder — including `/speckit.plan` and
`/speckit.tasks` in Claude Code — works against these specs unchanged. The additions
above sit alongside that folder rather than inside it.
