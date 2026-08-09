#!/usr/bin/env bash
# scripts/wire-contract/refresh-schema.sh — re-download GitHub's public GraphQL
# SDL schema and update its provenance metadata.
#
# This is the "R1" half of the wire-contract test layer (see issue #1453 /
# adrs/1453-wire-contract-graphql-schema-validation.md). The schema is public
# and needs no authentication, so this script is safe to run anytime,
# including from a laptop with no GitHub token configured.
#
# Usage:
#   scripts/wire-contract/refresh-schema.sh
#
# After running, review the diff of github/testdata/schema/github.graphql —
# a schema change that breaks github/wire_contract_test.go means a
# production query/mutation now targets a field or argument GitHub has
# removed or renamed, and needs a real code fix, not just a re-vendor.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCHEMA_DIR="$REPO_ROOT/github/testdata/schema"
SCHEMA_FILE="$SCHEMA_DIR/github.graphql"
META_FILE="$SCHEMA_DIR/SCHEMA_META.json"
SOURCE_URL="https://docs.github.com/public/fpt/schema.docs.graphql"

mkdir -p "$SCHEMA_DIR"

echo "Fetching $SOURCE_URL ..."
curl -fsS "$SOURCE_URL" -o "$SCHEMA_FILE"

FETCHED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
cat > "$META_FILE" <<EOF
{
  "source_url": "$SOURCE_URL",
  "fetched_at": "$FETCHED_AT",
  "notes": "GitHub's publicly documented GraphQL SDL schema for the free/pro/team product tier. Vendored offline so wire_contract_test.go can validate github/'s embedded query/mutation strings without live credentials. Refresh with scripts/wire-contract/refresh-schema.sh. See github/testdata/README.md for the staleness policy (wire_contract_test.go fails go test after 180 days)."
}
EOF

echo "Wrote $SCHEMA_FILE ($(wc -l < "$SCHEMA_FILE") lines) and $META_FILE (fetched_at=$FETCHED_AT)"
echo "Run 'go test ./github/...' to confirm every existing query/mutation still validates."
