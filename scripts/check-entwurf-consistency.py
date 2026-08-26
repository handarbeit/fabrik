#!/usr/bin/env python3
"""Assert cross-file invariants in the entwurf plugin.

Four review rounds found the same class of defect every time: a rule fixed in one skill
and left broken in its mirror. Rules that can live in one place moved to CONVENTIONS.md;
this covers the rest — invariants necessarily restated across files, where drift is
invisible to a reader and obvious to a script.

Two design rules, both learned from the previous bash version:

  * Match against whitespace-normalised text. Prose wraps, and a check anchored to a
    line break silently stops checking. That is how the phantom-string check became a
    no-op while still printing green.
  * A check whose pattern finds nothing must FAIL, never silently skip. A brittle check
    that goes quiet is worse than no check.

Usage: python3 scripts/check-entwurf-consistency.py
"""
import re, sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
P = ROOT / "plugin/entwurf"
C = P / "CONVENTIONS.md"
FOUND = P / "skills/architecture-foundations/SKILL.md"
EXP = P / "skills/experience-baseline/SKILL.md"
BASE = P / "skills/architecture-baseline/SKILL.md"
BASELINES = {"architecture-foundations": FOUND, "experience-baseline": EXP, "architecture-baseline": BASE}
RATIFYING = {"experience-baseline": EXP, "architecture-baseline": BASE}
ALL_SKILLS = sorted((P / "skills").glob("*/SKILL.md")) + sorted((P / "skills").glob("*/references/*.md"))

fails = []
def fail(msg): fails.append(msg)

for p in [C, FOUND, EXP, BASE]:
    if not p.exists(): sys.exit(f"missing file: {p}")

raw = {p: p.read_text() for p in [C, FOUND, EXP, BASE]}
def flat(p):
    """Whitespace-normalised, so a phrase spanning a line break still matches."""
    return re.sub(r"\s+", " ", raw[p])

# 1. Stop conditions must fire on missing SPECS, never conjunctively.
for n, p in [("architecture-foundations", FOUND), ("experience-baseline", EXP)]:
    f = flat(p)
    if re.search(r"no constitution and no specs|neither a constitution nor specs|no specs and no constitution", f, re.I):
        fail(f"{n}: stop condition is conjunctive — must stop on missing specs alone")
    if not re.search(r"if there are no specs.{0,40}stop", f, re.I):
        fail(f"{n}: no 'if there are no specs … stop' condition")

# 2. One marker vocabulary, across every skill and reference file.
ALLOWED = {"CLARIFICATION", "DECISION", "LOOKUP", "DESIGN", "MEASUREMENT", "BASELINE"}
seen = set()
for p in [C] + ALL_SKILLS:
    seen |= set(re.findall(r"NEEDS ([A-Z]+)", p.read_text()))
if seen - ALLOWED:
    fail(f"marker vocabulary drift: unexpected {sorted(seen - ALLOWED)}")

# 3. Ratification bars: document-side, '[ ]' not 'unticked', and 'unparked'.
#    Scope includes CONVENTIONS, which is where the canonical bar now lives.
for n, p in list(RATIFYING.items()) + [("CONVENTIONS", C)]:
    f = flat(p)
    if re.search(r"(do not offer|don't offer) ratification", f, re.I):
        fail(f"{n}: ratification bar is interviewer-side — must read 'Status is not set to Ratified … whoever asks'")
    if re.search(r"ratification while any gate item is unticked|while any gate item is unticked", f, re.I):
        fail(f"{n}: ratification bar says 'unticked' — must test for '[ ]', since '[~]' is also unticked")
    if re.search(r"while any blocking marker", f, re.I) and not re.search(r"while any [*_]*unparked[*_]* blocking marker", f, re.I):
        fail(f"{n}: ratification bar omits 'unparked' — parked markers remain in the document by design")

# 4. Gate written before ratification, in both ratifying skills.
for n, p in RATIFYING.items():
    g = raw[p].find("\n## The gate")
    r = raw[p].find("\n## Ratification")
    if g < 0 or r < 0: fail(f"{n}: missing '## The gate' or '## Ratification'")
    elif g > r: fail(f"{n}: ratification precedes the gate")

