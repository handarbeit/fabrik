#!/usr/bin/env bash
# draft-release-notes.sh — Generate AI-assisted release notes for the next Fabrik release.
#
# Usage: scripts/draft-release-notes.sh
#
# Requires: claude CLI (from Claude Code — https://claude.ai/code)
# Output:   release-notes.md in the repo root
#
# Workflow:
#   1. Run this script before tagging a release
#   2. Review and edit release-notes.md
#   3. Commit release-notes.md to main
#   4. Tag and push the release — the CI workflow passes the file to GoReleaser

set -euo pipefail

# Check for claude CLI
if ! command -v claude &>/dev/null; then
  echo "Error: 'claude' CLI not found." >&2
  echo "Install Claude Code (https://claude.ai/code) and ensure 'claude' is on your PATH." >&2
  exit 1
fi

REPO_ROOT="$(git rev-parse --show-toplevel)"
OUTPUT_FILE="$REPO_ROOT/release-notes.md"

# Detect the previous tag
PREV_TAG=""
if PREV_TAG="$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null)"; then
  echo "Previous tag: $PREV_TAG"
else
  # Fallback: second-most-recent tag (handles case where HEAD is already tagged)
  PREV_TAG="$(git tag --sort=-version:refname 2>/dev/null | sed -n '2p')" || true
  if [ -n "$PREV_TAG" ]; then
    echo "Previous tag (fallback): $PREV_TAG"
  else
    echo "No previous tag found — using full git log."
  fi
fi

# Collect git log since previous tag (or full log if no previous tag)
if [ -n "$PREV_TAG" ]; then
  GIT_LOG="$(git log "${PREV_TAG}..HEAD" --oneline)"
else
  GIT_LOG="$(git log --oneline)"
fi

if [ -z "$GIT_LOG" ]; then
  echo "No commits found since $PREV_TAG. Nothing to release." >&2
  exit 1
fi

echo "Generating release notes from $(echo "$GIT_LOG" | wc -l | tr -d ' ') commits..."

PROMPT="You are writing GitHub release notes for Fabrik, a Go CLI that orchestrates Claude Code through an SDLC pipeline driven by GitHub Issues and Projects.

Here is the git log since the previous release (one commit per line):

$GIT_LOG

Write polished GitHub release notes in Markdown. Group changes under these headings (omit a heading if there are no relevant commits):
- ## What's New
- ## Improvements
- ## Bug Fixes
- ## Documentation
- ## Internal / Chores

Rules:
- Write in clear, concise prose — not bullet points that just repeat the commit message verbatim
- Focus on what changed and why it matters to the user, not implementation details
- Omit merge commits and trivial chores unless they're user-visible
- Do not include a title line (the release tag is used as the title on GitHub)
- Output only the release notes Markdown, no preamble or explanation"

# Pipe prompt to claude and write output to release-notes.md
echo "$PROMPT" | claude --output-format text -p - > "$OUTPUT_FILE"

echo ""
echo "Release notes written to: $OUTPUT_FILE"
echo ""
echo "Next steps:"
echo "  1. Review and edit $OUTPUT_FILE"
echo "  2. git add release-notes.md && git commit -m 'chore: prepare release notes'"
echo "  3. git tag vX.Y.Z && git push origin main vX.Y.Z"
