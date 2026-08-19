---
name: architecture-foundations
description: Interviews the technical architect about the foundations a project is built on — the immovable constraints, the shared domain model and which feature owns each field, the interface contracts between features, and the authority model governing who can do what. Runs early, before experience design, and is re-run as specifications accumulate. Deliberately stops short of budgets, mechanisms and build order, which need the experience work first. Use this once the first specifications exist, and again whenever a new specification introduces an entity, a state or an authority. Trigger it when someone wants to capture technical constraints, establish a shared data model, define what each feature owns, agree interface contracts, or work out the guardrails a design has to live within.
---

# Architecture Foundations

Establish what is already fixed, what the shared things are called, who owns each of them,
and who is allowed to do what — so that specifications written weeks apart still describe
one system.

**Read `../../CONVENTIONS.md` first.** It carries the shared rules — stages, where work is
saved, the tree, markers, the decision register, interview rules, precedence, gates,
decision records and the baseline status footer. This skill does not repeat them.

This is **stage 3 of 5**, and it is the one stage that is **a living document, not a
one-off interview.**

## Run this early, and run it again

Specifications drift. Each new feature introduces concepts the earlier ones could not
anticipate, and without something owning the shared entities the drift is silent: two
specs model the same employee differently, three disagree about which state triggers
what, and nobody notices until integration.

So: run this as soon as the first few specifications exist — you do not need all of them —
and **re-run it whenever a new specification introduces an entity, a state, or an
authority.** On a re-run, lead with what has changed since last time and what the new
specs broke, rather than starting from the top.

The cost of running it early is that some of it will be wrong. The cost of running it late
is that everything built in between was built against nothing.

## What this is not

**Not budgets, build order, fitness functions, or mechanisms chosen for a quality** — performance, cost, scale, resilience. Those are stage 5, and
they cannot be written until the experience baseline exists — they convert user-facing
guarantees into system guarantees, and the guarantees do not exist yet.

The test for what belongs here: **does it need to know how the product looks or feels?**
Which feature owns the employee record does not. How long an offline capture may survive
does. The first is yours now; the second waits.

If the architect starts designing mechanisms — and they will — record it under **Deferred
to the baseline** and move on.

## Before starting

Establish the project root as `../../CONVENTIONS.md` describes.

**If there are no specs, say so and stop.** This stage derives from them; a constitution
alone is not enough, and a domain model invented from a blank page is the drift it exists
to prevent. If there are specs but no constitution, carry on, say so, and record the
absence in the document.

Then read, before asking anything:

- `.specify/memory/constitution.md` — note anything it fixes that is properly an
  architecture decision, per Precedence in `../../CONVENTIONS.md`
- every `specs/NNN-*/spec.md` that exists so far — the requirement bodies, not only the
  **Key Entities** sections; the entities that drift are the ones nobody declared
- `.specify/memory/decisions.md`, if it exists
- **any existing technical material anywhere in the project** — see "Look for what already
  exists" in `../../CONVENTIONS.md`.

  **Found material carries its own status; read it and carry it verbatim.** A document
  that calls itself "a recommendation, pending review" is evidence of a decision, not
  proof of a constraint — and promoting it wholesale manufactures exactly the false
  constraint you are trying to avoid, in the opposite direction. One document can be firm
  in one section and provisional in another. Bring its own words into topic 3's firmness
  test rather than inferring firmness from the fact that somebody wrote it down.

On a re-run, also read the other baselines' **Open questions** and lead with anything
addressed to the architect.

## Who you are talking to

A practising architect. **The product-owner rule inverts here** — see the expert-interview
section in `../../CONVENTIONS.md`.

Watch for the neighbouring-domain failure: an architect will confidently answer a question
about how people work. Route it — and still state your own guess, flagged as yours.

## Derive first, then ask

The specs already imply most of this. Mine them and lead with what you found.

> "Six of your specs touch the employee record and three of them describe it differently.
> I've drafted one model with payroll owning identity and the roster owning employment
> periods. What have I got wrong — and what's fixed that isn't written down anywhere?"

That last clause earns the interview. Most real constraints live in someone's head.

## The interview

Ten topics. A first run over a mature spec set is most of a session — the three-pass
derivation and the disclosure table carry it. A re-run is smaller only if the new specs add
entities without redefining existing ones; a spec that redefines a shared entity's identity
or lifecycle forces a rewrite, and it is honest to say so rather than pretend it is a diff.
**Length is not the signal of drift — content is.** If you are discussing how something
will be built rather than what it is and who owns it, defer it.

Record absence explicitly in every topic. "No external integrations" is a real answer.

### 1. What already exists that this has to work with

Systems that will not be replaced, data that already lives somewhere, integrations that
are contractually required, identity and access already established.

For each: what it is, why it cannot be replaced, and **what it forces**.