# 5. Amendment safety: every ratifying template's footer must be protected.
for n, p in RATIFYING.items():
    f = flat(p)
    if "never re-apply the template" not in f:
        fail(f"{n}: amendment branch does not forbid re-applying the template — a ratified footer can be silently reset")
    if not re.search(r"(leave|do not reset).{0,80}(Status|footer)", f, re.I):
        fail(f"{n}: amendment branch does not protect Status/Signed off by/Ratified")

# 6. No template may hard-code a version or a status. Includes CONVENTIONS' example footer.
for n, p in list(BASELINES.items()) + [("CONVENTIONS", C)]:
    if re.search(r"\*\*Version\*\*:\s*\d+\.\d+", raw[p]):
        fail(f"{n}: hard-codes a version in a footer — amendments would reset it")

# 7. Disclosure classes: consumer must carry a rule per class, not merely the words.
for cls in ["Concealed", "Visible but blocked", "Visible with reason", "Existence disclosed"]:
    if cls.lower() not in flat(FOUND).lower():
        fail(f"architecture-foundations: disclosure class missing: {cls}")
if not re.search(r"one rule per\s*\**\(?class[,)]|rule of its own", flat(EXP), re.I):
    fail("experience-baseline: does not require a rule per disclosure class (or class x viewer)")

# 8. Foundations sections stage 5 must carry forward.
for sec in ["Constraints", "Shared domain model", "Interface contracts", "Authority model",
            "Shared state vocabularies", "Where the design is free"]:
    if f"## {sec}" not in raw[FOUND]: fail(f"architecture-foundations: template missing '## {sec}'")
    if f"## {sec}" not in raw[BASE]:  fail(f"architecture-baseline: drops foundations section '## {sec}'")

# 9. Separate checklist files; stage 5 reads stage 3's by full path.
if "checklists/architecture-foundations.md" not in raw[FOUND]: fail("architecture-foundations: wrong checklist path")
if "checklists/architecture-baseline.md" not in raw[BASE]:     fail("architecture-baseline: wrong checklist path")
if ".specify/memory/checklists/architecture-foundations.md" not in flat(BASE):
    fail("architecture-baseline: must read stage 3's checklist by full path")
if "checklists/experience.md" not in raw[EXP]:                 fail("experience-baseline: wrong checklist path")
if re.search(r"checklists/architecture\.md", raw[FOUND] + raw[BASE] + raw[C]):
    fail("stale unified checklist path 'checklists/architecture.md' still referenced")

# 10. No skill may instruct removing a string nothing writes. (The check that was a no-op.)
removals = re.findall(r"Remove the [`\"]([^`\"]{4,})[`\"]", flat(BASE))
if not removals:
    fail("checker: no 'Remove the …' instruction found in architecture-baseline — "
         "check 10 has stopped checking anything (this is how it silently died before)")
for q in removals:
    needle = q.split("…")[0].strip()
    if needle and needle not in flat(FOUND) and needle not in flat(C):
        fail(f'architecture-baseline: told to remove a string nothing writes: "{needle[:50]}"')

# 11. Register: every baseline appends, gates on it, and records unresolved markers.
for n, p in BASELINES.items():
    f = flat(p)
    if not re.search(r"append.{0,60}decisions\.md", f, re.I):
        fail(f"{n}: never instructed to append to the decision register")
    if not re.search(r"- \[ \][^\n]*register", raw[p]):
        fail(f"{n}: gate has no decision-register item")
    if "Unresolved row" not in f:
        fail(f"{n}: gate does not require an Unresolved row per unresolved marker")

# 12. Register lives at project level, not inside a feature folder.
tree = re.search(r"```\n(\.specify/memory/.*?)```", raw[C], re.S)
if not tree: fail("CONVENTIONS: canonical tree not found")
else:
    block = tree.group(1)
    before_specs = block.split("specs/")[0]
    if "decisions.md" not in before_specs:
        fail("CONVENTIONS: decisions.md is not in the .specify/memory/ block — a per-feature register defeats its purpose")

