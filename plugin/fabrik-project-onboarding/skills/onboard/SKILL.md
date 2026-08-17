---
name: onboard
description: Interviews someone about a software project as a whole — the problem it solves, who it serves, what is in and out of scope, the hard constraints, and the domain vocabulary — and writes it up as a project constitution that every later feature specification inherits. Use this at the start of a project, before any individual feature is specified, whenever someone says they want to onboard a project, kick off or start a new project, capture project goals or context, define scope, agree terminology, or record the ground rules and non-negotiables that all specs must respect. Also use it when an existing constitution needs revisiting because the project's goals or boundaries have shifted. This is about the software being built — not about configuring folders, instructions, or settings for a Cowork project, which is a different thing that happens to share the word "project".
---

# Project Onboarding

Interview the person about the project as a whole and write the result to `.specify/memory/constitution.md`. Every feature specification written afterwards reads this file, so the work done here stops being re-litigated once per spec.

The value is concentrated in three things: **scope boundaries** (so features don't wander), **non-negotiables** (so constraints don't get discovered late), and **vocabulary** (so ten specs written over three months use one word per concept). Everything else is context that makes the specs read as though one person wrote them.

Run this once at the start of a project. Re-run it when goals or boundaries genuinely shift.

## Who this is for, and what's out of bounds

The person you're interviewing is a product manager, business owner, or domain expert. Assume deep knowledge of the business and none of software construction.

Apply one test to every question before you ask it: **could they answer this from running their own operation?** If answering would need a lawyer, an IT department, or someone who builds software, it's the wrong question — and asking it anyway costs you their confidence as well as the minute. Rewrite it as something they've lived:

| Don't ask | Ask |
|---|---|
| "Are there accessibility standards you must meet?" | "Is there anyone on your team a normal app would lock out — poor eyesight, no reading glasses to hand, colour blindness?" |
| "What are your data residency requirements?" | "Is there anything here you'd be uncomfortable with being stored abroad?" |
| "What systems must this integrate with?" | "Which systems you already pay for would people be annoyed to have to type into twice?" |
| "What's the connectivity like?" | "If someone's standing in the loading bay with no phone signal, does this still need to work?" |
| "What's your data retention policy?" | "If a client asked what you still hold on them from three years ago, what would the answer be?" |

The named jargon in that table isn't a blocklist to memorize — plenty of words that feel like plain English ("connectivity", "sync", "platform", "workflow") fail the same test. Judge each question by whether *they* could answer it, not by whether it contains a flagged word.

This document describes what the software should achieve and what bounds it. It does not describe how it gets built — no architecture, no tech stack, no data design, no testing or delivery process. All of that is deferred to the build stage, and stating it here creates a second source of truth that will drift.

The test for whether something belongs: **would it change what a feature spec says?** A rule that every price is shown inclusive of tax changes specs. A rule about code review does not.

### Separating the need from the solution

Technical opinions will come up, because people who've been burned have views — and those views usually arrive welded to a real constraint. *"Last time the app needed wifi and the kitchen has none, so it has to be a native app, not a website"* is one sentence containing both a hard environmental constraint and a build decision.

Split it. The constraint ("must work with no network in the kitchen") is exactly what topic 5 exists to capture and goes in **Constraints** or a principle; the conclusion ("native app, not a website") goes under **Technical notes**. Filing the whole sentence as an opinion loses the requirement, which is the most damaging thing this interview can do.

Say the split out loud, and say it in a way that keeps their point intact: *"So the hard rule is it has to work with no signal in the kitchen — I'm writing that down as non-negotiable. Your hunch about how to achieve that, I'll pass to the build team as a note rather than a rule, so they can find the best way to meet it."* People accept this readily when they can see the requirement survived; they resent it when it looks like their experience was filed away.

## Before starting

Establish where the finished document goes: the connected folder if there's exactly one, or the one they name if several are connected.

Then settle the **project root**, which every path below is relative to. If a `.specify/` folder already exists at that folder or anywhere above it, the folder containing it is the root — someone may have connected a sub-folder of a project that is already set up, and a second `.specify/` alongside the first would split the project in two. Otherwise the connected folder is the root, and `.specify/` gets created there.

If **no** folder is connected, do not write to a temporary or scratch directory. Say so before you start — "I don't have access to a folder I can save this in, so I'll hand the finished document back to you to download" — and tell them how to connect one so it saves automatically next time. A temporary working directory almost always exists, especially when this runs in the cloud, and writing a 45-minute interview there looks like success while quietly throwing it away. Connected folders are the only durable destination.

If `.specify/memory/constitution.md` already exists, read it and treat this as an amendment rather than a fresh start: show what's currently recorded, ask what's changed, and only reopen the parts that have moved. Bump the version and the amendment date rather than rewriting from scratch. People are far more willing to correct a wrong statement than to answer a blank question.

## The interview

Work through the seven topics below **one at a time**, in order. Each builds on the last — you can't bound scope before you know what the thing is, and you can't write a useful glossary before you've heard them talk.

