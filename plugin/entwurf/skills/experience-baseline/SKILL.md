---
name: experience-baseline
description: Interviews the experience or UX lead about how a product behaves for the people using it — the conditions they work in, the surface states every screen must handle, the component and pattern vocabulary, the words, the accessibility floor, and the interaction budgets — and writes it up as a project experience baseline that every feature inherits. Use this after the product specifications exist and the technical constraints are captured, and before the architecture baseline is written. Trigger it when someone wants to define the design system, establish UX foundations, agree interaction patterns, work out what the product looks like in each state, or set the design ground rules a whole product must follow.
---

# Experience Baseline

Decide once, for the whole product, what someone sees and what they can be asked to do —
so that eleven features do not become eleven products.

**Read `../../CONVENTIONS.md` first.** It carries the shared rules — stages, where work
is saved, the tree, markers, interview rules, precedence, gates, decision records and the
baseline status footer. This skill does not repeat them.

This is **stage 4 of 5**. What you produce is the input the architecture baseline needs.

## Why this tier carries the weight

A component defined for one feature is reused by six others; a state defined once applies
everywhere; a word chosen here appears on forty screens.

Note the honest limit: the per-feature experience document this is meant to be inherited
by **does not exist yet** (see the tree in `../../CONVENTIONS.md`). Today this baseline is
consumed by stage 5 and by whoever builds. Say that rather than promising an assembly
step nobody has built.

## Text is what gets built from

The output of design work is visual. The thing that builds this product reads files.

**Text is authoritative for behaviour; the design file is authoritative for appearance.**

Where the two meet — a state's signal is appearance that carries behaviour — record the
**distinguishing dimension and its floor**, not the rendering:

> "Sync state is distinguished by icon plus word, never colour alone. Legible at arm's
> length in poor light. Survives a screen reader and translation."

That is implementable, it is a real design decision, and it leaves the rendering to the
file. "Saved offline is signalled" is not a decision.

## What this is not

**Not per-feature flows.** **Not visual design** — no colour values, type scales or
spacing systems. If the lead volunteers them as binding, record a pinned reference to the
design file rather than transcribing them.

## Before starting

Establish the project root as `../../CONVENTIONS.md` describes.

**If there are no specs, say so and stop.** This skill's entire method is derivation from
them, and an experience baseline invented from a blank page is the failure this method
exists to prevent. If there are specs but no constitution, carry on, say so, and record the
absence in the document.

Read:

- `.specify/memory/constitution.md` — principles, users, and **ratified vocabulary**
- every `specs/NNN-*/spec.md`
- `.specify/memory/architecture.md` — the constraints, the **shared domain model**, the
  **shared state vocabularies** (topic 4 cites these rather than re-deriving them), the
  **authority model**, especially **Where the design is free**, and any **Open questions**
  addressed to the experience lead. The authority model is what makes topic 2's
  unauthorised-action question answerable; if it is missing, that is an open question back
  to the architect rather than something you decide.
- `.specify/memory/decisions.md`, if it exists — so you do not reopen something already
  settled
- **any existing material covering the same ground, wherever it lives** — see "Look for
  what already exists" in `../../CONVENTIONS.md`. Do not restrict this to design files:
  status vocabularies, state diagrams and interaction rules are routinely written inside
  architecture documents. Reconcile with them rather than claiming them — how a state is
  *signalled* is yours; what the states *are* belongs to the architecture baseline.

If the architecture constraints do not exist, carry on — but **record that in the
document itself**, under a "Written without architecture constraints" line, and hold
ratification. A spoken caveat is lost; the next stage will otherwise open a
normal-looking draft with no sign it was designed blind.

If an experience baseline already exists, treat this as an amendment: read the other
baselines' Open questions first and lead with anything addressed to you.

