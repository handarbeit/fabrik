---
name: architecture-baseline
description: Interviews the technical architect to complete a project's architecture baseline — stack and boundaries, the shared domain model, interface contracts, cross-cutting policies, non-functional budgets, build order, and the fitness functions that make the whole thing checkable. Converts user-facing guarantees from the experience baseline into system guarantees. Use this after the product specifications, technical constraints and experience baseline all exist, and before any code is written. Trigger it when someone wants to finalize the architecture, decide the technical approach for a whole project, set the patterns every feature must follow, or work out what the system must guarantee before a build starts.
---

# Architecture Baseline

Turn what the product promises its users into what the system guarantees, and write it
down so every feature built afterwards is built the same way.

**Read `../../CONVENTIONS.md` first.** It carries the shared rules — stages, where work
is saved, the tree, markers, interview rules, precedence, gates, decision records and the
baseline status footer. This skill does not repeat them.

This is **stage 5 of 5**, and the last before building starts — though the revision beat
described in `../../CONVENTIONS.md` may bring any of these documents back.

## What this prevents

The constitution stops eleven specs using eleven vocabularies. This stops eleven
implementations inventing eleven patterns — eleven models of the same entity, eleven
error conventions, eleven answers to what happens when the network dies.

That is the test for every section: **name the divergence it prevents.** A section that
cannot is decoration and should be cut.

## What your job is

Not to design features. **To make every decision that eleven parallel implementations
would otherwise each make separately — once, here, in a form that can be checked.**

Stage 3 deliberately deferred all design to you. Sections 1–3 below are unambiguously
design work and are yours to do.

## Before starting

Establish the project root as `../../CONVENTIONS.md` describes. Read, in this order:

1. `.specify/memory/architecture.md` — the constraints from stage 3, and the **Deferred
   to the baseline** list, which is your agenda
2. `.specify/memory/experience.md` — the **conditions of use** and **accessibility
   floor** (which drive more of the architecture than anything else in it), the surface
   classes, status dimensions, every `[NEEDS DESIGN]` marker — each is a system-side
   question you either answer here or record as an open risk — state inventory, composite state, interaction budgets, and
   any **Open questions** addressed to the architect
3. `.specify/memory/decisions.md` — so you do not re-decide something settled, and so you
   can check the **class** of anything described to you as agreed
4. `.specify/memory/constitution.md` — and compare its version to the
   `**Against constitution**` field in the constraints. If it has moved, say so; something
   recorded as fixed may no longer be
5. every `specs/NNN-*/spec.md`
6. **any existing technical material elsewhere in the project** — see "Look for what
   already exists" in `../../CONVENTIONS.md`. If one exists, settle explicitly whether
   this baseline supersedes it or defers to it, and record that. Leaving two architecture
   documents at two paths with no supersession rule is worse than either alone.

**If the experience baseline does not exist, stop and say so.** This skill converts
user-facing guarantees into system guarantees; without them you would invent the
guarantees and then design to your own invention. Offer to run `experience-baseline`
first. Proceed only if the architect explicitly accepts that every user-facing number in
the result is a guess.

**If the constraints from stage 3 do not exist**, say so and offer to run
`architecture-foundations` first. If the architect declines, write the Constraints
sections as empty with a note that no constraints pass was run, and skip the
gate-preservation instruction below.

## Who you are talking to

A practising architect. **The product-owner rule inverts** — see the expert-interview
section in `../../CONVENTIONS.md`. Do not explain patterns or offer menus of options they
know better than you.

## The core move

Take each guarantee the experience baseline makes to a user and convert it into a
guarantee the system makes to a developer.

> "A manager can capture with no signal and lose nothing"
> → local-first storage, a durable outbound queue, a **decided** conflict rule for two
> edits of one record, and a fitness function that fails if any capture path can drop data.

Convert the guarantee; do not quietly bound it. If the specs promise capture survives
"regardless of how much time has passed", introducing a maximum offline duration is not a
conversion — it is a new limit nobody agreed to, contradicting a ratified success
criterion. Where you believe a promise is unbuildable as written, that is the feedback
loop below, not a number you choose.

Work through the experience baseline's states and budgets in order. Anything that cannot
be converted is a promise the product cannot keep — see the feedback loop.

