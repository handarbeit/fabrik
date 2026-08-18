# Coverage Taxonomy

Assess the specification against every category below and mark each **Clear**, **Partial**, or **Missing**. The categories are ordered roughly by how expensive the gap is to discover late — a wrong scope boundary is more costly than a missing log line — but weigh impact against the specific feature rather than following the order mechanically.

**These category names are working vocabulary for you, not text to show the person.** They're engineering terms, and a business owner handed a table of twenty of them marked "Outstanding" learns nothing except that something is wrong. Each entry below carries a plain-language gloss in italics — use that phrasing when you need to raise the topic in conversation or in the coverage summary, and group the categories into the six business-readable headings named in the skill's Reporting section.

## Functional Scope & Behavior

- Core user goals and success criteria — *what it's for, and how you'd know it worked*
- Explicit out-of-scope declarations — *what this deliberately doesn't do*
- User roles / personas and how they differ — *who uses it, and who's allowed to do what*

## Domain & Data Model

- Entities, attributes, relationships — *the things it keeps track of, and what it records about each*
- Identity and uniqueness rules — *what makes two records the same thing or different things*
- Lifecycle and state transitions — *the stages something moves through, from created to finished*
- Data volume and scale assumptions — *how much of it there'll be*

## Interaction & UX Flow

- Critical user journeys and their sequence — *the steps someone actually takes, in order*
- Error, empty, and loading states — *what they see when something's wrong, missing, or still working*
- Accessibility and localization needs — *whether it has to work for people with disabilities, or in another language*

## Non-Functional Quality Attributes

- Performance (latency, throughput targets) — *how fast it has to be*
- Scalability (limits, growth expectations) — *how much busier it might get*
- Reliability and availability (uptime, recovery expectations) — *how often it can be down, and how quickly it must come back*
- Observability (what needs logging, measuring, tracing) — *what the business needs to be able to see and report on*
- Security and privacy (authentication, authorization, data protection, threat assumptions) — *who can get in, who can see what, and what must be kept private*
- Compliance and regulatory constraints — *rules you have to follow by law or contract*

## Integration & External Dependencies

- External services and APIs, and what happens when they fail — *other systems this relies on, and what happens when one is unavailable*
- Data import/export formats — *getting information in and out, including to a spreadsheet*
- Protocol and versioning assumptions — *how it keeps working when a connected system changes*

## Edge Cases & Failure Handling

- Negative scenarios — *what happens when someone does the wrong thing*
- Rate limiting and throttling — *what happens when too many people use it at once*
- Conflict resolution (for example, concurrent edits to the same record) — *what happens when two people change the same thing at the same time*

## Constraints & Tradeoffs

- Technical constraints already fixed (language, storage, hosting) — *decisions already made that can't be revisited*
- Explicit tradeoffs made, and alternatives already rejected — *what was chosen over what, and why*

## Terminology & Consistency

- Canonical glossary terms — *the agreed word for each thing*
- Synonyms and deprecated terms to avoid — *words that mean the same thing and shouldn't be mixed*

## Completion Signals

- Whether acceptance criteria are actually testable — *whether you could genuinely check each promise was met*
- Measurable definition-of-done indicators — *what "finished" means, in terms you could verify*

## Placeholders & Unresolved Decisions

- TODO markers and open questions left in the text — *notes-to-self nobody came back to*
- Ambiguous adjectives — "robust", "intuitive", "fast", "seamless" — used without a number attached — *promises that sound firm but can't be checked*

Vague adjectives are the highest-yield thing to scan for. They read as requirements but can't be tested, so they survive review and then get interpreted differently by everyone downstream. Turning one into a number is usually the single most valuable question available — but the number must come from the person, never from you. Ask what they'd expect or what they measure today; if they don't know, record the shape and leave the figure as `[NEEDS BASELINE]` rather than proposing a plausible one for them to agree to.
