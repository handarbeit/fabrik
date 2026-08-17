---
name: clarify
description: Finds the vague and missing parts of an existing feature specification and fixes them by asking up to five targeted questions, one at a time, writing each answer back into the spec as it is answered. Follows GitHub Spec Kit's clarify workflow. Use this when someone wants to review, tighten, sharpen, sanity-check, poke holes in, or fill the gaps in a spec or requirements document, when they ask "what's missing from this spec", "is this ready to hand to engineering", or "what did I forget" — and as the natural follow-up after a spec has been written.
---

# Clarify a Specification

Find where a specification is ambiguous or silent, ask the smallest number of questions that resolve the highest-impact gaps, and write every answer back into the spec so the document — not the chat transcript — carries the decision.

This runs after a spec exists and before anyone builds against it. Ambiguity caught here costs one question; ambiguity caught during the build costs a rebuild.

The person answering is a product manager, business owner, or domain expert — expert in the business, not in software construction. Every question must be answerable from what they know about their own operation. A question they'd need an engineer to answer is a question you shouldn't be asking.

**Never invent a specific number, threshold, name, or percentage they haven't given you.** Turning a vague adjective into a number is the single most valuable thing this skill does — but the number has to come from them. Proposing "under two seconds" to someone with no target in mind produces agreement, not information, and a figure you made up is then indistinguishable in the spec from one the business actually committed to. Ask what they'd expect, or what they measure today; if they genuinely don't know, record the shape and leave the figure as `[NEEDS BASELINE]`.

`[NEEDS BASELINE]` is a sanctioned marker meaning "not currently measured", written by this skill and by write-spec. Resolve it only with a figure the person supplies. Never fill one in yourself, and never treat one as a lingering vague placeholder to be cleaned up.

## Find the spec

Locate the specification in this order:

0. **If the person attached a spec to the conversation**, work from that and hand back an updated copy when you're done. Everything below about reading and writing files applies to that working copy instead. Check this first — it costs nothing and saves hunting for a file that was handed to you.
1. Otherwise establish which folder holds the spec: use the connected folder if there's exactly one, and ask which if several are connected. If **no** folder is connected, don't go looking in a temporary or scratch directory — ask them to attach the spec to the conversation instead, and tell them how to connect a folder so it's found automatically next time.
2. Settle the **project root**: if a `.specify/` folder exists at that folder or anywhere above it, the folder containing it is the root; otherwise the connected folder is. Everything below is relative to that root, and `feature_directory` is read relative to it — someone may have connected a sub-folder of a larger project whose `.specify/` sits higher up.
3. `.specify/feature.json` at the root — read `feature_directory`, and use `<feature_directory>/spec.md`. If the file can't be read, or the folder it names no longer exists, ignore it and fall through to the next step. Don't report that as an error — someone renaming a folder in Finder is a perfectly ordinary thing to do, and they have no idea a file was tracking it.
4. If that's missing or stale, list the `specs/NNN-*` folders under the root and use the highest-numbered one, confirming with the person which spec they mean if there's any doubt. Once resolved, quietly update `.specify/feature.json`'s `feature_directory` to point at the folder you settled on, preserving any other keys — leaving a stale pointer in place means every later run repeats this search, and it silently breaks write-spec's ability to revise a spec rather than starting a new one.
5. If no spec file exists at all, don't create one — say plainly that there's no spec here yet and offer to write one with them first.

Read `.specify/memory/constitution.md` too, if it exists. It settles project-wide vocabulary, constraints, and scope — so anything it already answers is not an ambiguity, and asking about it wastes one of five questions and makes the project look like it isn't listening to itself. It also gives you the canonical terms to normalize toward when you find the spec using two words for one thing.

## Steps

### 1. Scan for ambiguity

Read the whole spec and assess it against the coverage taxonomy in `references/taxonomy.md`. Mark every category **Clear**, **Partial**, or **Missing**. Keep this map to yourself while questioning — leading with a wall of categories buries the questions — but keep it, because the final report presents it back as the coverage summary, translated into plain language (see Reporting). The taxonomy's category names are working vocabulary for you, never text to show the person.