**If a ratified architecture baseline was built against this document**, an amendment here
can leave its budgets and fitness functions derived from a guarantee you have just
superseded. Say so plainly, record it as an Open question addressed to the architect, and
note that stage 5 must re-run to restore coverage — the same shape as stage 3's
`Amended since sign-off` note, running the other way.

**If the document carries a "Written without architecture constraints" line and those
constraints now exist**, reconcile against them, delete the line, and record the
reconciliation in the register. That is the only thing that lifts the ratification hold —
without it the document can never become binding.

## Who you are talking to

A practising designer. **The product-owner rule inverts** — see the expert-interview
section in `../../CONVENTIONS.md`.

The neighbouring-domain failure has a specific shape here: a designer will confidently
assert what the system can do — how much lives on the device, how fast sync is, what is
searchable offline. **Test:** *does the answer change if the device, the network, or the
data volume changes?* If so it is the architect's.

Route it without a fight, and state your own guess so the architect has something to
overturn:

> "I'll record thirty days as your working assumption and put it to the architect to
> confirm — if it comes back shorter, we'll know before it's built."

## Derive first, then ask

The specs are full of experience material. Mine them and lead with what you found; the
correction is the information.

> "Working offline runs through most of your specs, and photo capture through about a
> third. That implies at least these surface states — saved locally, syncing, sync
> failed, partially synced. What am I missing, and which of these actually look
> different to someone?"

Give a sense of weight rather than exact counts.

Some material below is **for you, not to be read aloud** — coverage lists and rules of
thumb are interviewer's notes. Reading them to a professional tells them what they have
believed for a decade.

## The interview

Eight topics. Expect an hour or more; topics 3, 4 and 5 carry most of the value. Splitting
across two sittings is fine — if you do, take 1, 2 and 4 first, because the state work
tells you which patterns topic 3 needs.

### 1. Who is at the screen, and in what condition

Not personas. Physical and cognitive conditions at the moment of use: what they are
holding, what else they are doing, light, noise, time, stress, gloves, who is watching.

**Ask first whether there is more than one answer.** Most products have several surface
classes — a frontline mobile surface and a seated back-office one are common, and their
conditions have nothing in common. The specs usually make this obvious, and the
constitution sometimes says so outright. Running them together produces one blurred
profile and leaks a frontline budget onto a desk-based screen that explicitly disclaims
it.

Record the reasoning, not just the conclusion. "One-handed" is a fact; "one-handed
because the other hand is carrying something and they will not put it down" survives an
argument.

### 2. Navigation and information model

How someone gets anywhere, per surface class. What the home surface is, what is one tap
away, what can be reached mid-task without losing work.

Two questions that are usually the highest-value ones available:

- **What must be reachable urgently**, and what must be impossible to reach by accident?
- **What does an action someone is not authorised for look like?** Take the **disclosure
  classes** from the architecture baseline's authority model and decide one rule per
  **(class, viewer, surface)** the model implies — the same authority is often concealed
  from one viewer and fully explained to another, so a rule per class alone under-specifies
  it. A class the designer judges wrong or unimplementable is a `[NEEDS DECISION]`
  addressed to the architect, not something to design around — concealed, visible-but-blocked, visible-with-reason, existence-disclosed. Do
  not pick a single house rule: a product with several classes will violate one of them,
  and the constitution usually requires the difference. If the authority model carries no
  disclosure classes, that is an open question addressed to the architect, not a decision
  you make on their behalf.

### 3. Component and pattern inventory

The direct equivalent of the constitution's glossary: it stops eleven features building
eleven date pickers.

**Derive jobs from the specs, not elements.** Specs describe what someone must do; they
rarely name UI. **Enumerate the job list in the document first**, ordered by how many specs each job appears in, and state where you cut it — an unbounded list makes the gate item unpassable at any real scale. Then bring that list of *jobs* — "choose a person from a roster", "attach
evidence", "show why this is restricted" — and ask the designer to name the element that
performs each. **You must not name the elements yourself**: `../../CONVENTIONS.md`
forbids inventing names, and a widget named by the interviewer ends up in the code by
Friday.