Two rules that matter more than the questions themselves:

**Propose, don't interrogate.** Once you know enough to draft, lead each topic with a concrete proposal drawn from what they've already said and ask them to correct it. "Here's what I think your user types are — what have I got wrong?" gets better answers in less time than "who are your users?", because reacting is easier than generating.

This technique has one failure mode, and it's serious enough to manage deliberately: **a proposal manufactures an opinion where none existed.** Propose "waste down from 4% to 3%" to someone with no target in mind and you'll get "yeah, call it 3" — and a number you invented is now in a document every spec inherits, indistinguishable from one they cared about. Three habits contain it:

- **Never propose a specific number, name, or threshold they haven't implied.** Propose the *shape* and let them fill it: "you'd want waste down by some percentage over the year — what would make this worth doing?"
- **Seed a deliberate error in list-shaped proposals.** Include one item you suspect is wrong. A proposal that comes back "yeah, that's right" wholesale returned nothing; the correction is the information.
- **Track what they accepted without changing.** If a whole proposal came back unamended, it's yours, not theirs — say so at the end and ask them to confirm it specifically. Mark anything still shaky as uncertain in the file rather than letting it read as settled.

**Play back what you heard in their words.** This document is only useful if they recognize it as theirs. Use their vocabulary, not a normalized version of it — that's the entire point of topic 6.

Expect 30–45 minutes for a first run; topics 5, 6, and 7 carry most of it. Don't rush those three — they're where the value is. If time is short, it's better to stop after topic 6 and schedule the rest than to skim all seven, and it's fine to split this across two sittings. If they're giving short answers throughout, take the hint and draft harder from less: a thin constitution that's correct beats a thorough one they stopped reading.

### 1. What is this and what's broken without it

What the project is, and the problem it exists to solve. Push past the solution to the problem underneath — "we need an app for shift managers" is a solution; "managers can't see waste until the monthly P&L, by which point the money's gone" is a problem, and it's the one that tells a spec author which features matter.

Ask what happens if nothing gets built. The answer usually reveals the real priority.

### 2. Who it serves

The distinct kinds of people who'll use it, what each needs from it, and roughly how many there are. Watch for roles that sound like one group but split under pressure — a "manager" who logs data during service and a "manager" who reviews it on Monday morning want different software, and finding that out here saves an argument in every spec.

Note anyone affected who never touches the software. Their needs show up as requirements regardless.

### 3. What success looks like

Project-level outcomes, not feature-level ones. What has to be true in a year for this to have been worth doing? Push for something countable — a number, a percentage, a time. "Better visibility" isn't checkable; "site managers see yesterday's waste before today's service" is.

Ask what they'd measure to know it's working, and whether that measurement exists today. **"We don't measure that at all" is a better answer than a number they've just made up**, and it belongs in the file — record it alongside the criterion as *(not currently measured)*. It tells the build team that establishing the baseline is part of the job, and it stops a fabricated target being treated as a commitment.

Resist inventing precision here. A criterion the business already tracks is worth three that sound rigorous and rest on nothing.

### 4. What's explicitly not in scope

The highest-value question in the interview and the one people skip. Every project has adjacent things it could grow into; naming them now is what stops a spec quietly annexing one.

Prompt with the obvious neighbours of what they've described, and ask directly: "should this ever do X?" A firm no is worth writing down. A "not in version one" is worth writing down differently — as deferred, with what would bring it back.

Close the topic by stating the boundary from the inside as well: "so this project is about *X*, and everything else stays where it is today — right?" That confirms the **In scope** line rather than leaving you to infer it from what they excluded, and the boundary gets sharper when they hear it drawn in both directions.

### 5. Hard constraints and non-negotiables

Things no feature may violate. Cover each area below, since people rarely volunteer them — but ask in the operator's language, not the category's. The headings are for you; the questions beside them are roughly what to say.

- **Rules they get audited on** — "what would an inspector or auditor pull you up on?" Food safety, allergens, employment law, tax. They'll know these cold; it's their job.
- **Information that's sensitive** — "is there anything here you'd be uncomfortable with the wrong person seeing, or with being stored abroad?" Also: "how far back do you need to be able to look, and is anything you're required to keep?"
- **Where it gets used** — "walk me through where someone actually stands when they'd use this." Kitchens, warehouses, and vans have no signal, wet hands, gloves, noise, and one shared device between six people. An office has none of that, and it's the environment software gets designed for by default.
- **Systems already in place** — "which systems you already pay for would people be annoyed to have to type into twice?"
- **People a default build would lock out** — "is there anyone who'd struggle with this — reading English, eyesight, anything?"
- **Commercial commitments** — "is anything here fixed by a contract, a franchise agreement, or a supplier deal?"

If an answer is genuinely beyond them — data residency and formal accessibility standards often are — don't press. Record what they did say and mark it as needing confirmation from whoever does own it. "Dana assumed UK-only; unconfirmed" is useful; a fabricated requirement is not.

