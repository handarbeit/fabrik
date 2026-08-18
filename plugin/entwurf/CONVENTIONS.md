# Conventions

**Every baseline skill reads this file before starting** — `architecture-foundations`,
`experience-baseline`, `architecture-baseline`. It holds the rules they share, so a
rule changes here once rather than in three places.

The three product-owner skills predate this file. `write-spec` and `clarify` now defer to
it for the marker vocabulary; `onboard` does not reference it at all and keeps its own
conventions, including its own footer shape and its own ratification behaviour for the
constitution — which is exempt from the procedure below. **When you change a rule here,
grep for it in `skills/*/SKILL.md`.**

Skills reference this file as `../../CONVENTIONS.md` from `skills/<name>/SKILL.md`.

## The three roles

A project needs three experts, and each writes a different kind of document. None of
them is qualified to write another's.

| Role | Knows | Writes |
|---|---|---|
| **Product owner** | the business, the users, the problem | what the product must do and why |
| **Experience lead** | how people work and what they can be asked to do | what someone sees, in what state, in what words |
| **Architect** | how systems are built and where they break | what the system guarantees and how it is bounded |

## The stages

| Stage | Skill | Produces |
|---|---|---|
| 1 | `onboard` | `constitution.md` |
| 2 | `write-spec`, then `clarify` | `specs/NNN-*/spec.md` |
| 3 | `architecture-foundations` | constraints, shared domain model, contracts, authority model |
| 4 | `experience-baseline` | `experience.md` |
| 5 | `architecture-baseline` | budgets, build order, fitness functions |
| — | **the revision beat** | amendments to any of the above |

The architect appears at stages 3 and 5, bracketing the experience work.

**Stage 3 runs early and is re-run as specs accumulate.** It is the only stage that is
explicitly a living document rather than a one-off interview. This comes from a real
project: eleven specifications written over days drifted, because each new feature
changed the meaning of earlier ones and nothing owned the shared entities. The author's
own retrospective concluded that *a shared domain model and interface contracts created
near the beginning would have prevented much of it.*

**What splits early and what stays late** follows one test: *does it need the experience
lead's output?*

- **Early (stage 3)** — constraints, the shared domain model and field ownership,
  interface contracts, and the authority model. None of these depend on how the product
  looks, and the specs need them while they are still being written. The experience lead
  also needs the authority model to decide what an unauthorised action looks like.
- **Late (stage 5)** — non-functional budgets, cross-cutting mechanisms, build order and
  fitness functions. These convert user-facing guarantees into system guarantees, so they
  cannot be written before those guarantees exist.
- **Split across both** — *stack and boundaries*: a firm commitment is a stage-3
  constraint, a reversible one is a stage-5 decision. And *where the design is free*
  is written at stage 3, because stage 4 is its reader.

