// sim-shard partitions tests/sim's top-level tests across CI shards (#2228,
// ADR 2228) and verifies the partition is complete.
//
// Usage:
//
//	go run ./tools/sim-shard -k K -index I        # print shard I's -run regex
//	go run ./tools/sim-shard -k K -verify         # check the live partition
//
// The partition is a pure function of the test names: sorted, then FNV-1a
// 32-bit of the name modulo K. There is no stored assignment, so a test added
// later lands in a shard with no list edit, and the check always compares
// against the live `go test -list` output rather than a list that can go
// stale. A test keeps its shard for a given K on every run; a different K is a
// different partition.
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

const defaultPkg = "./tests/sim"

func main() {
	k := flag.Int("k", 0, "number of shards (>= 1)")
	index := flag.Int("index", -1, "0-based shard index to print")
	verify := flag.Bool("verify", false, "verify the live partition is complete and disjoint")
	pkg := flag.String("pkg", defaultPkg, "package to list tests from")
	flag.Parse()

	if *k < 1 {
		fatalf("-k must be >= 1")
	}
	names, err := ListTests(*pkg)
	if err != nil {
		fatalf("%v", err)
	}
	shards := Partition(names, *k)
	if *verify {
		if err := Verify(names, shards); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("ok: %d tests across %d shards\n", len(names), *k)
		return
	}
	if *index < 0 || *index >= *k {
		fatalf("-index must be in [0, %d)", *k)
	}
	if len(shards[*index]) == 0 {
		fatalf("shard %d of %d is empty", *index, *k)
	}
	fmt.Println(RunRegex(shards[*index]))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sim-shard: "+format+"\n", args...)
	os.Exit(1)
}

// ShardOf returns the shard a test name belongs to for k shards.
func ShardOf(name string, k int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return int(h.Sum32() % uint32(k))
}

// Partition splits names into k shards. Input order does not matter, and
// duplicate input names are kept so Verify can see them.
func Partition(names []string, k int) [][]string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	shards := make([][]string, k)
	for _, n := range sorted {
		i := ShardOf(n, k)
		shards[i] = append(shards[i], n)
	}
	return shards
}

// Verify reports every name in all that is missing from the shards or appears
// more than once across them, and every shard name that is not in all.
func Verify(all []string, shards [][]string) error {
	want := map[string]bool{}
	for _, n := range all {
		want[n] = true
	}
	got := map[string]int{}
	for _, s := range shards {
		for _, n := range s {
			got[n]++
		}
	}
	var problems []string
	for _, n := range sortedKeys(want) {
		switch c := got[n]; {
		case c == 0:
			problems = append(problems, fmt.Sprintf("missing from every shard: %s", n))
		case c > 1:
			problems = append(problems, fmt.Sprintf("duplicated (%d shards): %s", c, n))
		}
	}
	extra := map[string]bool{}
	for n := range got {
		if !want[n] {
			extra[n] = true
		}
	}
	for _, n := range sortedKeys(extra) {
		problems = append(problems, fmt.Sprintf("not in the full test list: %s", n))
	}
	if len(problems) > 0 {
		return fmt.Errorf("partition is not complete and disjoint:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// RunRegex builds the anchored -run pattern selecting exactly these top-level
// tests. The ^(...)$ anchoring keeps -run's slash splitting from matching
// anything but the named tests; subtests follow their parent.
func RunRegex(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// ParseList extracts top-level test names from `go test -list` output,
// ignoring the `ok` line and any TestMain banner. Only names starting with
// "Test" are kept: Benchmark/Example/Fuzz are not run by a plain go test.
func ParseList(out string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Test") || strings.ContainsAny(line, " \t") || seen[line] {
			continue
		}
		seen[line] = true
		names = append(names, line)
	}
	sort.Strings(names)
	return names
}

// ListTests runs `go test -list` for pkg and returns its top-level tests.
func ListTests(pkg string) ([]string, error) {
	cmd := exec.Command("go", "test", "-list", "^Test", pkg)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go test -list %s: %w", pkg, err)
	}
	names := ParseList(stdout.String())
	if len(names) == 0 {
		return nil, fmt.Errorf("go test -list %s reported no tests", pkg)
	}
	return names, nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