### 2. Regulatory, contractual and legal obligations

What the law, a regulator, an insurer or a contract requires of the system itself.

Where an obligation exists but its parameters are unknown, that is almost always
`[NEEDS LOOKUP: who supplies it]` — a statutory window is a fact somebody can fetch, and
marking it a measurement guarantees it never blocks anything.

### 3. Platform commitments already made

What has been decided, by whom, and how firmly.

**A firm commitment is a constraint and belongs here. One that could still be revisited is
a decision and belongs under Deferred to the baseline** — it fails the "would it still be
true" test by definition. Ask directly which they would defend and which they would drop.
Write an ADR for anything reclassified.

### 4. Team and delivery reality

Who builds it, how many, what they know, how long there is. A two-person team that has
never shipped offline sync is a harder constraint than any regulation.

**If the team does not exist yet, that is the answer** — record it, and record what is
still being decided about it. Do not press the architect for it: hiring and budget are
commercial facts belonging to whoever funds the work, and the neighbouring-domain rule
applies to them exactly as it does to how people work.

### 5. Non-negotiables from experience

The things this architect knows will hurt, from having been hurt. Ask directly — they feel
like opinion rather than fact and are rarely volunteered.

For each, ask **how would you know it was violated?** and record the answer as the
*observable signal of a violation* — not as a fitness function. Writing down that
unencrypted sync traffic would show up in a proxy is recording a signal; deciding which
check runs in CI is stage 5's job.

**A non-negotiable whose check cannot be designed yet still stays here.** Record the check
as *owed to stage 5* and keep the constraint. Do not demote it — the experience lead reads
Constraints, and these rules have direct interaction consequences.

### 6. The shared domain model

**This is the section that prevents the drift, and the reason this stage runs early.**

The entities that appear in more than one specification, and for each field: **which
feature owns it.** Ownership is the part that stops divergence — a shared entity with no
owner gets modelled three ways by three specs written a week apart.

**Do not derive from the specs' Key Entities sections alone.** The entities that drift are
the ones nobody declared — they appear in requirement bodies, not in a declared list, and
some specs have no Key Entities heading at all. Run three passes:

1. **Declared** — every Key Entities section that exists.
2. **Undeclared** — every noun used by more than one spec that no spec defines. Search
   requirement bodies. *This is where the drift is*: a shared thing nobody owns because
   nobody wrote it down.
3. **Contested fields** — every field written by more than one spec's requirements.

Passes 2 and 3 produce the findings. Pass 1 mostly confirms what everyone already knew.

Lead with a draft model built from all three.

Three questions per entity, which are the ones that surface real disagreement:

- **Which feature owns each field, which *writes* it, and which merely reads it?** Owner
  and reader is two states and the real drift is three: one nominal owner, several
  writers. **More than one writer is a finding, not a note** — it means the same value is
  produced by two mechanisms that may disagree, and it has no state transition, so
  contracts will not catch it.
- **What makes two records the same record?** Identity rules are where imports, rehires
  and duplicates go wrong.
- **Where does it live when the answer is "none of the above"?** Every real system has a
  state nobody designed for, and it is usually discovered during the build.

**A field may have three dispositions, not two.** Owned by a feature; owned by nobody but
with a candidate that could own it; or — the important one — **no feature exists that
could own it**, because the spec that would define it has not been written.

That third case is the most valuable output this stage produces. A shared entity used
everywhere and defined nowhere is a missing specification, and saying so is a finding
addressed to the product owner. Do not resolve it by assigning it to whichever feature is
nearest, and do not write "the platform owns it" — that is writing "nobody" in a font that
passes a gate.

### 6b. Shared state vocabularies

Every status enumeration that appears in more than one specification: which feature owns
its vocabulary, what the values are, and whether the words are user-facing.

This exists because the re-run trigger names "a state" and nothing else in the method owns
one. Stage 4 cites domain states rather than deciding them, and stage 5 cites them rather
than re-answering them — so if this stage does not own them, nobody does, and they drift
in the gap.

Look specifically for **the same word meaning different things**. Two features both using
"Submitted", or a constitution glossary defining a term one spec omits and another extends,
is live drift in a shared vocabulary — and it is the class of thing a developer resolves by
collapsing several independent state machines into one column.

Record them as separate dimensions. Never merge two that can be true at once.

### 7. Interface contracts

How features hand work to each other — shapes and responsibilities, not endpoint lists.
What every caller can rely on, what errors look like, what is versioned.

The question that catches most cross-spec drift: **which state in one feature triggers
another?** Specifications describe their own behaviour and rarely name the handoff, so it
is exactly what goes missing.

### 8. The authority model

Who is allowed to do what, and how that is expressed. Roles, scopes, delegation, and
whether authority is per-record, per-location or global.