# 13. Constitution-version gate item needs an 'or its absence' escape everywhere.
for n, p in BASELINES.items():
    if not re.search(r"constitution version.{0,90}absence is recorded", flat(p), re.I):
        fail(f"{n}: constitution-version gate item has no 'or its absence' escape")

# 14. Inbound leg of the revision beat must be gated, not just the outbound.
for n, p in BASELINES.items():
    if not re.search(r"answered here, or restated", flat(p), re.I):
        fail(f"{n}: gate has no inbound open-question item")

# 15. Every template needs a parking section for blocking markers.
for n, p in BASELINES.items():
    if "## Deliberately deferred" not in raw[p]:
        fail(f"{n}: template has no '## Deliberately deferred' to park blocking markers into")

# 16. Declared topic/section counts must match reality, in every skill.
WORDS = {"Five":5,"Six":6,"Seven":7,"Eight":8,"Nine":9,"Ten":10,"Eleven":11,"Twelve":12}
for n, p in BASELINES.items():
    m = re.search(r"\b(" + "|".join(WORDS) + r")\s+(?:topics|sections)\b", raw[p])
    if not m:
        fail(f"{n}: no declared topic/section count found — check 16 cannot verify anything")
        continue
    actual = len(re.findall(r"^### \d+[a-z]?\. ", raw[p], re.M))
    if WORDS[m.group(1)] != actual:
        fail(f"{n}: declares '{m.group(1)} topics' but has {actual}")

# 17. A heading a skill tells you to RECORD UNDER must exist in that skill's template.
#     Scoped to record/write verbs — a skill legitimately references headings it only reads
#     from another stage's document, and flagging those false-fails.
for n, p in BASELINES.items():
    written = set(re.findall(r"(?:record(?:ed)?|write|writing) (?:it |them |each )?under `(#{2,3} [A-Z][^`]{3,60})`", flat(p), re.I))
    for h in written:
        if h not in raw[p].replace(f"`{h}`", ""):
            fail(f"{n}: tells you to record under '{h}' but the template has no such heading")

# 18. The not-applicable state must be reachable from every gate, not just defined.
if "[~]" not in raw[C]:
    fail("CONVENTIONS: no '[~]' not-applicable state — an inapplicable item strands ratification")
for n, p in BASELINES.items():
    if "[~]" not in raw[p]:
        fail(f"{n}: gate never mentions '[~]' — the not-applicable state is unreachable from where it is needed")

# 19. Every footer field a skill writes must be described in CONVENTIONS' canonical
#     footer section. A field added to one skill and not to the shared description
#     leaves CONVENTIONS no longer describing the document it claims to govern.
conv_footer = flat(C)
for n, p_ in BASELINES.items():
    tmpl = re.findall(r"\*\*(Version|Status|Signed off by|Ratified|Last Amended|Against [a-z ]+)\*\*", raw[p_])
    for field in set(tmpl):
        if f"**{field}**" not in conv_footer:
            fail(f"{n}: writes footer field '**{field}**' that CONVENTIONS' footer section never describes")

# 20. The marketplace listing and the plugin manifest must agree — the listing is what
#     someone reads before installing, and they drifted once already.
import json as _json
mk = [x for x in _json.loads((ROOT / ".claude-plugin/marketplace.json").read_text())["plugins"]
      if x["name"] == "entwurf"]
pj = _json.loads((P / ".claude-plugin/plugin.json").read_text())
if not mk:
    fail("marketplace.json: no entwurf entry")
else:
    for role in ["product owner", "technical architect", "experience lead"]:
        a = re.search(re.escape(role) + r" \(([^)]*)\)", mk[0]["description"])
        b = re.search(re.escape(role) + r" \(([^)]*)\)", pj["description"])
        if bool(a) != bool(b) or (a and b and a.group(1) != b.group(1)):
            fail(f"marketplace.json and plugin.json disagree on what the {role} stage covers")

print("entwurf consistency\n")
for f in fails: print(f"  ✗ {f}")
print(f"\n{'all invariants hold' if not fails else f'{len(fails)} violation(s)'}")
sys.exit(1 if fails else 0)
