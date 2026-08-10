package github

// This file implements R1 of #1453: static, offline validation of every
// GraphQL query/mutation string embedded in this package against a vendored
// copy of GitHub's real GraphQL SDL schema. See github/testdata/README.md
// for the schema refresh procedure and adrs/1453-wire-contract-graphql-schema-validation.md
// for the design rationale.
//
// The registry below references each hoisted query/mutation const BY
// IDENTIFIER (not by re-scraping source text), so what's validated here is
// provably the exact string the production code sends over the wire — there
// is no drift risk between "what's tested" and "what's shipped".

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

const wireSchemaPath = "testdata/schema/github.graphql"
const wireSchemaMetaPath = "testdata/schema/SCHEMA_META.json"

// wireSchemaMaxAge is how old the vendored schema is allowed to get before
// the test suite refuses to trust it. Per R1 ("a stale schema failing open
// is the one outcome to avoid"), this fails go test, it does not warn.
const wireSchemaMaxAge = 180 * 24 * time.Hour

// wireOperation is one GraphQL call site in this package. Documents holds
// one fully-rendered query/mutation string per call site; call sites whose
// query is built from a Go string template (the board-fetch queries, which
// interpolate "organization" or "user") render both variants here so both
// are validated, while still counting as a single call site for the
// completeness guard below.
type wireOperation struct {
	// name identifies the const this entry validates, for failure messages.
	name string
	// documents are the fully-rendered GraphQL documents to validate.
	documents []string
}

// wireContractRegistry enumerates every GraphQL query/mutation string
// embedded in this package. Each entry corresponds to exactly one
// c.graphqlRequest( call site in non-test source — see
// TestWireContract_RegistryIsComplete, which fails go test if this list
// and the call-site count in the source ever diverge.
var wireContractRegistry = []wireOperation{
	// project.go
	{"fetchProjectBoardQueryTemplate", []string{
		fmt.Sprintf(fetchProjectBoardQueryTemplate, "organization"),
		fmt.Sprintf(fetchProjectBoardQueryTemplate, "user"),
	}},
	{"probeProjectBoardQueryTemplate", []string{
		fmt.Sprintf(probeProjectBoardQueryTemplate, "organization"),
		fmt.Sprintf(probeProjectBoardQueryTemplate, "user"),
	}},
	{"fetchItemDetailsQuery", []string{fetchItemDetailsQuery}},
	{"fetchNodeCommentsQuery", []string{fetchNodeCommentsQuery}},
	{"fetchNodeLabelsQuery", []string{fetchNodeLabelsQuery}},
	{"fetchProjectUpdatedAtQuery", []string{fetchProjectUpdatedAtQuery}},
	{"fetchProjectItemStatusQuery", []string{fetchProjectItemStatusQuery}},
	{"fetchProjectItemStatusBatchQuery", []string{fetchProjectItemStatusBatchQuery}},
	{"lookupIssueProjectItemQuery", []string{lookupIssueProjectItemQuery}},
	{"addProjectV2ItemByIdMutation", []string{addProjectV2ItemByIdMutation}},
	{"addBlockedByMutation", []string{addBlockedByMutation}},

	// prs.go
	{"fetchPRReviewDecisionQuery", []string{fetchPRReviewDecisionQuery}},
	{"fetchPRReviewThreadsQuery", []string{fetchPRReviewThreadsQuery}},
	{"prNodeIDQuery", []string{prNodeIDQuery}},
	{"markPRReadyMutation", []string{markPRReadyMutation}},
	{"enablePullRequestAutoMergeMutation", []string{enablePullRequestAutoMergeMutation}},
	{"disablePullRequestAutoMergeMutation", []string{disablePullRequestAutoMergeMutation}},
	{"enqueuePullRequestMutation", []string{enqueuePullRequestMutation}},
	{"dequeuePullRequestMutation", []string{dequeuePullRequestMutation}},

	// status.go
	{"updateProjectItemStatusMutation", []string{updateProjectItemStatusMutation}},
	{"archiveProjectItemMutation", []string{archiveProjectItemMutation}},
	{"fetchStatusFieldQuery", []string{fetchStatusFieldQuery}},

	// comments.go
	{"resolveReviewThreadMutation", []string{resolveReviewThreadMutation}},
}