**This is needed early because the experience lead cannot design without it** — but only if
you ask the question they actually need answered.

**For every authority type, record its disclosure class**: may the person be told that the
restricted thing exists, and may they be told why?

- **Concealed** — its existence must not be revealed; showing a disabled control leaks it.
- **Visible but blocked** — they may see it exists and that they cannot act.
- **Visible with reason** — they may be told why, and sometimes must be.
- **Existence disclosed, contents concealed** — they may know a thing is happening without
  seeing any of it.

Without this, stage 4 has to guess, and a product usually needs several of these at once —
so a single house rule will violate one of them. Also ask about **derived authority**:
permission computed from state elsewhere, which ends by itself when that state changes.
It is harder than delegation and rarely volunteered.

Where the constitution has already fixed part of this, carry it and note the source.

### 9. Where the design is free

Close by asking what is **not** constrained. This is the section the experience lead
actually reads, and the one the architect finds most unnatural — it asks them to say where
they have no opinion. A document that only lists walls reads as one that forbids
everything.

## Writing the document

Write `.specify/memory/architecture.md`. This is the **first half** of a document
`architecture-baseline` completes at stage 5 — do not create a separate file.

**Read that file before writing it**, and take one of three branches:

- **It does not exist** — apply the template below.
- **It exists but has no stage-5 design sections** — this is still an amendment. Edit the
  foundations in place, bump the version, update `**Last reviewed against specs**` and
  `**Last Amended**`, and **never re-apply the template**. Re-applying it resets an
  architect's existing work to a fresh draft, and this is the commonest re-run there is.
- **It contains *any* of stage 5's design sections** — Stack and boundaries, Cross-cutting
  mechanisms, Non-functional budgets, Build order, Fitness functions — then additionally:

- Edit the foundations sections **in place**. Do not edit the *design* sections — Stack
  and boundaries through Fitness functions. `## Deliberately deferred` and
  `## Open questions` are yours to append to, never to rewrite.
- Leave `Status`, `Signed off by` and `Ratified` exactly as they are — you are not
  un-ratifying anyone's document. **But if `Status` is `Ratified`, the signature no longer
  covers what you just changed.** Add
  `**Amended since sign-off**: <sections>, v<new> — not covered by the sign-off at v<signed>`
  to the footer, record it as an Open question addressed to the architect, and say plainly
  in the closing report that stage 5 must re-run to restore coverage. A binding document
  quietly acquiring unsigned content is worse than one that loses work, because nothing
  looks wrong.
- Bump the version — minor for an added entity or contract, major if you have changed
  what an existing entity means.
- Update `**Last reviewed against specs**` and `**Last Amended**`.
- If `## Deferred to the baseline` is absent because stage 5 consumed and deleted it,
  **recreate it** — it is stage 5's agenda for the next re-ratification, and topic 3 still
  sends newly-deferred mechanisms there.
- If your amendment invalidates something in a stage-5 section, **do not delete it**.
  Record it under `## Open questions` addressed to the architect. If the document is
  Ratified, say plainly that it must return to Draft and **ask the architect to make that
  call in the conversation** — you may set Status to Draft on their say-so, and only then.

Overwriting the template onto a completed baseline destroys the budgets, build order and
fitness functions and silently discards a ratification. That is the single worst thing
this skill can do, and re-running after stage 5 is the most likely re-run there is.

```markdown
# [Project] Architecture Baseline

**Status of this document**: foundations captured; budgets, mechanisms, build order and
fitness functions are written at stage 5, after the experience baseline exists.

**Last reviewed against specs**: 001–NNN as at [YYYY-MM-DD]

## Supersedes / defers to
## Constraints
### Existing systems
### Regulatory and contractual obligations
### Platform commitments
### Team and delivery reality
### Non-negotiables
## Shared domain model
   *(one row per field: entity | field | owner | writers | readers | derived from (spec FRs) | as at)*
   *(a row whose owner is "no feature exists" is a finding, not a blank)*
## Shared state vocabularies
   *(one table per independent dimension — never merge two that can be true at once)*
## Interface contracts
## Authority model
## Where the design is free
## Deliberately deferred
   *(parked markers, each with its reopening trigger; a [NEEDS LOOKUP] names its fetcher
   and due date, a [NEEDS CLARIFICATION] names the product owner and the question)*
## Deferred to the baseline
   *(stage 5's agenda)*
## Open questions
   *(each entry names the role it is addressed to)*

**Version**: [x.y.z — bump on amendment, never reset] | **Status**: Draft | **Signed off by**: —
**Ratified**: — | **Last Amended**: [YYYY-MM-DD] | **Against constitution**: [x.y.z]
```

Record the spec range this was last reviewed against. On a re-run, update it — that field
is how anyone tells whether the model has seen the newest features.

