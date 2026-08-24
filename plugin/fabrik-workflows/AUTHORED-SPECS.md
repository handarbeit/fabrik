# Working on a project whose specifications were authored first

Some projects arrive with their specifications already written — by a product owner, an
architect and an experience lead, before any issue existed. They come in
[GitHub Spec Kit](https://github.com/github/spec-kit) layout: a `.specify/memory/` directory of
project-level baselines, and a `specs/NNN-feature-name/` tree. Several tools produce this shape,
and it does not matter which one did. What matters is that it is a body of work you are meant to
consume rather than recreate.

This file describes what to expect and how to read it. Skills reference it as
`../../AUTHORED-SPECS.md`.

## Two directions, and you must know which one you are in

A `specs/` tree can arrive two ways, and they are opposites. Getting this wrong destroys work.

**Authored-first — the specification file is canonical.** People wrote the specifications
before any issue existed. `.specify/memory/` is present. The specifications are authored,
numbered by feature (`specs/001-…`, `specs/002-…`), and the issue **references** one rather than
containing it.

**Fabrik-first — the issue body is canonical.** No `.specify/memory/`. The issue body is the
spec, Specify authors it, and the file is its **committed projection**, numbered by issue
(`specs/<issue-number>-<slug>/`).

**The test:** does `.specify/memory/constitution.md` exist? If yes, authored-first. If no,
Fabrik-first.

The two never coexist in one repository — they number the same directory differently, and each
assumes the other's artifact is derived. A repository that somehow has both has a problem worth
stopping for rather than guessing through.

## How to tell

```
PROJECT.md                       a visible index, if present
.specify/memory/constitution.md  the product's principles and vocabulary
.specify/memory/architecture.md  the technical baseline
.specify/memory/experience.md    the experience baseline
.specify/memory/decisions.md     the decision register
specs/NNN-feature-name/spec.md   the specifications
```

If `specs/NNN-*/spec.md` exists, **the specification is already authored**. If none of this
exists, ignore this file — the issue body is the spec, as normal.

## The rule that matters most

**Never rewrite an authored specification.**

Where a `specs/NNN-*/spec.md` exists, the issue **references** it rather than containing it, and
Specify becomes a *reading and consistency* stage rather than an authoring one. Rewriting the
issue body with a generated spec destroys work that three people were interviewed to produce.

The consistency half of Specify's job is the valuable half here, and it gets better inputs: read
`architecture.md` and `decisions.md`, not only `CLAUDE.md`.

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
