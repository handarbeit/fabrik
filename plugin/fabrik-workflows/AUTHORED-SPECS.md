# Working on a project whose specifications were authored first

Some projects arrive with their specifications already written — by a product owner, an
architect and an experience lead, before any issue existed. They come in
[GitHub Spec Kit](https://github.com/github/spec-kit) layout: a `.specify/memory/` directory of
project-level baselines, and a `specs/NNN-feature-name/` tree. Several tools produce this shape,
and it does not matter which one did. What matters is that it is a body of work you are meant to
consume rather than recreate.

This file describes what to expect and how to read it. Skills reference it as
`../../AUTHORED-SPECS.md`.

## Two independent questions, not one

It is tempting to fold this into a single project-wide switch — does
`.specify/memory/constitution.md` exist? — but that answers only one of two questions, and
conflating them breaks the common case: a specified project still raises issues (spikes,
migrations, tooling, infrastructure) that never had a specification written for them, and those
need Specify to author normally, constitution or no constitution.

**Question 1 — per issue: does this issue reference an authored specification, or does it contain
its own?** Look for an explicit pointer to a document the issue names as its source — most
commonly `specs/NNN-*/spec.md`, but any authored, ratified document the issue projects from counts
(a spike brief, an architecture memo). **Verify the reference before trusting it**: confirm the
named document actually exists and reads as a specification. An issue's own claim that "no spec
file is needed" or that it is "covered by the constitution" is not itself proof — check, don't take
its word. No verified reference → this issue authors normally, exactly as the rest of this
pipeline does, regardless of whether the project has a constitution at all.

**Question 2 — per project: what do you check consistency against?** `.specify/memory/constitution.md`
present → check against `architecture.md`, `decisions.md`, and the `specs/` corpus, on top of the
usual `CLAUDE.md`/README. Absent → `CLAUDE.md`, README, and existing configs, as normal. This
question is decided once per project and never varies by issue; Question 1 is decided fresh for
every issue and can go either way even inside a project that answers Question 2 "yes."

**The consistency check always runs**, whichever way Question 1 went. Where you authored the spec
yourself, run it against **your own rewritten output**, not the raw issue — a rewrite can introduce
a contradiction the issue didn't have, and it is the rewritten body that goes downstream. The check
is also for what the issue is silent about, not only what it asserts.

## Two directions the `specs/` tree itself can take, and why they don't collide

**Authored-first content.** People wrote specifications before any issue existed. Numbered by
feature (`specs/001-…`, `specs/002-…`). An issue whose Question 1 finds a verified reference reads
one of these and never writes it.

**Fabrik-first content.** No verified reference on this issue — Specify authors the issue body and
projects it. Numbered by issue: `specs/<issue-number>-<slug>/` in a project with no
`.specify/memory/constitution.md`, or `specs/fabrik-<issue-number>-<slug>/` in one that has it —
the prefix exists purely so an issue-numbered projection can never collide with a feature-numbered
authored entry sharing the same low number (`specs/012-…` next to a same-numbered `specs/012-slug/`
projection is a real risk otherwise, and luck is not a naming scheme). Same content and rules
either way — see "Commit the spec file" in `fabrik-specify`.

A repository that somehow has an authored `specs/NNN-*/spec.md` at the *same* path a projection
would use has a naming problem worth stopping for rather than guessing through; the `fabrik-`
prefix in a constitution-carrying project is what makes this structurally impossible for new
projections going forward.

## How to tell

**Project-level markers** (Question 2 — decides what to check against):

```
PROJECT.md                       a visible index, if present
.specify/memory/constitution.md  the product's principles and vocabulary
.specify/memory/architecture.md  the technical baseline
.specify/memory/experience.md    the experience baseline
.specify/memory/decisions.md     the decision register
specs/NNN-feature-name/spec.md   the specifications
```

If none of this exists, ignore this file entirely — plain Fabrik-first, no constitution to check
against, and every issue authors as normal.

**Per-issue reference** (Question 1 — decides whether to author): does the issue name a document
it projects from? Most often `specs/NNN-*/spec.md`, but treat any document the issue explicitly
cites as its source the same way. Confirm it exists and reads as a specification before trusting
it — do not accept the issue's own assertion in place of checking.

## The rule that matters most

**Never rewrite a verified, referenced specification.**

Where Question 1 finds one, the issue **references** it rather than containing it, and Specify
becomes a *reading and consistency* stage for this issue rather than an authoring one. Rewriting
the issue body with a generated spec destroys work that three people were interviewed to produce.

The consistency half of Specify's job is the valuable half here, and it gets better inputs when
Question 2 answers "yes": read `architecture.md` and `decisions.md`, not only `CLAUDE.md`.

## Precedence — what wins when two documents disagree

You will have the specification, the baseline and the register in front of you at once. They can
disagree, and the disagreement is usually a decision the specifications have not absorbed yet
rather than a mistake.

> **The specification is what you build. The architecture baseline constrains how.** Where they
> disagree, the specification wins and you **flag the disagreement rather than resolving it** — a
> baseline decision the specification has not absorbed is not yet in force.
>
> Anything in the decision register whose **Class** is not `Decided` is **not a commitment**.
> `Working assumption`, `Recommended and accepted`, `Technical suggestion` and `Exploratory` rows
> are context; treat them as things to ask about, not to build against.
>
> A baseline marked **`Status: Draft`** is advisory. You may deviate — and you must say where and
> why. Most projects sit at Draft for their whole specification phase, so this is the normal case
> rather than the exception.

The baseline's `## Spec amendments owed` section, if present, lists exactly the decisions that
have not landed in the specifications yet. It is the fastest way to see what will look
contradictory.

## Non-negotiables are gates, not advice

The baseline's Constraints section records non-negotiables, each with **how a violation would be
detected**. Those are Review and Validate gates. Read them as such — they are the project's own
statement of what must never be true, written by the person who will be angriest if it becomes
true.

The same applies to the baseline's fitness functions, where stage 5 has run.

## Blocking on an ambiguity

When you block, **name who you are blocked on**. The baseline groups its open questions as
addressed to the product owner, the architect, or the experience lead, and that grouping is what
makes them actionable. A block that names its audience lands in the right lane; one that does not
becomes something a human has to triage first.

And a block is evidence about the corpus, not only about your issue. An ambiguity you hit is a
signal the specifications have drifted, and it is a trigger for the architect to re-run their
foundations stage.

## Writing decisions down

The project's decision register and this pipeline's ADRs do different jobs at different altitudes.

- **An ADR is the reasoning.** Write one as normal, and declare its level:
  `**Level**: Context | Container | Component | Code | Domain`. Most of what this pipeline
  produces is Component or Code.
- **A register row is what is currently true.** Append one to `.specify/memory/decisions.md`
  **only** when your decision changes something at the domain or product altitude — an entity's
  owner, an authority, a product rule. That will be rare. Give it an honest `Class`: a judgement
  call you made while building is not the same as a ruling the product owner gave, and the column
  exists to keep those visibly apart.

Do not push implementation decisions into the register. Two hundred rows about module structure
would drown the eighty about what is true in the domain.