## The interview

Seven sections. Lead each with a proposal. **Record under each the divergence it
prevents** — one line is enough, and the gate checks for it.

### 1. Do the foundations still hold?

The shared domain model, interface contracts and authority model were written at stage 3,
possibly before half the specifications existed. **You are not re-authoring them — you are
checking they survived.**

Read them against the full spec set and the experience baseline, and ask:

- Does every entity in the final specs appear in the model, with an owner?
- Did any feature added since stage 3 change who owns a field?
- Does the authority model cover what the experience baseline assumed, including a
  disclosure class for every authority type?
- Does every state that triggers another feature still exist, and has any new spec
  introduced a handoff no contract covers?
- Do the **shared state vocabularies** still hold — has any new spec added a dimension, or
  started using an existing word in a second sense?

Amend stage 3's sections in place where they are wrong, and record each amendment in the
decision register with what it supersedes. If the answer is "substantially not", that is
worth saying out loud: it means stage 3 was run too late or not re-run often enough, and
that is a process finding rather than a document one.

### 2. Stack and boundaries

What is decided, and what is deliberately left open so a developer knows where they may
choose freely. Revisit anything stage 3 recorded as a reversible commitment; the
experience work may have made the case to change it.

### 3. Cross-cutting mechanisms

How the authority model from stage 3 is actually enforced, plus error handling, audit and
logging, time and identity, retention. Stage 3 decided *who may do what*; this decides
*how that is enforced and what happens when it fails*. Each will be invented per-feature if
not decided here, and each will be invented differently.

### 4. Non-functional budgets

Latency, payload sizes, offline duration, sync windows, retention, concurrency.

Start from the experience baseline's interaction budgets — but **check each against its
class in the register**. Only `Decided` rows are agreed; a `Working assumption` is a
proposal you may overturn, and conforming a system budget to an unconfirmed guess is how a
guess becomes a commitment. A system budget contradicting a *decided* interaction budget is
a bug in this document.

Carry every figure with its hedge and its source, per `../../CONVENTIONS.md`. A retention
period the constitution stated as "roughly seven years, pending legal review" does not
become `7 years` here; promoting a hedged number into a binding one is inventing a figure.

Carry forward `[NEEDS MEASUREMENT]` and `[NEEDS BASELINE]` markers unchanged — they are
the same marker under two spellings. But check each first: if you could settle it today,
it was mismarked and is a `[NEEDS DECISION]`, which blocks ratification.

### 5. Build order

The dependency-traced sequence: what must exist before what, what can proceed in parallel.
Ask what the smallest thing is that proves the risky parts work end to end, and put it
first.

### 6. Fitness functions

**This is what makes the baseline binding rather than advisory.**

Work from two inputs, not one: **the constitution's principles** — most are already phrased
as invariants, and a NON-NEGOTIABLE with no check is exactly the gap this section closes —
and the non-negotiables stage 3 recorded with their check *owed to you*.

For every invariant, ask **how would you know it was violated?** Write each as a
statement someone could check by reading a diff, running a test, or grepping a tree:

- "No request handler reaches storage directly."
- "No entity is written by a feature that does not own it."
- "Every externally-triggered action appears in the audit log."

Prose is enough. Do not write the test — there is no code yet and the stack was settled
ten minutes ago. What matters is that mechanising each later is a small job.

Stage 3 may have recorded non-negotiables with their check **owed to you**. Those are your
first inputs here.

**An invariant nobody can check automatically is not a preference.** "The system never
determines legal liability" is binding, non-negotiable, and unenforceable by a test — filing
it beside undecided work misrepresents it. Record those under
`### Invariants with no automated check`, with why, and **what non-automated control
substitutes** — a review step, an approval, an audit. Only genuinely optional preferences go
to Deliberately deferred.

### 7. Deliberately deferred

What is not being decided now, and the trigger that reopens each one.

**This is the exit for every blocking marker** — `[NEEDS DECISION]`, `[NEEDS LOOKUP]` and
`[NEEDS CLARIFICATION]` alike. A `[NEEDS LOOKUP]` parks with a **named fetcher and a due
date**; a `[NEEDS CLARIFICATION]` parks naming the product owner and the question put to them. A fork the architect genuinely cannot
settle today moves here with its reopening trigger, which resolves it for ratification
purposes. Without that exit the marker would block ratification forever, since inventing
an answer is forbidden.

