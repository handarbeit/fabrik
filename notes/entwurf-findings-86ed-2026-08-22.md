# entwurf — findings from the first real project

**Project**: 86ED, a restaurant operations product. Eleven specifications and a constitution
existed when entwurf's stage 3 first ran; four more specifications and two constitution
amendments followed over four days, driven by what stage 3 found.

**The pairing**: entwurf and Fabrik ship from the same repository and are intended to be used
together. entwurf produces the specifications, the architecture baseline and the decision
register; **Fabrik's agents consume all three**. Several findings below
follow from that, and the two plugins currently do not reference each other at all.

**Why this is worth reading**: `CONVENTIONS.md` cites this project as the motivating case for
making stage 3 a living document — *"eleven specifications written over days drifted, because
each new feature changed the meaning of earlier ones and nothing owned the shared entities."*
This is what happened when the stage it never got was finally run against it.

**Outcome, as a number.** Fields in the shared domain model with no feature that could own them:

```
v1.0.0   25        first run
v1.2.0   25        constitution amendment — went UP by 2, closed 0
v1.3.0   16        after one new specification
v1.5.0   13
v1.6.0   10        three architect decisions, no new specs
v2.0.0    1        after three more specifications
```

Every drop came from a specification being written. None came from further work on the
architecture document. That is the headline, and several findings below follow from it.

**Eleven findings and three smaller ones.** Nothing here is a defect in the method as written —
these are things it does not currently say. Findings 3 through 5 all follow from one fact the
method never states: the reader of these documents is an agent.

---

---

## The frame these findings sit in

**Fabrik is an entire engineering team.** That is the useful way to read everything below.
entwurf's job is to hand that team a body of work resolved well enough that contradictions do
not keep bouncing back to the product owner, the experience lead and the architect.

Some will bounce back. There will be judgement calls in flight. That is true of any team and it
is not a failure of the specifications. What matters is that a team of people and a team of
agents fail differently, in three specific ways:

**Memory exists, but in a different artifact than entwurf writes to.** I assumed workers had no
memory across issues and that is wrong — see finding 10. Fabrik agents write ADRs, and on
`~/dev/fantasy` they have written 204 of them, 182 citing another ADR and 187 citing an issue,
with supersession marked in line. The gap is not amnesia; it is that entwurf's register and
Fabrik's ADRs are two halves of one job and neither tool writes to the other's half.

**Asking is cheap for people and expensive for agents.** An engineer leans over and asks; it
costs seconds, and the answer is absorbed by everyone in earshot. A worker either blocks the
issue or decides silently. The cost asymmetry inverts the incentive exactly where you least
want it inverted.

**No "that can't be right."** The most valuable thing a team does with a fresh specification set
is read it and disbelieve parts of it. That function exists in this method — it is stage 3 —
but it runs periodically rather than continuously, and nothing routes a worker's disbelief back
into it.

So the target is not *no escalation*. It is **escalation that is cheap, correctly addressed, and
recorded where the next worker will see it.** Several findings below are versions of that.

---

## 1. Stage 3 finds missing specifications, and there is no route back to stage 2

**What happened.** The largest single output of the first run was 24 fields no feature could
own, which resolved to five missing specifications — tenancy and scoping, matter lifecycle,
user accounts, acknowledgment, and one more. Four specifications were subsequently written and
stage 3 re-ran after each.

**What the method says today.** `architecture-foundations`, topic 6:

> A shared entity used everywhere and defined nowhere is a missing specification, and saying
> so is a finding addressed to the product owner.