For each: the designer's name, what it is for, and **when to use it rather than its
nearest neighbour**. A list without selection rules gets ignored.

### 4. State inventory

This is the section most likely to be got wrong, because "state" means two different
things and only one of them is the designer's.

**Lead with the harvested dimensions and their count**, and ask what is missing from the set — not how many there are. Asking a professional to count what you have just read to them is the interrogation `../../CONVENTIONS.md` forbids. Real products have several,
and they are orthogonal — a record can be syncing *and* pending review *and* partially
settled at once. A single flat list renders co-occurring states as alternatives, which is
a bug that reaches production. Establish the dimensions before enumerating any values.

Then, per dimension, classify it:

- **Surface states** — empty, loading, saved-locally, syncing, sync-failed, stale,
  restricted, error. Usually around eight. **The designer decides these.**
- **Domain lifecycle states** — already enumerated in the architecture baseline's
  `## Shared state vocabularies` section, where the architect recorded them and their
  owners. **These are not re-decided here.** If no such section exists, derive the
  dimensions from the specs as a **proposal**, mark each "derived here, not ratified", and
  return the whole set to the architect as an open question — do not present a derived
  dimension as owned. Harvest them from that section, cite where each is defined, and ask the designer how each is
  signalled and which pairs could be mistaken for one another.

  **A dimension the designer says is missing, overlapping, or wrongly owned is a finding,
  not a query.** Record it in `## Status dimensions` as
  `[NEEDS DECISION: owner and vocabulary — proposed here, not ratified]` with their
  proposed values, addressed to the architect. Composite state is then decided against the
  confirmed dimensions only, and says so.

Asking a designer to re-decide ratified domain vocabulary invites them to contradict a
document the business has already signed, and they will rightly refuse.

For every state, record what it means, how it is signalled (per the distinguishing-
dimension rule above), and what the person can still do while in it.

**Then handle composition.** If several dimensions co-occur, how do they render together
in one line someone reads in four seconds? This is usually the hardest design problem in
the product and there is no other place it gets solved.

Finally, list **confusable pairs**. Vocabulary collisions — two things both called
"Submitted" — are already found at stage 3; carry those forward and decide what separates
them on screen. What you add is the pair that collides **visually**: two states with
different words that land in the same place, at the same size, and read the same at a
glance. That is invisible to everyone upstream.

### 5. Content and voice

The words are part of the product.

Decide: how uncertainty is phrased, how a warning reads without causing panic, whether
the product addresses the person directly, what it never says. Where the product handles
anything sensitive, wording is a correctness concern rather than tone.

Cover translation if needed: what is translated, what never is, and how an original and a
translation are distinguished on screen.

**The constitution's vocabulary outranks this document** — see Precedence in
`../../CONVENTIONS.md`. When the designer argues a ratified term misleads people, they
may well be right, but the move is a **proposed constitution amendment recorded under
Open questions addressed to the product owner**, not a local override.

Capture actual strings for anything load-bearing. A rule about tone is not implementable;
a sentence is.

### 6. Accessibility floor

The minimums that are non-negotiable, stated so they can be checked. Apply **how would
you know?** to every one.

Ask what would exclude someone from using this product at all, given topic 1's
conditions. That finds real constraints a standards checklist misses.

### 7. Interaction budgets

Taps to complete, time on task, how long before someone assumes it is broken.

**Establish who holds each number before asking for it.** Where the specs or constitution
assign a figure to a pilot, a measurement plan, or the product owner, record the owner
and the marker — do not interrogate the designer for a number the method has already
assigned elsewhere.

Do not invent them. Where none exists, write the **shape**: a start event, a stop event,
the conditions, the surface class it binds, and the population or percentile. The figure
alone carries `[NEEDS MEASUREMENT]`. A budget with no shape propagates into the
architecture baseline and then the build as a category name.