## The feedback loop

You will find things the experience baseline promised that the system cannot deliver. This
is expected and is why the architect goes last.

For each: name it, say what is achievable instead, and record it under **Open questions**
addressed to **whoever owns the promise** — the experience lead for an interaction, the
product owner for anything the constitution or a ratified success criterion fixed. An
experience lead cannot amend the constitution, and routing everything to them strands the
findings that matter most. Write an ADR if the
reasoning is worth preserving.

Per the revision beat in `../../CONVENTIONS.md`, the experience lead reads these when
their baseline is next amended. Keep them specific enough to act on.

A handful of conflicts is a revision; a rewrite means stage 3 was too thin, which is worth
saying out loud.

## Writing the document

**Read `.specify/memory/architecture.md` before writing it.** If it already contains this
stage's design sections, this is a re-ratification after a stage-3 amendment: edit in place,
bump the version, update `**Last Amended**`, and **never re-apply the template** or reset
the footer. Clearing an existing ratification before the new one is asked for loses it with
no record if the architect then says "keep it a draft".

In either case, consume `## Deferred to the baseline` and stage 3's `## Open questions` exactly as described below.

Otherwise, carry the foundations sections forward and add the design sections after them. Amend a foundations section only where stage 3 was wrong,
explain each change, and record it in the register as a supersession. Remove the
`**Status of this document**: foundations captured; …` line that stage 3 wrote.

**Consume stage 3's two working sections rather than leaving them:**

- **Deferred to the baseline** — every entry is either decided in a section below, or
  moved to **Deliberately deferred** with a trigger. Delete the section when empty.
- **Open questions** — entries still unanswered move into this document's Open questions,
  keeping the role each is addressed to.

```markdown
# [Project] Architecture Baseline

**Last reviewed against specs**: 001–NNN as at [YYYY-MM-DD]

## Supersedes / defers to
## Constraints                     *(from stage 3; amend in place if wrong)*
## Shared domain model             *(from stage 3; amend in place if wrong)*
## Shared state vocabularies       *(from stage 3; amend in place if wrong)*
## Interface contracts             *(from stage 3; amend in place if wrong)*
## Authority model                 *(from stage 3; amend in place if wrong)*
## Where the design is free        *(from stage 3)*

## Stack and boundaries
## Cross-cutting mechanisms
## Non-functional budgets
## Build order
## Fitness functions
### Invariants with no automated check
   *(binding, unenforceable by a test — with why, and the control that substitutes)*
## Deliberately deferred
## Open questions
   *(each entry names the role it is addressed to)*

**Version**: [x.y.z — bump on amendment, never reset] | **Status**: [Draft | Ratified] | **Signed off by**: —
**Ratified**: — | **Last Amended**: [YYYY-MM-DD] | **Against constitution**: [x.y.z]
```

Append every decision made here to `.specify/memory/decisions.md` per
`../../CONVENTIONS.md`, with its class, owner, source and hedge. The stack, the budgets,
the build order and the fitness functions are the largest block of binding decisions the
method produces, and none reach the register unless this step happens.

Leave `Status`, `Signed off by` and `Ratified` unset until the Ratification step below
resolves them. Open each design section with one italic line naming the divergence it
prevents.

## The gate

Evaluate this **before** offering ratification.

Write `.specify/memory/checklists/architecture-baseline.md`. **Do not rewrite stage 3's
checklist** — read
`.specify/memory/checklists/architecture-foundations.md`, restate its counts in Coherence below,
and leave the file alone. Two stages, two files, per `../../CONVENTIONS.md`.