For each Partial or Missing category, consider a question — but drop candidates where the answer wouldn't change what gets built, tested, or accepted, and drop ones better answered during technical planning. Note the latter internally so they can be reported as deferred rather than silently dropped.

### 2. Build the question queue

Assemble a prioritized queue of at most **five** questions. Each must:

- be answerable either by picking from 2–5 mutually exclusive options, or in five words or fewer
- materially affect architecture, data modeling, task breakdown, test design, user-facing behavior, operational readiness, or compliance
- be answerable from what this person actually knows. A question whose real answer depends on facts they don't hold — whether some data is already collected, whether a system can supply a field — isn't a clarification, it's a research task. Either frame it as a preference they can express from their own experience, or note it as deferred to planning

Rank by impact × uncertainty and cover the highest-impact unresolved categories first. Don't spend two of five questions on low-impact details while something like security posture stays unresolved. Skip anything already answered elsewhere in the spec, anything purely stylistic, and plan-level execution detail unless it blocks correctness.

If nothing meets the bar, say "No critical ambiguities detected worth formal clarification", show a compact coverage summary, and suggest moving on. That's a real outcome, not a failure.

### 3. Ask, one question at a time

Present exactly one question, wait for the answer, then move on. Don't reveal what's queued next — seeing five questions at once invites skimming, and each answer can change what's worth asking afterward. Saying where you are ("question 2 of up to 5") is fine and worth doing; it costs nothing and tells the person the end is in sight.

**Write questions a non-technical stakeholder can answer unaided:**

- Lead with `**Question:**` followed by a full sentence that ends in `?` and stands on its own. A topic label like `Acceptance device/runtime matrix (FR-023)` is not a question — it's a heading, and it puts the work of guessing the question on the reader.
- The only thing allowed after the `?` is a parenthesized requirement id: `**Question:** <question>? (FR-023)`. Never lead with the id. The first time you use one in a session, gloss it once — "FR-023 is just the numbered line in the spec this would change" — then use it bare after that.
- Immediately after the question, add one plain-language **Why it matters** sentence naming the stake — what goes wrong at acceptance or at ship time if this stays undecided.
- Use everyday words. Introduce jargon only if you define it in the same sentence. Self-check: could someone who has never heard of Spec Kit answer from the question line alone?

**Multiple-choice format** — analyze the options first and take a position, based on best practice for the project type, common patterns in similar work, risk reduction, and any goals or constraints already visible in the spec. Lead with the recommendation, because an expert opinion is more useful than a neutral menu:

```markdown
**Recommended:** Option B — <one or two sentences of reasoning>

| Option | Description |
|--------|-------------|
| A | <description> |
| B | <description> |
| C | <description> |
```

Then: `You can reply with the option letter (e.g., "A"), accept the recommendation by saying "yes" or "recommended", or give your own short answer.`

**Short-answer format** — when there are no meaningful discrete options:

```markdown
**Suggested:** <proposed answer> — <brief reasoning>
```

Then: `Format: Short answer (≤5 words). Accept the suggestion by saying "yes", or give your own answer.`

**Handling the reply:** "yes", "recommended", or "suggested" means take your stated recommendation. Otherwise check the answer maps to an option or fits the five-word limit. If it's ambiguous, ask for quick disambiguation — that stays part of the same question and doesn't advance the count.

The five-word limit governs what the person has to *type*, not what gets recorded. A terse reply plus your stated recommendation resolves into a full decision, and it's that decision — precise enough to build and test against — that goes into the spec. "B" is a fine answer; "B" is not a fine requirement.

**Stop early** when the remaining queued questions have become unnecessary, when the person signals they're done ("done", "good", "no more", "stop", "proceed"), or when five questions have been asked. Retries on a single question don't count as new questions.

### 4. Integrate each answer immediately

After each accepted answer, update the spec — on disk, or in the working copy you'll hand back if the spec was attached to the conversation — before asking the next question. Saving incrementally means an interrupted session still leaves the spec better than it was.

On the first integration of the session, ensure a `## Clarifications` section exists — placed after the title block and immediately before the first `##` section, whatever that section happens to be — with a `### Session YYYY-MM-DD` subheading for today. Append the exchange:

```markdown
- Q: <question> → A: <final answer>
```