Append to `.specify/memory/decisions.md` per `../../CONVENTIONS.md`, with the right class:
an entity owner you proposed and the architect accepted is **Recommended and accepted**,
not **Decided**.

This document stays **Draft** until stage 5. Ratifying foundations alone would be signing
off half a document.

## The gate

Write `.specify/memory/checklists/architecture-foundations.md` with evaluated state per
`../../CONVENTIONS.md`. On a re-run, move the previous run's evaluated
items into the dated `## Previous evaluation` block before evaluating this one, and never
edit it — a failure that was fixed and one silently dropped must not look identical.

```markdown
# Architecture Foundations Checklist: [PROJECT]

**Purpose**: Validate that the foundations are usable by specification and experience work
**Created**: [YYYY-MM-DD]
**Document**: [../architecture.md](../architecture.md)
**Reviewed against specs**: 001–NNN · **previous run**: 001–NNN as at [YYYY-MM-DD]

## Constraints

- [ ] Every constraint states what it forces, not only what it is
- [ ] Every topic records its absences explicitly rather than omitting them
- [ ] Every platform commitment recorded as a constraint is firm; reversible ones are deferred
- [ ] Existing technical material in the project was found and reconciled, or its absence confirmed

## Foundations

- [ ] Every entity appearing in more than one spec is in the model, including those no spec declares
- [ ] Every field records owner, writers and readers, and where it was derived from
- [ ] Every field is owned, or recorded as "no feature exists that could own it" and addressed to the product owner
- [ ] Every entity states what makes two records the same record, or is recorded as having no owning feature, in which case the identity rule is owed to the spec that will own it
- [ ] Every status vocabulary appearing in more than one spec is recorded as its own dimension
- [ ] Every cross-feature handoff names the state in one feature that triggers the other
- [ ] Every authority type carries a disclosure class
- [ ] Areas where the design is free are named explicitly

## Discipline

- [ ] No mechanism chosen for a *quality* — performance, cost, scale, resilience — appears, and no budget, build order or fitness function. Identity and contract mechanisms belong here
- [ ] Every non-negotiable states how a violation would be detected, or records that check as owed to stage 5
- [ ] Every unknown carries the right marker — a fetchable fact is [NEEDS LOOKUP], not [NEEDS MEASUREMENT]
- [ ] Every figure carries its hedge and its source
- [ ] Every decision is in the register with its class and owner, and every unresolved marker has an Unresolved row naming who resolves it

## On a re-run only

- [ ] Every entity introduced by a spec added since the last review is in the model, or excluded with a reason
- [ ] Every model entry whose source specs changed was re-derived, not carried forward
- [ ] Stage 5's sections, and the ratification footer, are untouched by this run
- [ ] If the document was Ratified, the footer records which version the sign-off actually covers, and the architect was told the amendment is not covered by it
- [ ] Anything this run invalidated in a stage-5 section is an open question, not a deletion

## Handover

- [ ] Every open question names the role it is addressed to
- [ ] Every open question another baseline addressed to the architect is answered here, or restated with why it is still open
- [ ] The spec range this was reviewed against is recorded
- [ ] The constitution version this was written against is recorded, or its absence is recorded in its place

Mark any item that cannot apply to this project `[~]` with a one-line reason — `[~]` is
not a failure and does not block. The "On a re-run only" block is `[~]` on a first run.


## Previous evaluation — [YYYY-MM-DD], v[x.y.z]
   *(the prior run's items and their states, verbatim; moved here before this run was
   evaluated, and never edited)*

**Counts** — give both figures so a reader can see the direction of travel:
specs reviewed __ (prev __) · entities in >1 spec __ / modelled __ · fields total __ / owned __ / no-owner-exists __ (closed since last run __ / new __) · fields with >1 writer __ · state dimensions __ · authority types __ / with disclosure class __ · open questions __ · unresolved markers: CLARIFICATION __ DECISION __ LOOKUP __ DESIGN __ MEASUREMENT __ (parked __)
```

**Fields with a candidate owner that is still unassigned should be zero.** Fields where
*no feature exists that could own them* should not — that number is the single most useful
output of this stage, and it is a list of specifications nobody has written yet.

Report both, with denominators. A model that scores well by simply not modelling the
awkward entities is worse than one that scores badly honestly.

## Finishing

Report in plain terms:

- **What is now fixed**, one line each.
- **The shared model** — the entities and who owns them. This is what the specs anchor to.
- **The authority model**, since the experience lead cannot start without it.
- **Where the design is free** — repeat it; it is what they will forget they said.
- **Your defaults**, flagged as yours.
- **Open questions, grouped by who they are addressed to.**
- **Where it was saved**, and that `.specify` is hidden by the operating system.
- **When to come back**: as soon as a new specification introduces an entity, a state or an
  authority. Say this explicitly — a living document that nobody returns to is just an
  early document that went stale.