```markdown
# Architecture Baseline Checklist: [PROJECT]

**Purpose**: Validate that the architecture baseline is complete and binding before building starts
**Created**: [YYYY-MM-DD]
**Document**: [../architecture.md](../architecture.md)

## Completeness

- [ ] Every design section names the divergence it prevents
- [ ] Stage 3's foundations were re-checked against the full spec set, and amendments recorded in the register
- [ ] Every shared entity field has exactly one owning feature, or is recorded as "no feature exists that could own it" and addressed to the product owner
- [ ] Every surface state in the experience baseline has a system-side answer; domain states are cited, not re-answered
- [ ] Build order is dependency-traced, not preference-ordered
- [ ] Every entry from stage 3's Deferred to the baseline is decided or re-deferred with a trigger

## Checkability

- [ ] Every invariant is either written so a violation could be detected, or recorded under Invariants with no automated check with its substitute control
- [ ] Every non-negotiable whose check stage 3 recorded as owed has one now, or is recorded under **Invariants with no automated check** with the non-automated control that substitutes
- [ ] Every constitution NON-NEGOTIABLE has a fitness function or an entry under Invariants with no automated check
- [ ] Every budget is a figure with its hedge and source, or carries a marker
- [ ] Every marker was re-checked: nothing settleable today is left as a measurement

## Coherence

- [ ] No decision here contradicts a constitution principle, a ratified success criterion, or ratified vocabulary
- [ ] No system budget contradicts a *decided* interaction budget
- [ ] Stage 3's foundations gate was read and its counts restated, not overwritten
- [ ] Every decision made here is in the register with its class, owner, source and hedge, and every unresolved marker has an Unresolved row naming who resolves it
- [ ] Existing technical material is superseded or deferred to, explicitly
- [ ] Constraints recorded in stage 3 are unchanged, or each change is explained
- [ ] The constitution version this was signed against is recorded, or its absence is recorded in its place

## Readiness

- [ ] Every feature at the head of the build order can start without a further decision; anything blocked is listed with what unblocks it
- [ ] Every open question another baseline addressed to the architect is answered here, or restated with why it is still open
- [ ] Every [NEEDS DESIGN] in the experience baseline is answered system-side or listed as an open risk
- [ ] No *unparked* blocking marker remains — unresolved markers: CLARIFICATION __ DECISION __ LOOKUP __ DESIGN __ MEASUREMENT __ (parked __)

Mark any item that cannot apply to this project `[~]` with a one-line reason — `[~]` is
not a failure and does not block ratification.


## Previous evaluation — [YYYY-MM-DD], v[x.y.z]
   *(the prior run's items and their states, verbatim; moved here before this run was
   evaluated, and never edited)*

**Stage 3 counts, restated**: entities modelled __ of __ · fields owned __ / no-owner-exists __ · state dimensions __ · unresolved markers DECISION __ LOOKUP __

**Counts**: budget rows with a real figure __ of __ · fitness functions __ · open questions returned to the experience lead __ · to the product owner __ · ADRs written __
```

Fill the counts. The last readiness item is the real test: if a developer would still have
to invent something, the baseline is not finished.

## Ratification

Follow **the ratification procedure** in `../../CONVENTIONS.md`. Only after the gate is
evaluated.

Ask directly: *is this ready to be treated as binding, or should it stay a draft?* Explain
what changes:

- **Draft** — the build may deviate but must say where and why.
- **Ratified** — a deviation requires an ADR rather than a judgement call.

Record their name and today's date in the footer, and set Status.

**Status is not set to Ratified while any gate item is `[ ]`, or while any *unparked*
blocking marker remains — `[NEEDS DECISION]`, `[NEEDS LOOKUP]` or `[NEEDS CLARIFICATION]` —
whoever asks.** A parked marker still appears in the document; blocking on its mere presence
re-closes the exit. See the ratification procedure in `../../CONVENTIONS.md`.
`[NEEDS MEASUREMENT]` does not block: a missing figure is honest and does not stop the
structure binding.

If a baseline was ratified and a later gate fails, say so plainly and return it to Draft.
A binding document beside a failing gate is worse than an honest draft.

## Finishing

Report in plain terms:

- **What is now binding**, section by section.
- **The fitness functions**, listed. A reviewer will check against these, so the architect
  should recognise every one.
- **Your defaults**, flagged as yours.
- **Every outstanding marker**, by type, with who supplies each.
- **Anything returned to the experience lead, and anything returned to the product owner**, each named individually. The product owner has no baseline skill that reads open questions, so this report is their only delivery mechanism.
- **Status**, and what it means for the build.
- **Where it was saved**, and that `.specify` is hidden by the operating system.

Say plainly that the baseline will be amended once building starts, and that amending it
is normal — the alternative is a build quietly diverging from a document nobody updated.