One bullet per question, carrying the resolved decision. A question that needed a disambiguation round still produces one bullet, with the final answer rather than the exchange that got there.

Then apply the answer where it actually belongs:

| Kind of answer | Where it goes |
|---|---|
| Functional ambiguity | Update or add a Functional Requirement |
| Role or actor distinction | Update User Scenarios / the relevant actor description |
| Data shape, entity, relationship | Update Key Entities, preserving existing order |
| Non-functional constraint | Add or sharpen a measurable item under Success Criteria — turn the vague adjective into a number *the person gave you* |
| Edge case or failure mode | Add a bullet under Edge Cases |
| Terminology conflict | Normalize the term throughout; keep the old one only as `(formerly referred to as "X")`, once |

When a clarification invalidates an earlier statement, **replace** that statement rather than adding a contradicting one. A spec that says two things is worse than a spec that said nothing. Before moving on, search the whole document for the vague term the answer just retired — "normalised basis", "appropriate", "as needed" — because the phrase you resolved in a requirement usually also appears in an acceptance scenario or an entity description, and a spec that's precise in one place and vague in another is still vague.

If an answer requires a **new** functional requirement, give it the next unused FR number and place it with the requirements it relates to rather than appending it to the end — grouping survives longer than numeric order. Add at least one acceptance scenario covering it in the same edit. New requirements are the main way a clarify pass makes the quality checklist worse than it found it, and the fix costs one sentence at the time you already have the context.

Preserve heading hierarchy, don't reorder unrelated sections, and keep each insertion minimal and testable rather than narrative.

### 5. Validate

On each write, check the two things that write could have broken: one bullet per accepted answer with no duplicates, and no statement left contradicting the answer just integrated.

Once at the end, confirm the rest: no more than five accepted questions; no lingering vague placeholder that an answer was meant to resolve; valid markdown with no new headings beyond `## Clarifications` and `### Session YYYY-MM-DD`; and consistent terminology across every section touched.

### 6. Re-validate the quality checklist

If `<feature_directory>/checklists/requirements.md` exists, re-evaluate it against the updated spec. If it doesn't, skip this silently.

Read the file and find the checkbox lines — `- [ ]`, `- [x]`, `- [X]`, tolerating leading whitespace, ignoring anything inside code fences. Record the before state, re-evaluate each item against the spec as it now stands, and flip only the markers whose state actually changed: unchecked → `[x]` when an item now passes, checked → `[ ]` when it now fails. Leave unchanged markers exactly as they were, including their case, and don't touch headings, notes, ordering, or whitespace — a clean diff is what makes the change reviewable.

Track three lists for the report: newly passing, regressions, and still unchecked. Record before/after counts as checked-over-total.

## Reporting

**Nothing in this report uses the taxonomy's internal category names — deferrals included.** Use the plain-language gloss from `references/taxonomy.md` everywhere, not just in the coverage summary. "Deferred: Protocol and versioning assumptions" is exactly the leak this rule exists to prevent.

Close with:

- how many questions were asked and answered
- the path to the updated spec
- which sections were touched
- checklist status if re-validated, as before → after counts (e.g. "12/16 → 15/16 items passing"), naming anything newly passing, any regression, and anything still unchecked
- a short coverage summary **in plain language**. "Observability", "Protocol and versioning assumptions", "Rate limiting and throttling" are engineering vocabulary and mean nothing to the person reading. Group what you assessed into these seven business-readable headings, and mark each **Resolved**, **Deferred**, **Already clear**, or **Still open**:
  - what it does
  - who uses it
  - the information it keeps
  - who's allowed to see and do what, and any rules you have to follow
  - how it behaves when things go wrong
  - how fast and reliable it has to be
  - the words it uses
- a recommendation in plain terms: either "this is ready to hand to whoever's building it", or "worth another look once you've decided X", naming what would need to be true. Describe it as something they can ask for, never as a command, a skill, or a stage to run — they have no way to start one.

Name anything deferred explicitly with the reason. A gap that's been consciously deferred is a managed risk; a gap that quietly vanished from the report is not.

If the person says they're skipping clarification entirely — an exploratory spike, say — that's their call. Proceed, but note once that unresolved ambiguity tends to resurface as rework.