These become the constraints every spec inherits, and they're the ones most expensive to discover late — a constraint found during a build usually invalidates work already done.

### 6. The words the project uses

Capture the domain vocabulary in their words, with a definition for each. This is the single most useful artifact for whoever builds the software, and the person you're interviewing is the only one who can produce it.

You'll have been collecting candidates through the whole conversation — the terms they used without explaining, because to them they're obvious. Play them back and ask for definitions. Chase three things specifically:

- **Terms used interchangeably.** If "site", "store", "restaurant", and "location" have all appeared, ask which one is right. Pick one, record the others as "don't use".
- **Terms that look ordinary but aren't.** Words like "cover", "shift", "waste", or "return" carry precise local meaning that an outsider will silently get wrong.
- **Distinctions that matter.** If two things sound alike but must never be conflated, say so explicitly — that's a bug prevented.

### 7. Principles

Three to seven rules that every feature must honour. Derive them from what's already been said rather than asking cold — people struggle to state principles in the abstract but recognize them instantly.

A good principle is falsifiable and constrains real decisions: "any action a manager takes during service must work with one hand and no network" rules things out. "We value quality" doesn't rule anything out and shouldn't be written down.

When one smells like a slogan — "we're a people business", "quality first" — test it by asking what it would forbid. Put it concretely, since stating a rule's negative space is an abstract task: *"if the build team came back with X, would you send it back?"* If nothing gets sent back, it's a sentiment, and it dilutes the principles that mean something. Don't run this test on principles that already obviously constrain something; you'll just get the principle restated.

Mark any that are genuinely absolute as **NON-NEGOTIABLE**.

## Writing the file

Write `.specify/memory/constitution.md`, creating `.specify/memory/` if needed.

Keep the top-level headings exactly as below — `## Purpose`, `## Core Principles`, `## Product Context`, `## Scope & Constraints`, `## Technical notes`, `## Governance` — even where a section is thin, because that's the shape anything downstream looks for. Sub-headings within them are yours to drop: a project with no glossary worth recording is better off without a `### Glossary` heading than with one holding invented terms.

```markdown
# [PROJECT NAME] Constitution

## Purpose

[Two or three sentences: what the project is, the problem it solves, what happens if it isn't built.]

## Core Principles

### I. [Principle Name]

[What it requires, and what it forbids. Mark NON-NEGOTIABLE where it genuinely is.]

### II. [Principle Name]

[...]

## Product Context

### Who this serves

- **[User type]**: [what they need from it, roughly how many, where and how they use it]

### Success looks like

- [Countable project-level outcome] [*(not currently measured)* where no baseline exists today]

### Glossary

- **[Term]**: [Definition]. [Where relevant: "Not to be confused with X" or "Use this rather than Y, Z".]

## Scope & Constraints

### In scope

- [What this project covers]

### Out of scope

- [What it does not cover] — [firm exclusion, or deferred with what would bring it back]

### Constraints

- **[Category]**: [The constraint, and where it comes from.]

## Technical notes (deferred to build)

*Not binding on specifications. Preferences and prior experience raised during onboarding,
recorded for the build team to weigh.*

- [Whatever was raised, attributed to why they raised it.]

## Governance

This constitution constrains every feature specification in this project. A specification that
conflicts with it is wrong by default — if the constitution is what's wrong, amend it here first
rather than making an exception in a spec.

Amendments are made by the project owner, and this document should be revisited whenever the
project's goals or boundaries shift.

**Version**: [VERSION] | **Ratified**: [YYYY-MM-DD] | **Last Amended**: [YYYY-MM-DD]
```

A first run is version `1.0.0`, ratified and amended on today's date. On a later amendment, bump the version — patch for wording, minor for a new principle or constraint, major for a change that invalidates specs already written — update the amendment date, and keep the original ratification date.

The version scheme is for the document's own bookkeeping. Don't explain it to them unless they ask — say "version 1.0, dated today" and move on. "Ratified" likewise: it means the date they agreed it, nothing more.

## Finishing

Show the finished constitution and ask them to read it once, properly. It's short, they're the only person who can catch what's wrong in it, and everything downstream inherits its mistakes.

Three things to call out by name rather than leaving them to notice:

- **Anything you drafted that they accepted without changing.** Name those items specifically and ask them to confirm each one — they carry your judgement, not theirs, and this is the last cheap moment to catch it.
- **Anything recorded as uncertain**, including answers that need confirming from someone else.
- **Anything deferred**, and what would bring it back.

**Say where it was saved, in words they can act on.** They will go looking for this file, and by default they won't find it:

> "I've saved this in your project folder, inside a folder called `.specify`. The dot at the front means your computer hides it in Finder and File Explorer — that's normal, and nothing's gone wrong. Just ask me to show you the project setup any time and I'll open it for you."

Then point at what's next, as something they can say rather than something they must run: features get written up one at a time — they describe a feature in their own words and ask for it to be turned into a spec — and each one inherits this document automatically, so none of this has to be repeated.
