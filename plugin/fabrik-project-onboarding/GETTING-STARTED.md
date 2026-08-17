# Getting started

You don't need to install anything technical, use a terminal, or know git. You need the **Claude desktop app**, on a computer, and about five minutes of setup. The phone app won't work for this — it can't connect to a folder, and your specs need somewhere to be saved.

A **spec** is a written description of what a piece of software should do and why, detailed enough that someone can build it without guessing. That's what you're going to produce here.

## 1. Install the plugin

In the Claude desktop app, click **Cowork**, then **Customize** in the sidebar and open **Plugins**.

1. Select **Add marketplace** and enter: `handarbeit/fabrik`
2. Find **Fabrik Project Onboarding** in the list and click **Install**.

You don't need a GitHub account for this — it's a public repository, so it just loads.

When a new version ships you'll see an **Update** button in the same place.

## 2. Make a project and connect a folder

Your specs are written to a real folder on your computer, so Claude needs access to one.

If someone's already building the software, ask them which folder to use — often a shared
one that syncs to them automatically, so there's no separate step to send anything. If it's
just you for now, make a new folder anywhere you like (Documents is fine) and use that. It
can be shared with a developer later without moving anything.

A **project** in Cowork is just a saved setup: which folders Claude can use, plus any
standing instructions. Making one means you don't reconnect the folder every time.

1. Click **Projects** in the left sidebar, then the **+** button.
2. Choose a starting point:
   - **Use an existing folder** if the folder is already on your computer — this is
     the usual case.
   - **Start from scratch** if nothing has been set up yet; it creates a new folder
     for you.
3. Give it a name and a short description.
4. Confirm the folder is attached. You can add more folders later from the project's
   settings.

From then on, click the project in the sidebar to start a session with that folder
already connected. Projects live only on your computer.

## 3. Onboard the project (once)

Start here, before writing any individual feature:

> Let's set up the project.

You'll be asked about seven things, one at a time — what the project is and what's broken without it, who it serves, what success looks like, what's explicitly out of scope, the hard constraints, the words your business uses, and the rules every feature has to respect. Expect thirty to forty-five minutes, and it's fine to do it in two sittings.

Most of it is you correcting drafts rather than answering from a blank page. If a proposal is wrong, say so — that's faster than being asked open questions, and it's what the format is designed around.

You'll get a short document at the end. **Read it properly.** Every feature spec inherits it, including its mistakes, and you're the only person who can spot what's wrong.

## 4. Write a spec, one feature at a time

Describe the feature in your own words, with however much detail you have:

> I want to spec out a feature for tracking food waste — spoiled deliveries, prep offcuts, plate returns — across all our sites, so we can see where the money is going.

You'll get back a written specification, plus at most three questions about things that genuinely couldn't be guessed. Everything else gets a reasonable default, written down under **Assumptions** so you can correct anything wrong.

The spec lands in the folder you connected, as `specs/001-your-feature/spec.md`.

You'll notice folders called `specs` and `.specify`. Those are just the standard names for where specifications and project settings live — nothing you need to press or remember. The one starting with a dot is hidden by your computer by default, so don't be alarmed when you can't see it; ask Claude to open it for you.

## 5. Sharpen it

When you're happy with the shape:

> Clarify this spec.

Up to five questions, one at a time, each with a recommended answer. If the recommendation looks right, just say "yes". Each answer gets written into the spec as you go, so decisions live in the document rather than the chat.

Then go back to step 4 for the next feature.

## What you're being asked for (and what you're not)

**You're being asked for:** what the software should do, who it's for, why it matters, and how anyone would know it's working. That's the whole job, and you're the person who knows it.

**You're not being asked for:** how it gets built. No databases, frameworks, or architecture — that's deferred to the build stage. If you have a strong technical opinion, say it anyway; it gets recorded for the build team rather than thrown away.

Two things are worth knowing, because they're where specs usually go wrong:

**Say what "good" looks like in numbers.** "The system should be fast" can't be checked by anyone. "A manager can log a waste entry in under 30 seconds" can. Every vague adjective — robust, intuitive, seamless — is a decision someone downstream will end up making on your behalf. If you can put a number on it, you've made that decision yourself.

**Rank the user stories honestly.** A *user story* is one thing someone needs to be able to do, written so it could ship on its own. P1 means "must be in the first version"; P2 and P3 can follow later. If everything is P1, nothing is, and the build order gets chosen by someone with less context than you.

## If something goes wrong

- **You can't find your spec** — it's in the `specs` folder, which is *not* hidden: look for `specs/001-your-feature/spec.md` in the folder you connected. If the whole folder looks empty, see the next entry.
- **You can't find the project setup** — that one *is* hidden. The constitution written during setup lives in a folder whose name starts with a dot (`.specify`), and your computer hides those by default. It's almost certainly there. Ask Claude to show you the project setup rather than hunting for it in Finder or File Explorer.
- **It genuinely didn't write anything** — this happens when no folder was connected to the session, so there was nowhere to save to. Claude should tell you and offer the files as a download instead. Connect a folder (step 2) and ask again, and it'll save automatically from then on.
- **You want to change something after the fact** — just say so. "Add a user story about ordering" or "the waste one should be P1, not P2" works fine, and it doesn't have to be the same conversation or even the same day; the existing spec gets edited rather than a new one started.
- **The project's goals have shifted** — ask to update the project setup. It'll show you what's recorded and only reopen what's changed.
- **You want to start a new feature** — just describe it. Each gets its own numbered folder; nothing overwrites.
