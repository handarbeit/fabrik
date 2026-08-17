---
name: write-spec
description: Creates a new feature specification from a plain-language feature description, written for a product manager or business owner rather than an engineer, following GitHub Spec Kit's specify workflow. Writes a numbered specs/NNN-short-name/spec.md plus a requirements quality checklist, validates the spec against it, and asks up to three high-impact clarifying questions. Use this whenever someone wants to write, draft, start, or create a spec, specification, feature spec, PRD, or requirements document — and also when they describe a feature they want built and ask to "spec this out", "spec out a feature", "write this up properly", or "turn this into a spec". Trigger it even if they never say the words "spec kit" or "speckit".
---

# Create a Feature Specification

Produce a feature specification a development team can build from, using GitHub Spec Kit's structure and quality bar. The specification describes **what** users need and **why** it matters — never how to implement it.

The person using this is a product manager, business owner, or domain expert — deep knowledge of the business, none of software construction assumed. Write the spec for business stakeholders and keep the conversation in plain language. Never ask them a question only an engineer could answer, and don't use a technical term without defining it in the same sentence. Don't narrate file paths or internal steps as you go — but do say where the spec ended up when you're finished, so they can find it.

## How to ask anything, anywhere in this skill

This governs **every** question below, not just the clarifying questions in step 6:

- **Always lead with a recommendation.** A neutral menu hands the decision back to someone who came here precisely because they don't know the options. Say what you'd do and why, in a sentence, and make agreeing cheap: "say yes and I'll take it."
- **"You pick" / "I don't know" / "which is normal?" is an answer.** Take your recommendation and carry on — never stall waiting for a preference they don't have. Record the result under **Assumptions** as *your* guess, not as their decision.
- **Never invent a specific number, threshold, name, or percentage they haven't given you or clearly implied.** Filling in structure is the job; manufacturing facts is not. See "Writing success criteria" for what to do when a figure is genuinely missing.
- **Batch what you can.** Every separate round-trip costs the person patience they'd rather spend on the spec.

## Where the work goes

Specs live in a folder the engineering team can read. Before writing anything, establish the working folder:

- If exactly one folder is connected to this session, use it.
- If several are connected, ask which one to use — don't say "which one holds the specs", since on a first run none of them does.
- If **no** folder is connected, say so plainly — "I don't have access to a folder I can save this in, so I'll hand these back to you to download" — and hand the files back instead. Everything below still applies: produce all three deliverables, at these relative paths, so they can be dropped into a project intact:
  - `specs/NNN-short-name/spec.md`
  - `specs/NNN-short-name/checklists/requirements.md`
  - `.specify/feature.json` — note this is a *sibling* of `specs/`, not inside the feature folder, so handing back only the feature folder leaves it out

A temporary working directory almost always exists, especially when this runs in the cloud, and treating one as the **destination** looks like success while quietly throwing the work away. That's the prohibition — staging files somewhere in order to hand them back is fine and necessary. What's forbidden is finishing, reporting success, and leaving the only copy somewhere that disappears when the session ends. Also tell the person how to connect a folder, so the next spec saves automatically.

Then settle the **project root**, which is what every path below is relative to. There is exactly one, and it is never two:

- If a `.specify/` folder already exists at the working folder, or anywhere above it, the folder containing that `.specify/` is the project root. Use it. Someone connecting a sub-folder of a bigger project — a single package inside a larger repository — must not get a second `.specify/` alongside the first, and must not get their specs filed somewhere the existing project can't see.
- Otherwise the working folder is the project root, and `.specify/` gets created there.

Paths, relative to the project root:

- `specs/NNN-short-name/spec.md` — the specification
- `specs/NNN-short-name/checklists/requirements.md` — the quality checklist
- `.specify/feature.json` — pointer to the active feature, so the clarify skill and downstream engineering tooling can find it

Because there is one root and everything hangs off it, `feature_directory` is always a plain `specs/NNN-short-name` with no prefix to compute. If you ever find yourself working out an offset between two folders, the root was resolved wrongly — go back and resolve it once, properly.