**A mechanism is not automatically late.** The test is not "is this a mechanism" — it is
*does this need to know how the product looks or feels?* An identity rule ("same record
iff matching stable external ID, never name") is a mechanism and belongs at stage 3,
because contracts and identity have no mechanism-free form. What waits for stage 5 is a
mechanism chosen for a **quality** — performance, cost, scale, resilience — because those
trade against the experience.

Stage 5 cannot run before stage 4. Stage 3 must not wait for either, and should be
re-opened whenever a new spec introduces an entity, a state or an authority.

The dependency runs **what the user must experience → what the system must guarantee →
how it is built**. Reverse it and the architect invents the guarantees, which is how a
product ends up committed to numbers nobody agreed to.

### The revision beat

Later stages find things earlier stages got wrong. This is expected and is not a
failure of either.

Every baseline skill, on finding that an earlier document cannot stand, records the
problem in its own **Open questions**, **addressed to a named role**. Every baseline
skill, when re-run as an amendment, **reads the other baselines' Open questions first**
and leads with anything addressed to it.

That is what makes the loop close. An open question with no named audience is a note to
nobody, and the whole method leaks through that gap.

Amending a document is normal. Re-ratify it afterwards and bump the version.

## Where the work is saved

**Establish the project root before writing anything.**

- Use the connected folder if there is exactly one; ask which if there are several.
- If a `.specify/` folder already exists at that folder **or anywhere above it**, the
  folder containing it is the project root. Someone may have connected a sub-folder of
  a project that is already set up, and a second `.specify/` splits the project in two.
- Otherwise the connected folder is the project root, and `.specify/` is created there.

**If no folder is connected, do not fall back to a temporary or scratch directory.**
Say so plainly and hand the files back for download instead. A temporary working
directory almost always exists, especially in the cloud, and treating one as the
destination looks like success while quietly throwing the work away. Staging files in
order to hand them back is fine; finishing and leaving the only copy somewhere that
disappears is not. Tell the person how to connect a folder so it saves next time.

**Look for what already exists.** Before writing a baseline, search the project for
material that covers the same ground — `architecture/`, `design/`, `docs/`, any
`*architecture*.md`, `*design*.md`, exported research, a blueprint. Real projects
accumulate these outside `.specify/`, and a baseline written in ignorance of one
produces a second source of truth that contradicts the first. Read what you find,
treat its decisions as already made, and settle explicitly whether your document
supersedes it or defers to it.

## The tree

All paths relative to the project root. `.specify/` and `specs/NNN-name/` are GitHub
Spec Kit's own names — never rename them to match a skill, and never invent variants.

```
.specify/memory/
  constitution.md          product baseline
  architecture.md          technical baseline
  experience.md            experience baseline
  decisions.md             the decision register — what is currently true, and who decided
  checklists/
    architecture-foundations.md   stage 3 gate
    architecture-baseline.md      stage 5 gate
    experience.md                 stage 4 gate

specs/NNN-feature-name/
  spec.md                  what it must do and why
  checklists/
    requirements.md        product gate
  technical.md             PLANNED — no skill writes this yet
  experience.md            PLANNED — no skill writes this yet

adrs/NNN-short-title.md    shared decision history, all roles
```

**The two PLANNED paths have no producer.** The baselines are written to be inherited
by per-feature documents that do not exist yet. Until they do, the baselines are
consumed only by the stage after them — say so rather than implying an assembly step
that nobody has built.

Create `.specify/memory/checklists/` if it does not exist.

Feature numbering: take **one higher than the highest number present**, zero-padded to
three digits — never the lowest unused one. Gaps left by deleted features stay gaps.

## Markers

One vocabulary across all roles. Each marks a gap as a first-class object rather than a
silence, and none may be resolved by inventing an answer.

| Marker | Means | Blocks ratification |
|---|---|---|
| `[NEEDS CLARIFICATION: question]` | a product decision only the business can make | until parked |
| `[NEEDS DECISION: question]` | a fork an expert must settle, but hasn't yet | until parked |
| `[NEEDS LOOKUP: who supplies it]` | a fact that exists in the world; nobody here has fetched it | until parked |
| `[NEEDS DESIGN: question]` | an unresolved interaction | no |
| `[NEEDS MEASUREMENT]` | a number that does not exist yet | no |

`[NEEDS BASELINE]` is a **legacy alias for `[NEEDS MEASUREMENT]`**, used by the
product-owner skills and present in existing `specs/*/spec.md`. Treat the two as
identical. **Do not rewrite existing ones** — changing markers inside a ratified spec
is a worse problem than carrying two spellings.

### Choosing between them

These three are routinely confused, and choosing wrong is how a decision disappears.

- **Could a competent expert settle it today, with the people already in the room?**
  Then it is `[NEEDS DECISION]`, however numeric it looks. "Maximum offline duration"
  is a decision, not a measurement.
- **Does the answer exist somewhere in the world — a statute, a contract, a carrier's
  terms, a vendor's limits — and nobody has looked it up?** `[NEEDS LOOKUP]`, and name
  who fetches it. This is not a measurement; marking it one guarantees it never blocks
  anything and quietly ships unknown.
- **Would you have to observe or instrument something that does not exist yet?**
  `[NEEDS MEASUREMENT]`. A document carrying one is not defective — it is honest.

A measure whose *shape* is right satisfies its gate even when the figure is missing.
But the shape has to be real: a start event, a stop event, the conditions, and who or
what it applies to. `"Time to complete a closing checklist: [NEEDS MEASUREMENT]"` is a
category name, not a budget.

### Every blocking marker needs an exit

A marker that blocks ratification and cannot be resolved blocks it forever, because
inventing an answer is forbidden. So every blocking marker has a parking route, and the
route is the same one:

**Move it to `## Deliberately deferred` with a reopening trigger.** For
`[NEEDS DECISION]` the trigger is whatever would let someone settle it. For
`[NEEDS LOOKUP]` it is a **named fetcher and a due date**, and the document records that
the areas depending on it are provisional.

For `[NEEDS CLARIFICATION]` the trigger names the **product owner and the question put to
them** — an architect or designer cannot settle it alone, so the parking is provisional
until they answer.

A parked marker no longer blocks. An unparked one does. Statutory windows, carrier terms
and contracts under legal review routinely take weeks — a baseline that cannot bind until
counsel replies is a baseline nobody can build against.

### Caps

**Three per feature document** for `[NEEDS CLARIFICATION]`, `[NEEDS DECISION]` and
`[NEEDS DESIGN]`. More than three usually means guesses are being avoided.

**Project baselines are uncapped.** A baseline spans every feature, and three forks is
the right budget for one spec and the wrong one for eleven. Instead of capping, count:
every gate states how many of each marker remain, and the closing report lists them
individually with their owner.

## Interview rules

These govern every question any skill asks.

**Propose, don't interrogate.** Once you know enough to draft, lead with a concrete
proposal drawn from what they have already said, and ask them to correct it. Reacting
is faster and more accurate than generating from a blank page.

**Take a position.** A neutral menu hands the decision back to someone who came here for
an opinion. Recommend one, say why in a sentence, and make agreeing cheap. "You pick" /
"I don't know" is an answer: take your recommendation, carry on, and record it as *your*
guess rather than their decision.

**Never invent a specific number, threshold, name, or percentage they have not given you
or clearly implied.** Filling in structure is the job; manufacturing facts is not.

**Carry a figure with its hedge and its source.** `roughly 3 years (product owner,
unreviewed)` is not `3 years`. Hedges are lost by transcription, not by decision, and a
number that arrives in a binding document without its qualifier has been silently
promoted. If you drop the hedge, you invented a figure.

**Ask "how would you know?"** Every claim that could be checked, should be — expressed
so that someone could tell whether it had been violated. This is the single question
that separates a document that binds from one that merely reads well.

**Surface your defaults.** List them back at the end, flagged as yours: *"these are my
guesses, not your words — tell me any that are wrong."*

**Record decisions in the document, not the chat.** The transcript is lost; the file is
what the next role and the build read.

### These are expert interviews

The product-owner skills assume no engineering background and must never ask a question
only an engineer could answer. **The baseline skills assume the opposite** — the person
being interviewed is the expert, and simplifying wastes their time.

That inversion has a rule attached, and it matters more with experts, not less:

> **Never default a decision inside the expert's own domain. Always default the
> neighbouring domains, and say so out loud.**

An architect will wave through a plausible-sounding interaction default; a designer will
wave through a plausible-sounding storage default. The answer sounds competent either
way, which is why it has to be flagged rather than absorbed.

**Both halves of that rule matter.** Routing a neighbouring-domain claim to its owner is
only half the job. Having routed it, **still state your own guess, flagged as yours**, so
the other expert has something to overturn rather than a blank page. An open question with
no proposed answer is work moved, not work done.

Use this test: *does the answer change if the device, the network, the data volume, or
the people using it change?* If so it belongs to a different expert.

Some of your material is for you, not for them. A coverage list or a rule of thumb is an
interviewer's note; reading it aloud to a professional tells them something they have
believed for a decade, in a tone implying they don't.

## Precedence

When two documents disagree, this is the order:

1. **The constitution** outranks every baseline, including on vocabulary. A baseline that
   needs different words needs a constitution amendment, not a local override.
2. **A ratified baseline** outranks a draft one.
3. **An earlier stage's decision** stands until the later stage returns it as an open
   question and the earlier role amends it.
4. **Constraints recorded at stage 3 bind stage 4 regardless of document status.** The
   architecture document is Draft by design until stage 5, and rule 2 must not be read as
   letting a ratified experience baseline outrank the non-negotiables it was bounded by.

Where the product owner has already fixed something that is properly an architecture or
experience decision — data residency, a retention period, an identity source — it is
**fixed**, not advisory. Carry it, note that it came from the constitution, and if it is
wrong, return it as an open question addressed to the product owner.

## Gates

Every baseline has a checklist beside it, written to disk with evaluated state — `[x]`
for pass, `[ ]` for fail, `[~]` for **not applicable**, which requires a one-line reason
beside it.

`[~]` exists because ratification turns on "no item is `[ ]`", and without it a single
inapplicable item strands a document forever. A project with no design file cannot pin a
design reference; that is not a failure and must not read as one. The checkbox state is durable state, not a note to yourself:
later skills compare it before and after their own run, and the build reads it as a gate.

**Never overwrite another stage's evaluated gate — or your own previous one.** On a
re-run, move the previous run's evaluated items into a dated
`## Previous evaluation — [YYYY-MM-DD], v[x.y.z]` block above the new one before
evaluating, and never edit it. A failure that was fixed and one silently dropped must not
look identical. Each stage
writes its own checklist file, named for the stage. A later stage *reads* an earlier one
and restates its counts; it never rewrites it. A skill re-running over its own gate keeps
the previous evaluation as a dated block, so a failure that was fixed and one silently
dropped do not look identical.

**A checklist that always passes carries no information.** Prefer items a second reader
could check over items only the author can attest to: a count beats a claim. *"Budget rows
carrying a real figure: 2 of 11"* tells you something; *"no number was invented"* is the
author certifying their own behaviour.

If every item ticks on the first attempt, look harder before reporting it.

## The decision register

`.specify/memory/decisions.md` answers one question that nothing else in the tree
answers: **what is currently true, and who decided it?**

ADRs record individual decisions and their reasoning. They do not tell you which decision
is still in force. On a real project this is the failure that costs most: decisions end
up spread across conversations, specs, the constitution, review notes and consolidated
summaries, and finding the latest authoritative answer means reconciling sources by hand
rather than opening one file.

Every baseline skill appends to the register as it works. **Create the file with its
header row if it does not exist** — a greenfield project has no register until the first
skill writes one. One row per decision that
outlives the conversation it was made in:

| Decision | Class | Owner | Source & hedge | Date | Status |
|---|---|---|---|---|---|
| Postgres for the booking store | Decided | architect | confirmed, spike #4 | 2026-08-16 | Current |
| Retention: roughly 3 years, operational default | Working assumption | product owner | constitution 1.1.0, *unreviewed by counsel* | 2026-08-15 | Current |
| Two weeks of history cached locally | Working assumption | experience lead | designer's estimate, unconfirmed | 2026-08-17 | Current |
| Read replicas for the reporting surface | Recommended and accepted | architect | proposed here, accepted | 2026-08-18 | Current |
| MySQL for the booking store | Decided | architect | pilot scoping note, cost-driven | 2026-08-12 | Superseded by "Postgres for the booking store" |

These rows are deliberately from an unrelated product. **Never carry an example from this
file into a real register** — a pre-classified answer is exactly the guess-hardening this
column exists to prevent.

The **Source & hedge** column exists because a figure loses its qualifier by
transcription, not by decision. `roughly 3 years (unreviewed)` and `3 years` are different
facts, and the second one is the invented figure this method forbids.

### Class — the provenance field

This is the column that earns the register. At project scale these six become
indistinguishable, and a working assumption quietly hardens into a commitment nobody
remembers making:

- **Decided** — someone with the authority chose it. Name which role.
- **Recommended and accepted** — you proposed it, they agreed. Not the same as decided,
  and the distinction matters when it turns out wrong.
- **Working assumption** — proceeding on it, not committed to it.
- **Technical suggestion** — recorded for the build team, binding on nobody.
- **Unresolved** — carries a marker, and names who resolves it.
- **Exploratory** — an idea being held, explicitly not part of the product.

A row whose class is anything other than **Decided** is not a commitment, and must never
be quoted downstream as though it were. A later stage told that something is "already
agreed" must check the class before relying on it.

**No skill should restrict itself to the first two classes.** This is not a quota — never
manufacture a row to fill a class — but a run recording only `Decided` is almost certainly
promoting guesses. In particular: a figure
you are proceeding on that nobody has confirmed is a **Working assumption**, and **every
unresolved marker gets an Unresolved row naming who resolves it, parked or not** — that is what
makes the register answer "what is still open" without opening every document.

### Supersession

When a decision replaces an earlier one, the new row is added and the old row's **Status**
becomes `Superseded by "<the superseding decision's text>"`. Reference it by its words,
never by row position — the table is append-only and unsorted. Never delete a row. A decision that vanishes leaves two
documents disagreeing with no way to tell which is current — which is exactly how an
architecture document and a product blueprint end up committing to different platforms
with nobody noticing.

## Decision records

Architecture and experience decisions both need a record of *why*, and both write into
one shared history at `adrs/NNN-short-title.md`. Product requirements rarely need this,
which is why Spec Kit has no equivalent.

Use the standard three sections — **Context**, **Decision**, **Consequences**.

Number an ADR after the feature that prompted it — `adrs/003-offline-conflict-rule.md` —
suffixing `-a`, `-b` where one feature produces several. A baseline-level decision with no
single feature takes one higher than the highest ADR present.

Write one whenever a decision is made that a reasonable person would question later, and
whenever a feature deviates from a ratified baseline. Deciding that a commitment is
reversible is exactly such a decision.

## Baseline status

Each baseline carries this footer.

```
**Version**: [x.y.z] | **Status**: Draft | **Signed off by**: —
**Ratified**: — | **Last Amended**: YYYY-MM-DD | **Against constitution**: 1.2.0
```

- **Draft** — advisory. The build may deviate, but must say where and why.
- **Ratified** — binding. A deviation requires an ADR, not a judgement call.

`**Signed off by**` and `**Ratified**` stay `—` until Status is Ratified. A draft
carrying a ratification date is self-contradictory.

Sign-off is per role: the product owner ratifies the constitution, the architect the
architecture baseline, the experience lead the experience baseline. Nobody ratifies
another role's document. Ratification is always asked for explicitly and never assumed.

### The ratification procedure

Every baseline that ratifies follows this, in this order — stage 3 is excluded, since it
stays Draft by design until stage 5. It is written here rather than in each skill
because it was previously restated per skill, fixed in one, and left broken in the others.

1. **Write the gate first** and evaluate every item. Ratification is never offered before
   a checklist exists to be checked.
2. **Status is not set to Ratified while any gate item is `[ ]`, or while any unparked
   blocking marker remains — whoever asks.** State this from the document's side, not
   yours. Whoever owns sign-off does not wait to be offered it, and "ratify it now, I'll
   fix the gaps in the amendment" is the most likely thing they will say. The only route
   to binding today is fixing or parking the failing items. Record the request and the
   refusal in the register.
3. **Ask explicitly** — *is this ready to be treated as binding, or should it stay a
   draft?* — and explain what changes either way.
4. **Write the outcome to the footer**: set `Status`, `Signed off by` and `Ratified`
   together. A ratification recorded only in the conversation did not happen.
5. **If a later gate fails**, say so plainly and return the document to Draft. A binding
   document beside a failing gate is worse than an honest draft.

**Against constitution** records the version this was signed against. When the
constitution moves past it, the baseline may be stale — say so rather than assuming it
still holds.

A first creation is `1.0.0`. Thereafter: patch for wording, minor for a new principle or constraint, major for a
change that invalidates work already done. Keep the original ratification date. Don't
explain the scheme unless asked.