Ask which missing numbers would change the architecture, and flag those — nine unmeasured
budgets arriving at stage 5 flat is much less useful than two marked as load-bearing.

### 8. What you are deliberately not deciding

Close by asking what is being left open on purpose, and **what would reopen it**. Without
the trigger, deferred decisions get relitigated by whoever arrives next.

## Writing the document

**Read `.specify/memory/experience.md` before writing it.** If it does not exist, apply
the template below. If it exists, this is an amendment: edit in place, bump the version,
update `**Last Amended**`, and **never re-apply the template** — doing so resets a ratified
baseline to `Draft | — | —` and erases a sign-off with no record anywhere. Leave `Status`,
`Signed off by` and `Ratified` as they are until the Ratification step resolves them, and
keep the original ratification date.

```markdown
# [Project] Experience Baseline

**Written without architecture constraints** — *(include only when they were absent;
deleting it after reconciling is what lifts the ratification hold)*

## Surface classes
## Conditions of use
   *(per surface class)*
## Navigation and information model
## Component and pattern inventory
## Status dimensions
   *(the independent dimensions, before any values)*
## State inventory
   *(surface states — decided here; domain states — cited, with signalling only)*
## Composite state
## Confusable pairs
## Content and voice
## Accessibility floor
## Interaction budgets
## Design references
   *(if no design file exists, say so and say what follows: appearance is unowned. A
   binding appearance decision the lead states is recorded here verbatim, marked
   **provisional, pending a design file**, with the guarantee it carries — a size or a
   position that a promise depends on is not decoration.)*
   *(otherwise pinned — a version, date or frame, never a bare link. A live link drifts within a
   week and the "authoritative for appearance" claim then points at something else.)*
## Deliberately deferred
   *(with the trigger that reopens each one)*
## Open questions
   *(each entry names the role it is addressed to — architect or product owner)*

**Version**: [x.y.z — bump on amendment, never reset] | **Status**: Draft | **Signed off by**: —
**Ratified**: — | **Last Amended**: [YYYY-MM-DD] | **Against constitution**: [x.y.z]
```

Present the inventories as tables. They are looked up mid-build, not read once.

Append to `.specify/memory/decisions.md` per `../../CONVENTIONS.md`, with the right class
— a component name the designer chose is **Decided**; one you proposed and they accepted
is **Recommended and accepted**.

Write an ADR per `../../CONVENTIONS.md` for any decision a reasonable person would
question later — the unauthorised-action affordance and any challenge to ratified
vocabulary are textbook cases.

Use `[NEEDS DESIGN: question]` for an interaction genuinely left unresolved, and give each
one a reopening trigger under `## Deliberately deferred` — an unresolved interaction with
no trigger is an unresolved interaction that ships.

A blocking marker you cannot resolve is parked, not left to block: see "Every blocking
marker needs an exit" in `../../CONVENTIONS.md`. Open
questions addressed to another role are prose, not markers, and are uncapped.


## The gate

Write `.specify/memory/checklists/experience.md` with evaluated state per
`../../CONVENTIONS.md`. On a re-run, move the previous run's evaluated
items into the dated `## Previous evaluation` block before evaluating this one, and never
edit it — a failure that was fixed and one silently dropped must not look identical.

