# entwurf — findings from the first real project

**Project**: 86ED, a restaurant operations product. Eleven specifications and a constitution
existed when entwurf's stage 3 first ran; four more specifications and two constitution
amendments followed over four days, driven by what stage 3 found.

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

Nothing here is a defect in the method as written. These are things it does not currently say.

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

**Proposed change.** Add it to the `architecture.md` template between `## Deferred to the
baseline` and `## Open questions`, with a line in the gate: *every decision requiring a
specification change is listed as owed until the specification reflects it.*

---

## 3. Topic 4 should ask *how* it will be built, not only who by

**What happened.** Topic 4 was answered late: the build team is one person plus an agent
pipeline that drives coding agents through a specification-to-merge lifecycle. That single fact
changed the completeness bar for everything before it.

An engineer who encounters "what is a Matter?" asks someone. **An agent invents one** —
plausibly, self-consistently, and differently in each of the four specifications that reference
it. The inconsistency surfaces at integration.

So the twenty-four unowned fields stopped being a documentation gap and became a delivery risk,
and a build-order rule fell out of it:

> A specification is pipeline-ready when everything it references is owned — not when its own
> checklist passes.

**What the method says today.** Topic 4 asks who builds it, how many, what they know, how long
there is. All useful. It does not ask **how** — and agent-built and human-built projects have
materially different definitions of a finished specification.

**Proposed change.** Extend topic 4 with the build model, and note the consequence: where
specifications are the direct input to automated implementation, every unowned field and every
unapplied amendment is a defect waiting to be built rather than a question waiting to be asked.

---

## 4. A clean checklist does not mean a consistent corpus

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

## 5. The gate decays silently, and nothing says when to re-run it

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

## 6. There is a third disposition, and it is neither owned nor unowned

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

## 7. Three smaller findings

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