`.specify/` and `specs/` are GitHub Spec Kit's own directory names. They are fixed by that format and have nothing to do with what this skill is called — never rename them to match the skill, and never invent variants.

## Read the project constitution first

If `.specify/memory/constitution.md` exists, read it before writing anything. It carries the project's scope boundaries, non-negotiable constraints, and agreed vocabulary, and this spec inherits all three:

- **Use its glossary terms exactly.** If it says "site", write "site" throughout — never "store" or "location". Consistent vocabulary across a set of specs is most of what makes them usable together.
- **Treat its constraints as settled.** Don't re-ask them, and never spend one of your three clarification markers on something it already answers.
- **Check the feature against its scope boundaries.** If the description asks for something the constitution lists as out of scope, raise it before writing rather than after — that's a conversation about the project, not about this spec.

If no constitution exists, say so **before** writing, not after — by the time the spec is written the vocabulary and scope boundaries have already been invented, and this first spec is usually the one that matters most. Offer the choice plainly and in one breath: setting the project up first takes about half an hour and every later spec inherits it, or you can write this spec now and set the project up afterwards.

If they'd rather press on, carry on without a constitution — but at the end, tell them this spec was written before the project's shared vocabulary and boundaries existed, and that once the project is set up they can ask for this spec to be gone over again to reconcile the two.

## Steps

### First gate: is this a new feature, or a change to one that exists?

Before anything else — before naming, numbering, or creating a folder — decide which of the two you're being asked for.

If the request **modifies a spec that already exists** — a different priority, an extra user story, a term used wrongly, a requirement that isn't right — go straight to "Revising a spec you already wrote" below and **do not allocate a number**. "Add a user story about ordering", "make the ordering one P1, not P2", "that's not what I meant by site" are all revisions, and they arrive far more often in a later session than in the one that wrote the spec.

Only continue into step 0 when they're describing a genuinely different feature.

### 0. Pre-flight: one feature, and the two things worth knowing first

Ask these together, in one message, each with a recommendation — not as three sequential round-trips. If the answer to all of them is obvious from the description, ask nothing and carry on.

**Is this one feature?** Create exactly one feature per invocation. Before generating a name or creating any folder, check whether the description actually describes one feature or several.

The test is **not** whether the parts are related — three things can all concern food waste and still be three features. The test is whether they could ship separately and be prioritized against each other. If each part could go live on its own and someone might reasonably want one before another, they're separate features.

If it's several, say so in the person's own words and ask which comes first — "that's really three things: recording what gets binned, advising on over-ordering, and comparing sites against each other. Which matters most right now?" — then spec that one and offer to do the others afterwards.

Do this before creating anything on disk. A folder created for the wrong feature burns its number permanently (numbers never get reused, see step 2) and leaves `.specify/feature.json` pointing at a directory nobody wanted.

**Where is this used, and on what?** Never silently default the physical environment. Where is the person standing, and on what device, when they use this? A kitchen at the end of service means no signal, wet or gloved hands, noise, and one shared screen on a wall — and a spec that quietly assumes an office desk and a personal phone will be wrong in ways no checklist item catches. Recommend the reading you'd draw from their description and let them correct it. Skip this only when a constitution already answers it.

**Is there a project set up?** If no constitution exists, this is where you say so — see "Read the project constitution first" above for what to offer. Fold it into the same message.

If the feature description is empty, ask for it rather than inventing one.

### 1. Generate a short name

Extract a 2–4 word kebab-case name from the description. Prefer action-noun form; preserve technical terms and acronyms.

- "I want to add user authentication" → `user-auth`
- "Track waste from prep through service across all sites" → `waste-tracking`
- "Let head office compare waste across all sites" → `cross-site-comparison`

### 2. Create the feature folder

Scan `specs/` for existing `NNN-*` folders and take **one higher than the highest number present**, zero-padded to three digits — not the lowest unused one. Gaps left by deleted or renamed features stay as gaps, so the numbers keep telling you the order features were specified. If `specs/` doesn't exist, or exists with no `NNN-*` folders in it, start at `001`.