That is exactly right and it stops there. There is no described route for the finding to reach
the product owner, no statement that they should write the specification, and no instruction to
re-run stage 3 afterwards — even though the re-run trigger ("a new specification introduces an
entity, a state or an authority") describes precisely what a spec written to close such a
finding will do.

**What we built instead.** A GitHub Project distinct from the delivery board, one issue per
finding, and a pair of project-scoped skills — one that briefs the product owner on what is
assigned to them and one that drafts a well-formed issue. Every project would reinvent this.

**Proposed change.** Describe the loop in `CONVENTIONS.md` as a first-class part of the method,
alongside the revision beat which it resembles:

> **The specification beat.** Stage 3 routinely discovers that a shared entity has no owning
> specification. That finding returns to stage 2, the product owner writes the specification,
> and stage 3 re-runs — its own trigger guarantees it must. Expect several cycles. On this
> project three cycles took 25 unowned fields to 1, and no amount of further work on the
> architecture document would have closed a single one.

Setting that expectation matters. A reader of the current text could reasonably believe stage 3
is a single pass that ends with a list of complaints.

---

## 2. "Spec amendments owed" earned a place in the template

**What happened.** Stage 3 makes decisions that specifications must reflect — an entity owner,
a corrected derivation, a duplicated definition collapsed to one. At peak this project carried
**twenty** such decisions that no specification had absorbed.

**What the method says today.** Precedence, rule 3:

> An earlier stage's decision stands until the later stage returns it as an open question and
> the earlier role amends it.

Correct, and there is no mechanism. Open questions are for things unresolved; these are
resolved and unapplied, which is a different state and a more dangerous one — the baseline
reads as settled while the specifications still say the old thing.

**What we built.** A `## Spec amendments owed` section: spec, amendment, and the finding it
came from. It became the most operationally important part of the document.

**And it matters more than "untidy" because agents read both documents.** An unapplied
amendment does not leave a gap in a worker's context — it leaves a **contradiction**. The
baseline says the roster supplies facts; the specification says the roster derives authority.
Both are in front of the agent. It will pick one, silently, and there is nothing in either
document telling it which wins.

**Proposed change.** Add it to the `architecture.md` template between `## Deferred to the
baseline` and `## Open questions`, with a line in the gate: *every decision requiring a
specification change is listed as owed until the specification reflects it.*

---

## 3. entwurf produces specifications, a baseline and a register; Fabrik consumes all three — and neither plugin says so

The two plugins ship from the same repository, are used together by design, and **do not
reference each other**. entwurf's output is `specs/NNN-name/spec.md`. Fabrik's Specify stage
opens with:

> Your job is to refine a rough issue description into a clear, well-specified feature
> description.

and Plan states plainly: *"The issue body is the spec, owned by Specify."* So Fabrik's model is
issue-first — a rough issue becomes a spec — while entwurf's is file-first. Where entwurf has
run, **Specify's authoring job is already done, and doing it again would overwrite an authored
specification with a generated one.**

86ED discovered this per-project and recorded it as a local gotcha. `~/dev/fantasy` solved the
opposite direction — the issue body is the spec and Specify projects it to a file. Neither
solution is in the plugin, so every project meets the seam cold.

**Proposed change.** State the pairing in both plugins, and define the entwurf-first direction:

- **Where a `specs/NNN-*/spec.md` exists, the issue references it rather than containing it**,
  and Specify becomes a *reading and consistency* stage rather than an authoring one. Its
  existing "check consistency with existing features" work is the valuable half and should
  read `.specify/memory/architecture.md` and `decisions.md`, not only `CLAUDE.md`.
- **Specify must never rewrite an authored specification.** Stock behaviour destroys it.
- entwurf's README should say what happens next; Fabrik's should say what it expects to find.

### 3a — the PLANNED per-feature paths already have a producer

`CONVENTIONS.md` describes `specs/NNN-name/technical.md` as PLANNED with no producer:

> The baselines are written to be inherited by per-feature documents that do not exist yet.

**Fabrik's Plan stage produces exactly that document** — a per-feature implementation design
derived from the spec — and posts it as a stage comment rather than writing the file. The
producer exists; the two halves have never been introduced.

### 3b — stage 5's outputs are Fabrik configuration, not advice

- **Build order** is the board's dependency graph. 86ED's own orientation already says *"build
  order is a stage-5 output — do not hand-wire issue dependencies before then,"* which is a
  project reinventing the rule.
- **Fitness functions** are what Validate gates on. Stage 5 currently describes them
  abstractly; paired with Fabrik they have a concrete home, and "the check is owed to stage 5"
  becomes "the check is owed to Validate."
- **Pipeline readiness** is a board rule: a specification whose referenced entities are unowned
  does not get an issue yet.

### 3c — the outer and inner loops

The two plugins are two loops and the projects using them will invent the distinction anyway.
86ED did, on day two, and it clarified everything downstream:

| | Outer loop — entwurf | Inner loop — Fabrik |
|---|---|---|
| Cycle | product | code |
| Worked by | people | agents |
| Output | specifications, decisions, ADRs | pull requests |

The seam between them is human-gated: an outer-loop task settles a specification, and only then
does an inner-loop issue exist for it. Nothing crosses automatically. Worth shipping rather
than rediscovering.

---

## 4. Because agents implement, "an implementation detail" is now a defect class

**Of everything here, this is the one I would fix first.**

The corpus contains **27 deliberate deferrals** — ten phrased as "an implementation detail",
seventeen as "outside this spec's scope" — spread across **9 of its 15 specifications**. Every
one of them is well-judged by the standards of specification writing. Examples:

- *the specific CSV file format is an implementation detail outside this spec's scope*
- *the exact matching criteria for a stable source identifier are an implementation detail*
- *the specific mechanism for detecting similar names worth flagging is an implementation detail*

With a human engineer these are correct. Over-specifying is a real failure and the specs avoid
it. **With an agent implementation they are invention points** — and unlike an unowned entity,
which stage 3 surfaces, a deferral is *invisible to every check in the method*. It reads as
good practice. It passes 16/16.

The risk is not that the decision is undeferred. It is that the chain runs:

```
spec defers → Research or Plan agent decides → nobody sees it until Review
```

The decision still gets made — by an agent, silently, differently in each specification that
defers the same thing, with the first human sight of it at Review.

**Proposed change.** `write-spec` and `clarify` should treat a deferral as requiring a **named
consumer**, in the same way `[NEEDS LOOKUP]` requires a named fetcher:

> Deferring a decision is legitimate. Deferring it to nobody is not. Every "this is an
> implementation detail" must name who decides it and when — a human at Plan, the architect at
> stage 5, or the build team. Where implementation is automated, a deferral with no named
> consumer is a decision an agent will make silently, and the same deferral appearing in two
> specifications is two agents making it differently.

That is a small change to a checklist item and it would have caught all 27.

## 5. Precedence was written for humans reading documents. Agents read them now

Fabrik's workers consume the specifications, `.specify/memory/architecture.md` and
`.specify/memory/decisions.md`. Three of the method's rules become load-bearing in a way they
were not when a person was doing the reading.

**"Draft" means advisory, and the build may deviate.** `CONVENTIONS.md`:

> **Draft** — advisory. The build may deviate, but must say where and why.

Stage 3 stays Draft by design until stage 5. So for most of a project's life, **the architecture
baseline is advisory to the build** — and an agent has to know both that it is permitted to
deviate and that it is obliged to say so. Neither is discoverable from the document, which
carries `**Status**: Draft` in a footer and explains nowhere what that obliges a reader to do.

**The Class column is now a parsing rule, not a nuance.**

> A row whose class is anything other than **Decided** is not a commitment and must never be
> quoted downstream as though it were.

Downstream is an agent. It will read `Working assumption` and `Recommended and accepted` and
`Technical suggestion` rows in the same table as `Decided` ones, and nothing in its context
tells it these bind differently. On 86ED the register carries all six classes and several
architect's-own-defaults deliberately flagged as guesses — exactly the rows an agent should
not build against without asking.

**Precedence needs to be machine-legible.** The ordering exists and is good:

> 1. The constitution outranks every baseline · 2. A ratified baseline outranks a draft ·
> 3. An earlier stage's decision stands until the earlier role amends it · 4. Constraints
> recorded at stage 3 bind stage 4 regardless of document status

Rule 3 is the one that resolves the contradiction above — the specification wins until amended,
and the baseline's decision is inert until applied. That is the right answer and **no worker
will ever find it**, because it lives in the plugin's shared rulebook rather than anywhere a
Fabrik agent reads.

**Proposed change.** A short precedence block, written for a worker rather than a role, carried
where workers actually look — the project's orientation file, or better, a Fabrik stage-skill
section:

> The specification is what you build. The architecture baseline constrains how. Where they
> disagree, the specification wins and you flag the disagreement rather than resolving it — a
> baseline decision that the specification has not absorbed is not yet in force. Anything in
> the decision register whose Class is not "Decided" is not a commitment; treat it as context
> and ask. A Draft baseline is advisory: you may deviate, and you must say where and why.

**And the non-negotiables need to reach the worker at all.** 86ED's baseline carries four —
one central authority decision point, insert-only audit at the storage layer, no status
collapsed into one column, sensitive material in a separate tier rather than a filtered view.
Each records how a violation would be detected. Those are precisely Review and Validate gates,
and nothing currently carries them from the baseline to the stage that would enforce them.

---

## 6. A clean checklist does not mean a consistent corpus

**What happened.** A specification came back **16/16 with no clarification markers while
directly contradicting an existing specification** — it permitted configuration to cross a
boundary that another spec forbade crossing. It surfaced only because the architect read it
against the corpus.

**Why it is not a `write-spec` defect.** `write-spec` validates a document against itself,
which is all a per-document checklist can do. But the product owner reasonably read 16/16 as
"nothing left to resolve," and said so.

**Proposed change.** Two small ones.

`write-spec`'s checklist gains a closing note stating what it does not check:

> This checklist validates this specification against itself. It cannot detect a conflict with
> another specification — that check happens at stage 3, which reads the corpus as a whole.
> A clean result here means this document is internally sound, not that it agrees with the
> others.

And `architecture-foundations` gains an explicit instruction to look for cross-spec
contradiction, not only for undeclared and contested entities. Passes 2 and 3 find things
nobody declared; nothing currently tells the architect to find things two specs declare
*differently*.

---

## 7. The gate decays silently, and nothing says when to re-run it

**What happened.** The baseline moved through four version bumps without its checklist being
re-evaluated. It sat claiming a spec range and a set of counts that were two constitution
versions and two specifications out of date, understating progress by twelve fields.

**What the method says today.** `CONVENTIONS.md` is strong on preserving previous evaluations
and on the checkbox state being durable:

> later skills compare it before and after their own run, and the build reads it as a gate

It says nothing about **when** a re-evaluation is required. The re-run trigger is defined for
new specifications, not for amendments to the baseline itself.

**Proposed change.** One sentence in Gates:

> A version bump requires a gate evaluation. If the amendment is small enough not to warrant
> one, say so in the checklist header — an unexplained gap between the document's version and
> its gate's is indistinguishable from a gate nobody ran.

---

## 8. There is a third disposition, and it is neither owned nor unowned

**What happened.** Twice a field marked "no feature exists that could own it" was reclassified
rather than closed. One was deliberately unspecified because the constitution treats that class
of logic as proprietary and restricted. The other was deferred — the capability does not ship
in the pilot at all, and the principle governing it was reworded to describe a boundary rather
than assert that something exists.

Neither is a gap. Both had been sitting in the model inviting someone to fill them.

**What the method says today.** Topic 6 names three dispositions: owned by a feature; owned by
nobody but with a candidate; no feature exists that could own it.

**Proposed change.** Add a fourth: **deliberately out of scope** — restricted, deferred, or
excluded, with the decision recorded. It is not a gap, and labelling it as one is as misleading
as writing "the platform owns it."

---

## 9. Three smaller findings

**entwurf produces AI-assisted documents and says nothing about labelling them.** This project
independently wrote rules into three of its own specifications requiring AI-assisted content to
carry a visible provenance label until a human verifies it — and then merged an unlabelled
244-line design document produced with AI assistance. The method's own outputs are in the same
category. Worth a line in `CONVENTIONS.md`: a baseline produced through these interviews is
AI-assisted, and where a project sets its own provenance rules, its baselines are subject to
them.

**A scope exclusion should record why, so the reason can be checked for expiry.** A finding was
deliberately excluded from a task on the grounds that it needed an engineer's judgement and the
project had none. Two days later the build team was answered and the exclusion had to be
reversed. Exclusions written as bare "out of scope" cannot be audited for staleness; written as
"out of scope *because*" they can.

**Split a question before asking it when it has a product half and an architecture half.** One
question — where a particular authority lives — was put to the architect as a single fork. He
correctly refused it: the product half had already been answered in a clarification session
months earlier, and only the storage half was his. Interview rules already say to route
neighbouring-domain claims to their owner; they could also say to *split* a question that spans
two domains rather than routing the whole thing to one.

---

## 10. The register and the ADR log are two halves of one job, and each tool uses only its own

**I had this wrong and the evidence corrected it.** My first draft claimed Fabrik workers carry
no memory between issues. They do. On `~/dev/fantasy` — Fabrik without entwurf, one human, the
full six-stage pipeline — agents have written **204 ADRs**. 182 of them cite another ADR, 187
cite an issue, and supersession is marked in line (*"Superseded by ADR-109"*). A recent one
reasons about a review bot's diff limit, quotes its own measurements, and links two prior
decisions. That is a working institutional memory, written by agents, for agents.

Now put the two projects side by side:

| | `~/dev/fantasy` — Fabrik alone | 86ED — entwurf then Fabrik |
|---|---|---|
| ADRs | **204** | **1** |
| Decision register | none | **87 rows** |
| Specs | 538, one per issue | 15, one per feature |

Each tool produced its own artifact and ignored the other's entirely.

**Both artifacts are load-bearing and they are not substitutes.** `CONVENTIONS.md` already says
why:

> ADRs record individual decisions and their reasoning. They do not tell you which decision is
> still in force.

Fantasy's in-line supersession softens that — you *can* find what is current — but only by
scanning every ADR touching a topic and checking each status. That is a search, not a lookup,
and it gets worse at 204 and worse again at 500. And the register carries classes ADRs do not:
working assumptions, exploratory ideas, technical suggestions binding on nobody, and unresolved
markers naming who resolves them. Fantasy has no place for *"we are proceeding on this but
nobody has confirmed it"*.

Meanwhile 86ED has one ADR and 87 register rows — the mirror failure. Several of those rows are
implementation-shaped decisions with real reasoning that deserved an ADR and got a table cell.

**Proposed change.** State the division of labour and make each tool write to both:

- **An ADR is the reasoning**; a register row is **what is currently true**. A decision worth an
  ADR is worth a register row pointing at it. A register row whose reasoning would not fit in a
  cell wants an ADR.
- **Fabrik's stage skills should append a register row** when they make a decision that outlives
  their issue — with an honest Class, which is the column that keeps an agent's judgement call
  visibly distinct from a product owner's ruling.
- **entwurf should write ADRs**, not only register rows, for its own architecture decisions.
  Topic 3 already says to write one for a reclassified commitment; that instruction should be
  general.

## 11. Escalation is normal, and the answer has nowhere reusable to land

`FABRIK_BLOCKED_ON_INPUT` exists, so a worker can stop and ask. Two things around it are
missing.

**The block is role-blind.** A worker can say it is blocked; it cannot say *on whom*. The
baseline already carries the vocabulary — 86ED's open questions are grouped as *addressed to the
product owner*, *to the architect*, *to the experience lead*, and that grouping is what made
them actionable. A block that names its audience lands in the right lane; one that does not
becomes something a human has to triage.

**A worker's escalation is evidence about the corpus, not just about that issue.** Plan's skill
frames an open question as an upstream failure — *"if there are, something was missed
upstream."* True, and the useful response is also to treat it as a **stage-3 re-run trigger**.
The existing triggers are "a new specification introduces an entity, a state or an authority";
a worker blocking on an ambiguity is at least as strong a signal that the corpus has drifted.

**And this reframes finding 4.** The 27 deferrals are not wrong. A real team receives
specifications with open ends and settles them in a standup. The problem is that asking is cheap
for that team and expensive for this one — so the fix is both halves: **name the consumer, and
make sure a route to them exists.** 86ED built that route: an outer-loop board, one issue per
question, a skill that briefs the person on what is waiting. It was built for entwurf's
findings, it is equally the inner loop's escalation channel, and neither plugin knows it exists.

---

## What worked, and should not be changed

- **The three dispositions for a field, and the instruction not to resolve a gap by assigning
  it to the nearest feature.** This is what produced the missing-specification findings at all.
  A weaker method would have let the architect quietly assign Venue to whichever spec mentioned
  it most.
- **Counts in the gate rather than claims.** "25 → 16 → 13 → 1" told the story better than any
  prose, and made it obvious that architecture work was not what closed the gap.
- **Preserving previous evaluations verbatim.** Reading the v1.0.0 evaluation next to the
  current one shows a failure that was fixed and one that was silently dropped as different
  things, exactly as intended.
- **Stage 3 staying Draft until stage 5.** It was amended nine times in four days. Anything
  ratified would have been re-ratified nine times or quietly lied about.
- **The register's Class column.** Distinguishing "Decided" from "Recommended and accepted"
  from "Working assumption" mattered constantly, particularly for keeping the architect's own
  defaults visibly separate from the product owner's decisions.