// loadWireSchema loads the vendored GitHub GraphQL SDL schema. It is a
// test helper, not a package-level var, so a load failure surfaces as a
// normal test failure (t.Fatalf) rather than a package-init panic.
func loadWireSchema(t *testing.T) *ast.Schema {
	t.Helper()
	data, err := os.ReadFile(wireSchemaPath)
	if err != nil {
		t.Fatalf("reading vendored schema %s: %v (run scripts/wire-contract/refresh-schema.sh)", wireSchemaPath, err)
	}
	schema, err := gqlparser.LoadSchema(&ast.Source{Name: wireSchemaPath, Input: string(data)})
	if err != nil {
		t.Fatalf("parsing vendored schema %s: %v", wireSchemaPath, err)
	}
	return schema
}

// TestWireContract_SchemaNotStale enforces R1's "a stale schema failing
// open is the one outcome to avoid": once the vendored schema is older than
// wireSchemaMaxAge, every test in this file starts validating queries
// against a schema that may no longer reflect GitHub's live API, so the
// suite must fail loudly rather than keep passing silently.
func TestWireContract_SchemaNotStale(t *testing.T) {
	data, err := os.ReadFile(wireSchemaMetaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", wireSchemaMetaPath, err)
	}
	var meta struct {
		FetchedAt string `json:"fetched_at"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parsing %s: %v", wireSchemaMetaPath, err)
	}
	fetchedAt, err := time.Parse(time.RFC3339, meta.FetchedAt)
	if err != nil {
		t.Fatalf("parsing fetched_at %q in %s: %v", meta.FetchedAt, wireSchemaMetaPath, err)
	}
	age := time.Since(fetchedAt)
	if age > wireSchemaMaxAge {
		t.Fatalf("vendored GraphQL schema is %s old (fetched %s), older than the %s staleness limit — "+
			"run scripts/wire-contract/refresh-schema.sh to refresh it before trusting wire-contract results",
			age.Round(time.Hour), meta.FetchedAt, wireSchemaMaxAge)
	}
}

// TestWireContract_RegistryIsComplete guards against the registry above
// silently falling out of sync with the source: a new c.graphqlRequest(
// call site added to production code without a matching registry entry
// would otherwise escape schema validation entirely, which is exactly the
// "presents as complete but isn't" failure R5 warns about, applied to R1.
// This cannot say *which* query is missing, but it turns a silent gap into
// a failing go test.
func TestWireContract_RegistryIsComplete(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing *.go: %v", err)
	}

	callSitePattern := regexp.MustCompile(`\bc\.graphqlRequest\(`)
	var sourceFiles []string
	var source []byte
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}
		sourceFiles = append(sourceFiles, f)
		source = append(source, data...)
		total += len(callSitePattern.FindAllIndex(data, -1))
	}

	if total != len(wireContractRegistry) {
		t.Fatalf("found %d c.graphqlRequest( call sites across %v but wireContractRegistry has %d entries — "+
			"a query was added or removed without updating the registry in wire_contract_test.go",
			total, sourceFiles, len(wireContractRegistry))
	}

	var missing []string
	for _, op := range wireContractRegistry {
		namePattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(op.name) + `\b`)
		if !namePattern.Match(source) {
			missing = append(missing, op.name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("registry entries %v reference an identifier no longer found in source %v — "+
			"the call site was likely removed or renamed without updating wire_contract_test.go",
			missing, sourceFiles)
	}
}

// TestWireContract_AllQueriesValidateAgainstSchema is R1's core test: every
// GraphQL query/mutation string this package sends to GitHub must validate
// (operation names, field selections, argument names, variable types)
// against the vendored real schema. This is what would have caught
// addBlockedByIssue at go test time (see AC2/AC3 below for the proof).
func TestWireContract_AllQueriesValidateAgainstSchema(t *testing.T) {
	schema := loadWireSchema(t)

	for _, op := range wireContractRegistry {
		op := op
		t.Run(op.name, func(t *testing.T) {
			for i, doc := range op.documents {
				_, errs := gqlparser.LoadQuery(schema, doc)
				if errs != nil {
					t.Fatalf("document variant %d for %s failed schema validation: %v\n\ndocument:\n%s", i, op.name, errs, doc)
				}
			}
		})
	}
}

// TestWireContract_NeutralizationCatchesBadOperationName is AC2's
// permanent regression proof: it mutates an in-test COPY of
// addBlockedByMutation's operation name from addBlockedBy to
// addBlockedByIssue — the exact historical bug preserved in the comment
// above the real const in project.go — and asserts schema validation
// rejects it, naming the operation. It never touches the real production
// const; addBlockedByMutation itself is separately proven valid by
// TestWireContract_AllQueriesValidateAgainstSchema above.
func TestWireContract_NeutralizationCatchesBadOperationName(t *testing.T) {
	schema := loadWireSchema(t)

	const marker = "addBlockedBy(input"
	if !strings.Contains(addBlockedByMutation, marker) {
		t.Fatalf("addBlockedByMutation no longer contains %q — update this test's neutralization target", marker)
	}
	bad := strings.Replace(addBlockedByMutation, marker, "addBlockedByIssue(input", 1)

	_, errs := gqlparser.LoadQuery(schema, bad)
	if errs == nil {
		t.Fatal("expected schema validation to reject addBlockedByIssue, but it passed — " +
			"operation-name validation is not working")
	}
	if !strings.Contains(errs.Error(), "addBlockedByIssue") {
		t.Fatalf("validation error did not name the bad operation addBlockedByIssue: %v", errs)
	}
}

// TestWireContract_NeutralizationCatchesBadArgumentName is AC3's
// independent proof that field/argument-level validation works, not just
// operation-name validation — an operation-name-only check would pass the
// previous test while missing most of this bug class (e.g. a correct
// mutation name with a wrong input field, such as the issue's own
// blockingIssueId/blockedById example). It mutates an in-test COPY of
// addBlockedByMutation's input field name and asserts a distinct
// validation error naming the bad field.
func TestWireContract_NeutralizationCatchesBadArgumentName(t *testing.T) {
	schema := loadWireSchema(t)

	const marker = "blockingIssueId: $blockingIssueId"
	if !strings.Contains(addBlockedByMutation, marker) {
		t.Fatalf("addBlockedByMutation no longer contains %q — update this test's neutralization target", marker)
	}
	bad := strings.Replace(addBlockedByMutation, marker, "blockedById: $blockingIssueId", 1)

	_, errs := gqlparser.LoadQuery(schema, bad)
	if errs == nil {
		t.Fatal("expected schema validation to reject the blockedById argument, but it passed — " +
			"argument-level validation is not working")
	}
	if !strings.Contains(errs.Error(), "blockedById") {
		t.Fatalf("validation error did not name the bad argument blockedById: %v", errs)
	}
}

// TestWireContract_SchemaFilePresent is a cheap sanity check that the
// vendored schema path used throughout this file resolves from the
// package's own working directory (go test runs with cwd == package dir),
// so a future path refactor fails fast with a clear message instead of the
// more oblique "reading vendored schema: no such file" from loadWireSchema.
func TestWireContract_SchemaFilePresent(t *testing.T) {
	if _, err := os.Stat(filepath.Clean(wireSchemaPath)); err != nil {
		t.Fatalf("vendored schema not found at %s (relative to github/): %v", wireSchemaPath, err)
	}
}