Create `specs/NNN-short-name/` and `specs/NNN-short-name/checklists/`, then write `.specify/feature.json` pointing at the folder you just made:

```json
{ "feature_directory": "specs/001-waste-tracking" }
```

This file names the *active* feature, so overwrite `feature_directory` if it already exists — but preserve any other keys in the file rather than replacing the whole thing, since other tooling may keep state there.

`feature_directory` is always relative to the project root settled in "Where the work goes" — a plain `specs/NNN-short-name`, exactly as the example shows, with nothing prefixed to it. Downstream tooling resolves this path from the project root, and since `.specify/` sits at that root by construction, the two cannot disagree.

### 3. Write the specification

Read `references/spec-template.md` and use it as the structure. Preserve section order and headings, and replace every bracketed placeholder with concrete content. Optional sections that genuinely don't apply get removed entirely rather than left as "N/A"; the sections marked `*(mandatory)*` are always kept and always filled.

`## Assumptions` is never removed, however detailed the description was, and even though the template carries no `*(mandatory)*` marker on it. It's the section that makes informed guessing safe, and the checklist item *"Dependencies and assumptions identified"* depends on it. Leave the template's markers exactly as they are — don't add a `*(mandatory)*` marker to Assumptions, since that would diverge from upstream.

The template is scaffolding, and none of its instructions to *you* should survive into the finished spec. Remove all of these:

- `<!-- ... -->` guidance comments
- the `*Example of marking unclear requirements:*` line and the two illustrative FRs beneath it — they're samples, and leaving them in collides with your real requirement numbering
- bracketed authoring notes like `[Add more user stories as needed, each with an assigned priority]`
- any `---` rule left stranded by content you removed. The rules separate user stories, so keep one between each pair of stories you actually write and drop the rest

Keep `*(mandatory)*` on the headings that carry it — the quality checklist checks that those sections are complete, so the marker has to survive. Drop conditional annotations like `*(include if feature involves data)*`: they're instructions about whether to include the section, and once you've decided to include it, they've done their job.

Two title-block lines have a shape downstream tooling reads, so keep it exactly even though the content changes. The template has:

```
**Feature Branch**: `[###-feature-name]`
**Input**: User description: "$ARGUMENTS"
```

Written out, with a real name and the person's own words, those become:

```
**Feature Branch**: `001-waste-tracking`
**Input**: User description: "Track waste from prep through service across all sites"
```

The backticks around the branch name stay, and so do the `User description:` prefix and the quotes around it.

Fill the header fields: feature name, `**Feature Branch**` set to `NNN-short-name`, today's date as `YYYY-MM-DD`, status `Draft`, and the person's original words in `**Input**`. Use `YYYY-MM-DD` for every date this skill writes, so the spec and the clarify skill's session headings agree.

Then work in this order. This is the order to *author* in, not the order sections appear in the document — the template's own section order is authoritative, and Key Entities belongs under Requirements, above Success Criteria:

1. Extract key concepts — actors, actions, data, constraints.
2. Write **User Scenarios**, prioritized P1, P2, P3. Each story must be independently testable: if only that one story shipped, it would still deliver real value. This is what lets engineering slice the work into something shippable.
3. Write **Edge Cases**. Ask yourself, in the person's own domain, what happens when someone forgets, does it twice, does it out of order, or stops halfway — and what happens when the thing being recorded doesn't fit the categories on offer. The checklist scores this section, so it has to be written deliberately rather than back-filled when a check fails.
4. Write **Functional Requirements** — each testable and unambiguous.
5. Write **Success Criteria** — measurable and technology-agnostic (see below).
6. Identify **Key Entities** if the feature involves data.
7. Record every default chosen under **Assumptions**. This is where informed guesses become visible and correctable, which is what makes guessing safe.

**Never invent a specific number, threshold, name, or percentage the person hasn't given you or clearly implied.** Filling in structure is the job; manufacturing facts is not. A target like "cuts waste by 15% within six months" put into someone's mouth reads as their commitment and gets quoted back at them later — and if they never said they measure waste at all, it's fiction with a number attached.

When a success criterion needs a figure nobody has supplied, write the *shape* of the measure and mark the figure as missing — or pick a criterion the business already tracks. One criterion resting on a number they actually know beats three that sound rigorous and rest on nothing. Written into the spec, without backticks, it reads:

> Reduces food waste across sites by [NEEDS BASELINE]% within [NEEDS BASELINE] months *(not currently measured)*

`[NEEDS BASELINE]` is not a `[NEEDS CLARIFICATION]` marker: it doesn't count against the cap of three, it doesn't block the *"No [NEEDS CLARIFICATION] markers remain"* item, and a criterion carrying one still satisfies *"Success criteria are measurable"* as long as the shape is measurable.

### 4. Handle genuine unknowns

Fill gaps with informed guesses from context and industry standards, and document them under Assumptions. Reserve `[NEEDS CLARIFICATION: specific question]` markers for decisions that truly can't be defaulted:

- the choice significantly changes scope or user experience, **or**
- multiple reasonable readings exist with materially different implications, **and**
- no sensible default exists.

Cap this at **three markers**. More than three usually means guesses are being avoided rather than real gaps surfaced — prioritize by scope > security/privacy > user experience > technical detail, and default the rest. Every question spent on something guessable is a question not spent on something that matters.

Things with obvious defaults, which shouldn't be asked about, stated as business needs rather than technical choices: data retention ("records kept for the current financial year" — the industry norm for the domain), responsiveness ("fast enough not to slow someone down mid-task"), and error handling ("a plain message explaining what went wrong and what to do next").

Don't default anything that names a technology. Authentication *methods*, integration *patterns*, storage and hosting choices are implementation, and an assumption recording one will fail the checklist item *"No implementation details (languages, frameworks, APIs)"* in step 5 and have to be deleted again. Record the need — "only staff on shift at that site can enter waste for it" — never the mechanism.

**The physical environment of use is never silently defaulted** — but it's asked in step 0's pre-flight, before the spec is written, not here. If you reach this point without knowing it, that's a step 0 you skipped.

### 5. Create and run the quality checklist

Write `specs/NNN-short-name/checklists/requirements.md` from the block below, replacing `[FEATURE NAME]` and `[DATE]` with the real values. Every other bracketed string in the block — notably `[ ]`, `[x]`, and the literal `[NEEDS CLARIFICATION]` inside a checklist item — is part of the checklist's own text and stays exactly as written.

```markdown
# Specification Quality Checklist: [FEATURE NAME]

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: [DATE]
**Feature**: [../spec.md](../spec.md)

## Content Quality

- [ ] No implementation details (languages, frameworks, APIs)
- [ ] Focused on user value and business needs
- [ ] Written for non-technical stakeholders
- [ ] All mandatory sections completed

## Requirement Completeness

- [ ] No [NEEDS CLARIFICATION] markers remain
- [ ] Requirements are testable and unambiguous
- [ ] Success criteria are measurable
- [ ] Success criteria are technology-agnostic (no implementation details)
- [ ] All acceptance scenarios are defined
- [ ] Edge cases are identified
- [ ] Scope is clearly bounded
- [ ] Dependencies and assumptions identified

## Feature Readiness

- [ ] All functional requirements have clear acceptance criteria
- [ ] User scenarios cover primary flows
- [ ] Feature meets measurable outcomes defined in Success Criteria
- [ ] No implementation details leak into specification

## Notes

- Items marked incomplete require spec updates before clarification or planning
```

The sixteen checklist items are kept word-for-word identical to upstream Spec Kit, in the same order, so the file means the same thing to anyone downstream. Don't add, remove, reword, or reorder them. Two things outside the graded items differ from upstream deliberately: the `**Feature**` line links to the spec rather than carrying upstream's bare placeholder, and the `## Notes` bullet says "before clarification or planning" where upstream names its slash commands — this reader has no way to run those. Four items need a consistent reading:

- *"No implementation details leak into specification"* (Feature Readiness) and *"No implementation details (languages, frameworks, APIs)"* (Content Quality) are differently worded but test the same thing. Evaluate once, tick both the same way. The denominator stays 16 — it counts checkboxes, not independent judgments.
- *"Feature meets measurable outcomes defined in Success Criteria"* can't mean the built feature at this stage, since nothing is built. Read it as: every success criterion is attributable to a user story in this spec — no criterion measures something the spec doesn't deliver, and no story ships with nothing measuring it.
- *"All functional requirements have clear acceptance criteria"* — the template attaches acceptance scenarios to user stories, not to individual requirements. Read it as: every functional requirement is exercised by at least one acceptance scenario somewhere in the spec. A requirement no scenario touches is one nobody can sign off.
- *"Success criteria are measurable"* is satisfied when the **shape** of the measure is measurable, even where the figure itself is still `[NEEDS BASELINE]`. Tick it, and name the outstanding baselines in the Finishing report instead. The alternative — treating it as a failure — has exactly one available repair, which is inventing the number you're forbidden to invent, so it would burn all three repair passes and still fail.

Evaluate each item against what was actually written, quoting the relevant spec text to yourself as evidence rather than trusting intent. A checklist ticked from memory of having meant to do the thing is worth nothing.

**Write the result into the file.** Save `requirements.md` with `[x]` against every item that passes and `[ ]` against every item that doesn't. The checkbox state is durable state, not a note to yourself: the clarify skill compares the marks before and after its own run, and downstream tooling reads them as a gate. A checklist left entirely unticked reads as a spec that failed every check.

Work the results in this order:

1. **Fix what's fixable first.** For every failing item other than the clarification-marker one, correct the spec and re-check. Allow up to three passes. Doing this before asking questions matters — the questions in step 6 should be about the spec as it now stands, not a draft you already know is wrong.

   Keep the running record of what failed and what you changed in your **reply to the person**, not in the file. Upstream's `## Notes` section is a single fixed bullet that downstream tooling reads as part of the gate; appending a repair log to it changes a machine-read file into a scratchpad. Leave the `## Notes` section exactly as the block above has it.
2. **Then handle the markers.** Leave *"No [NEEDS CLARIFICATION] markers remain"* unticked and go to step 6. This item is expected to fail on the first pass — leaving up to three markers is the design, not a defect — and it gets ticked once the answers are written in. Report the count honestly (e.g. "15 of 16, pending your answers below") rather than pre-ticking it.
3. **If everything passes and no markers remain**, skip step 6 entirely and go straight to Finishing.

### 6. Ask the clarifying questions

Present all outstanding questions together (maximum three, numbered Q1–Q3), then wait for answers to all of them. Batching them respects the person's time and lets them see the whole decision surface at once.

Every question must be answerable from the person's own knowledge of how their business runs. If answering it would require knowing how software gets built, it isn't a question for them — default it and record the default under Assumptions instead.

Take a position. A neutral menu hands the decision back to someone who came here precisely because they don't know the options, and "you pick" is the most likely reply. Recommend one, say why in a sentence, and make agreeing the cheapest possible action:

```markdown
**Q1: [Topic]**

In the spec right now: "[quote the relevant line]"

**[The specific question, in plain language]?**

**Why it matters**: [one sentence on what changes depending on the answer]

| Option | Answer | What it means |
|--------|--------|---------------|
| A      | [First suggested answer] | [What this means for the feature] |
| B      | [Second suggested answer] | [What this means for the feature] |
| C      | [Third suggested answer] | [What this means for the feature] |
| Custom | Something else | Tell me in your own words |

**Recommended: [X] — [why, in one line].** Say "yes" and I'll take it.
```

Keep the tables well-formed — spaces around cell content, at least three dashes in the separator row — so they render.

Number the questions `Q1`, `Q2`, `Q3` and use that form in the heading too, so what you call them matches what they're labelled.

Handle the three realistic replies:

- **An answer, or "yes" / "the recommended one"** — take it, and record it as the person's decision.
- **"You pick" / "I don't know" / "which is normal?"** — take your recommendation, and record it under **Assumptions** as your guess, not under Clarifications as their answer. The distinction matters: the next person to read the spec needs to know which decisions the business actually made.
- **"Not yet"** — leave the markers in place and say plainly which decisions are still open and that they can be settled later without redoing anything.

When answers come back, replace each marker with the chosen answer, save the spec, and re-run the checklist — including the marker item, which should now pass.

A `[NEEDS BASELINE]` figure the business plausibly knows is a good candidate for one of your three questions — ask what they currently measure. If they don't track it at all, leave the marker, record it under Assumptions, and say so at the end; that's a real answer, not a gap.

Record the exchange in the spec under `## Clarifications`, placed after the title block and immediately before the first `##` section, with a `### Session YYYY-MM-DD` subheading — the same placement the clarify skill uses, so the two maintain one consistent section rather than two competing ones. One bullet per question **the person actually answered**, in the form `- Q: <question> → A: <answer>`. A question they handed back to you produces an Assumptions entry and no Clarifications bullet — that distinction is the whole point, and without it a decision the business actually made is indistinguishable from a guess.

## Revising a spec you already wrote

"Add a user story about ordering", "make the waste one P1, not P2", "that's not what I meant by site" — these are edits to the spec that already exists, not requests for a new one.

Find the spec the change belongs to — `.specify/feature.json`'s `feature_directory` if it covers the feature being changed, otherwise the `specs/NNN-*` folder that does, asking which they mean if it's genuinely unclear. Session and conversation are irrelevant: a revision the next morning is still a revision.

Edit that spec in place: make the change, re-run the checklist, and report what moved. Do **not** allocate a new `NNN` number, create a second folder, or repoint `feature.json` — that burns a number, splits one feature across two specs, and leaves the person's earlier answers behind in the first one.

Only start a new spec when the person is describing a genuinely different feature.

## Writing success criteria

Success criteria are the part most specs get wrong. They must be verifiable by someone who knows nothing about the implementation.

Good — measurable, user-facing, technology-agnostic:

- "Users can complete checkout in under 3 minutes"
- "95% of searches return results in under 1 second"
- "Cuts end-of-shift reconciliation time by 40%"

Bad — leaks implementation:

- "API response time under 200ms" → say "users see results instantly"
- "Database handles 1000 TPS" → state the user-facing volume instead
- "Redis cache hit rate above 80%" → not a user outcome at all

## Staying out of implementation

The spec answers what and why. It doesn't name languages, frameworks, databases, APIs, or code structure. When the description already contains technical choices, capture the *need* behind them in the spec and note the stated preference under Assumptions — the constraint survives without the spec turning into a design document.

Don't embed extra checklists inside the spec. `checklists/requirements.md` is the only one this creates.

## Finishing

Report back in plain language:

- **A one-line summary** of what the spec covers.
- **Checklist status**, as a count of what genuinely passes right now — "16 of 16 quality checks passing", or "15 of 16 — the last one clears once you've answered the questions above".
- **Where it was saved**, in words they can act on. Name the folder, and warn them about the hidden one: "it's in your project folder under `specs/001-waste-tracking`. There's also a folder called `.specify` holding the project's settings — the dot at the front means your computer hides it in Finder and File Explorer, so don't worry if you can't see it. Ask me any time and I'll open either for you." If there was no folder to save into, say that instead — what you're handing back, and how to connect a folder so next time it saves by itself.
- **The assumptions, in the chat, not just in the file.** List every default you chose, flagged as yours: *"These are my guesses, not your words — tell me any that are wrong."* Call out separately any figure left as `[NEEDS BASELINE]`, and say plainly that it's waiting on a number they'd have to know. This is the whole reason guessing is safe rather than reckless, and it doesn't work if it lives in a file nobody opens. Keep it short — a handful of bullets, not the full section pasted back.
- **What happens next**, described as something they can say rather than something they must run: "if you want, I can go back over this and tighten anything vague — just ask", or "this is ready to hand to whoever's building it." Never tell them to run a command, a skill, or a stage; they have no way to start one.

Say plainly that the spec is a draft meant to be argued with, and that changing it later costs nothing.