```markdown
# Experience Baseline Checklist: [PROJECT]

**Purpose**: Validate that the experience baseline is complete and buildable before the architecture baseline is written
**Created**: [YYYY-MM-DD]
**Document**: [../experience.md](../experience.md)

## Completeness

- [ ] Surface classes are identified, and conditions of use are recorded per class
- [ ] Every job on the recorded job list has a named element, and the list records how it was bounded
- [ ] Independent status dimensions are enumerated before any state values
- [ ] Domain states are cited to where they are defined, not re-decided
- [ ] Composite state — how co-occurring dimensions render together — is decided, or carries a [NEEDS DESIGN] naming the dimension it is blocked on and who confirms it, or the document records that only one status dimension exists
- [ ] Every deferred decision records the trigger that reopens it
- [ ] Every disclosure class in the authority model has its own rule for what an unauthorised action looks like, or is recorded as not present on this product

## Buildability

- [ ] Every decision here is stated so a per-feature designer inherits it without re-deciding it
- [ ] Every state signal records a distinguishing dimension and its floor, not a rendering
- [ ] Every pair of states that could be mistaken for each other, including across different objects, is listed with what separates them
- [ ] No behavioural decision is recorded only as "see the design file"
- [ ] Design references are pinned to a version, date or frame

## Discipline

- [ ] No decision here contradicts a constitution principle or its ratified vocabulary
- [ ] No decision here contradicts a constraint recorded in the architecture foundations, or the contradiction is an open question addressed to the architect
- [ ] Where no design file exists, the document records that appearance is unowned
- [ ] Every accessibility item states how a violation would be detected
- [ ] Every budget records a start event, stop event, conditions and surface class
- [ ] Claims that depend on system capability are routed to the architect with a stated guess
- [ ] Every figure carries its hedge and its source
- [ ] The constitution version this was written against is recorded, or its absence is recorded in its place
- [ ] Every decision is in the register with its class, owner, source & hedge, date and status, and every unresolved marker has an Unresolved row naming who resolves it
- [ ] Every open question another baseline addressed to the experience lead is answered here, or restated with why it is still open

Mark any item that cannot apply to this project `[~]` with a one-line reason — `[~]` is
not a failure and does not block ratification.


## Previous evaluation — [YYYY-MM-DD], v[x.y.z]
   *(the prior run's items and their states, verbatim; moved here before this run was
   evaluated, and never edited)*

**Counts**: surface classes __ · status dimensions __ (confirmed __ / proposed __) · jobs identified __ / named __ · disclosure rules __ of __ (class × viewer) · components named __ · budgets with a real figure __ of __ · unresolved markers: CLARIFICATION __ DECISION __ LOOKUP __ DESIGN __ MEASUREMENT __ (parked __) · open questions to architect __ · to product owner __
```

Fill the counts — they are what a second reader can check.


## Ratification

Follow **the ratification procedure** in `../../CONVENTIONS.md`. Only after the gate above
is written and evaluated.

Ask explicitly: *is this ready to be treated as binding, or should it stay a draft?*

- **Draft** — advisory; the build may deviate but must say where and why.
- **Ratified** — binding; a deviation requires an ADR.

Record the outcome **in the footer** — set `Status`, `Signed off by` and `Ratified`
together. A ratification recorded only in the conversation did not happen, and the
document on disk stays Draft.

**Status is not set to Ratified while any gate item is `[ ]`, while any unparked blocking
marker remains, or while the document still carries the "written without architecture
constraints" line — whoever asks.** A `[~]` item does not block; it carries its one-line
reason instead. `[NEEDS DESIGN]` and `[NEEDS MEASUREMENT]` do not block. Record any request
for early ratification, and the refusal, in the register.

## Finishing

Report in plain terms:

- **What is now decided for the whole product** — the component, dimension and state
  vocabulary especially.
- **Conditions of use and the accessibility floor.** These drive more of the architecture
  than anything else in the document, and stage 5 needs them named rather than buried.
- **Your defaults**, flagged as yours.
- **Every `[NEEDS MEASUREMENT]`**, with who holds the number, and which of them would
  change the architecture.
- **Composite state** — the rule you decided, and what it is blocked on if anything.
- **Every `[NEEDS DESIGN]`**, individually, with who resolves it.
- **Open questions, grouped by who they are addressed to.**
- **Status** — Draft or Ratified, and what that means.
- **Where it was saved**, and that `.specify` is hidden by the operating system.
- **What happens next**: the architect writes the baseline against this and may return
  things that cannot be delivered as designed. That is expected, and handled as a small
  revision rather than a redesign.
