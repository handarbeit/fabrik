---
name: audit-documentation
description: Audit Fabrik docs against recently shipped features — files gap issues for undocumented behavior, closes issues whose gaps are now covered
user_invocable: true
---

# Audit Documentation

Compare recently shipped features against USER_GUIDE.md, README.md, and docs/index.md. File GitHub issues for documentation gaps and close existing documentation issues whose gaps are now covered.

## Usage

```
/audit-documentation                  # Scan issues closed in the last 30 days
/audit-documentation --since v0.0.20  # Scan issues referenced in commits since a tag
```

**Known limitation**: Rate limits may apply for repos with hundreds of closed issues. The tag-based mode may miss issues if their PRs used `Fixes` or bare URLs instead of `Closes #NNN`.

## Steps

### Step 1 — Parse arguments and pre-flight

Detect whether `--since <tag>` was provided:

```bash
# Tag-based mode
TAG="v0.0.20"  # extracted from --since argument

# Validate tag exists locally
git rev-parse --verify "refs/tags/$TAG" 2>/dev/null
if [ $? -ne 0 ]; then
  # Try fetching tags and retry
  git fetch --tags origin
  git rev-parse --verify "refs/tags/$TAG" 2>/dev/null || {
    echo "Error: tag '$TAG' not found locally or on origin. Aborting."
    exit 1
  }
fi
```

For the default 30-day mode, compute the cutoff date using Python (portable across macOS and Linux):

```bash
CUTOFF_DATE=$(python3 -c "from datetime import datetime,timedelta; print((datetime.now()-timedelta(days=30)).strftime('%Y-%m-%d'))")
echo "Scanning issues closed since: $CUTOFF_DATE"
```

### Step 2 — Discover source issues

**Tag-based mode (two-pass):**

Pass 1 — extract issue numbers from commit messages since the tag:
```bash
git log "$TAG..HEAD" --format="%s%n%b" | grep -oE '#[0-9]+' | grep -oE '[0-9]+' | sort -u
```

Pass 2 — get the tag's commit date, then find merged PRs since that date and extract `Closes #NNN` references:
```bash
TAG_DATE=$(git log -1 --format="%ci" "refs/tags/$TAG" | cut -d' ' -f1)
gh pr list -R tenaciousvc/fabrik --state merged \
  --search "merged:>=$TAG_DATE" \
  --json number,body \
  --limit 500 \
  --jq '.[] | .body' \
  | grep -oiE '(closes|fixes|resolves) #[0-9]+' \
  | grep -oE '[0-9]+' \
  | sort -u
```

Combine and deduplicate the issue numbers from both passes into a list (e.g. `200 201 205`).

Then fetch their full details for use in subsequent steps:
```bash
for NUM in $ISSUE_NUMBERS; do
  gh issue view "$NUM" -R tenaciousvc/fabrik --json number,title,labels,body
done
```

**Date-based mode (default 30 days):**

Fetch closed issues with their full bodies in one call to avoid rate-limit issues from per-issue view calls:
```bash
gh issue list -R tenaciousvc/fabrik \
  --state closed \
  --search "closed:>$CUTOFF_DATE" \
  --json number,title,labels,body \
  --limit 200
```

### Step 3 — Filter issues

From the discovered set, exclude:

1. **Issues labeled `documentation`** — avoid recursive analysis of documentation issues as source material
2. **Issues that are infrastructure/tooling-only** — issues with no user-facing feature content (e.g., CI fixes, dependency bumps, internal refactors with no observable behavior change)

For each excluded issue, note: issue number, title, and reason for exclusion.

Log the exclusions in your working notes so you can include them in the final summary.

### Step 4 — Read documentation

Read the three documentation targets in full:

```bash
cat USER_GUIDE.md
cat README.md
cat docs/index.md
```

As you read, note which sections are most likely to be affected by recently shipped features — these are the sections to scrutinize during gap analysis.

### Step 5 — Check existing open documentation issues

Fetch all currently open issues labeled `documentation`:

```bash
gh issue list -R tenaciousvc/fabrik \
  --state open \
  --label documentation \
  --json number,title,body
```

Keep this list for two purposes:
1. **Deduplication** — don't file a new gap issue if an equivalent one already exists
2. **Deck clearing** — close issues whose gaps are now covered by current docs

### Step 6 — Analyze gaps

For each filtered source issue (or group of related issues), compare the feature/behavior described against the three documentation files.

**Rules for gap analysis:**
- A feature is adequately documented if a user reading the docs would know the feature exists, how to use it, and what behavior to expect
- A feature is inadequately documented if it's missing entirely, mentioned without explanation, or only documented internally (e.g., in ADRs, not in USER_GUIDE)
- **Err on the side of filing** — if uncertain whether a feature is adequately covered, file a gap issue. False positives are cheap (the Specify stage will refine them); false negatives cause documentation drift
- Group related issues that describe the same undocumented feature into a single gap entry — don't file one issue per source issue if they all relate to the same gap

For each gap, record:
- Gap title (what's undocumented)
- Gap description (what the docs are missing)
- Source issue numbers
- Whether a matching open documentation issue already exists (from Step 5)

### Step 7 — Clear the deck

For each existing open `documentation` issue from Step 5 where the current docs **clearly and completely** address the described gap:

```bash
gh issue close -R tenaciousvc/fabrik <number> \
  --comment "Closed by /audit-documentation: gap is now covered in current documentation."
```

**Conservatism rule**: Only close if the documentation section clearly describes this specific behavior — if in doubt, leave open. Partial coverage does not justify closure.

### Step 8 — File new gap issues

For each gap from Step 6 that does NOT already have a matching open documentation issue:

```bash
# 1. Create the issue, capture the URL
ISSUE_URL=$(gh issue create -R tenaciousvc/fabrik \
  --title "<gap title>" \
  --label "documentation" \
  --label "fabrik:yolo" \
  --body "<issue body — see format below>" \
  --json url --jq '.url')

# 2. Add to the Fabrik PM project board (org project #1), capture item ID
ITEM_ID=$(gh project item-add 1 --owner tenaciousvc \
  --url "$ISSUE_URL" \
  --format json --jq '.id')

# 3. Set status to Specify
gh project item-edit \
  --id "$ITEM_ID" \
  --field-id PVTSSF_lADOA8vIBc4BTew-zhAtcsQ \
  --project-id PVT_kwDOA8vIBc4BTew- \
  --single-select-option-id 29d94fed
```

**Issue body format:**

```markdown
## Documentation Gap

<Clear description of what is missing or inadequately documented in USER_GUIDE.md, README.md, or docs/index.md>

## Problem

<Excerpt of the relevant Problem/Summary from the source issue(s) — keep concise, 2-5 sentences>

## Scope

- USER_GUIDE.md — <which sections need updating>
- README.md — <if relevant>
- docs/index.md — <if relevant>

## Source issues

<list of source issue numbers as #N references>
```

Keep the body concise — the Specify stage will elaborate as needed.

### Step 9 — Report summary

Print a structured summary of everything the audit did:

```
## Audit Documentation — Summary

**Period**: <date range or tag range scanned>

**Issues scanned**: <N>
**Issues excluded**: <N>
  - <issue#>: <title> — <reason>
  - ...

**Documentation gaps found**: <N>

**New gap issues filed** (<N>):
  - #<number>: <title>
  - ...

**Existing documentation issues closed** (<N>):
  - #<number>: <title>

**Existing documentation issues left open** (<N>):
  - #<number>: <title> — <reason left open>
```

If no gaps were found, say so explicitly: "No documentation gaps found — docs appear current."
