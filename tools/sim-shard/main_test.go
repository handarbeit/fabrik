package main

import (
	"fmt"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func fixtureNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("TestScenario%03d_Case", i)
	}
	return names
}

func TestPartitionIsCompleteAndDisjoint(t *testing.T) {
	names := fixtureNames(300)
	for k := 1; k <= 8; k++ {
		shards := Partition(names, k)
		if len(shards) != k {
			t.Fatalf("k=%d: got %d shards", k, len(shards))
		}
		if err := Verify(names, shards); err != nil {
			t.Fatalf("k=%d: %v", k, err)
		}
	}
}

func TestPartitionIsDeterministicAndOrderIndependent(t *testing.T) {
	names := fixtureNames(120)
	rev := make([]string, len(names))
	for i, n := range names {
		rev[len(names)-1-i] = n
	}
	a, b := Partition(names, 3), Partition(rev, 3)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("partition depends on input order")
	}
	if !reflect.DeepEqual(a, Partition(names, 3)) {
		t.Fatal("partition is not repeatable")
	}
	for i, s := range a {
		for _, n := range s {
			if ShardOf(n, 3) != i {
				t.Fatalf("%s is in shard %d but ShardOf says %d", n, i, ShardOf(n, 3))
			}
		}
	}
}

func TestPartitionAddedTestNeedsNoListEdit(t *testing.T) {
	names := fixtureNames(50)
	before := Partition(names, 4)
	grown := append(append([]string(nil), names...), "TestBrandNew")
	after := Partition(grown, 4)
	if err := Verify(grown, after); err != nil {
		t.Fatal(err)
	}
	// Existing tests keep their shard.
	for i := range before {
		for _, n := range before[i] {
			if ShardOf(n, 4) != i {
				t.Fatalf("%s moved", n)
			}
		}
	}
}

func TestVerifyFailsWhenATestIsDropped(t *testing.T) {
	names := fixtureNames(40)
	shards := Partition(names, 3)
	dropped := shards[0][0]
	shards[0] = shards[0][1:]
	err := Verify(names, shards)
	if err == nil {
		t.Fatal("Verify accepted a partition that drops a test")
	}
	if !strings.Contains(err.Error(), "missing from every shard: "+dropped) {
		t.Fatalf("error does not name the dropped test %s: %v", dropped, err)
	}
}

func TestVerifyFailsWhenATestIsDuplicated(t *testing.T) {
	names := fixtureNames(40)
	shards := Partition(names, 3)
	dup := shards[0][0]
	shards[1] = append(shards[1], dup)
	err := Verify(names, shards)
	if err == nil {
		t.Fatal("Verify accepted a partition that duplicates a test")
	}
	if !strings.Contains(err.Error(), "duplicated (2 shards): "+dup) {
		t.Fatalf("error does not name the duplicated test %s: %v", dup, err)
	}
}

func TestVerifyFailsOnUnknownTest(t *testing.T) {
	names := fixtureNames(10)
	shards := Partition(names, 2)
	shards[0] = append(shards[0], "TestGhost")
	err := Verify(names, shards)
	if err == nil || !strings.Contains(err.Error(), "TestGhost") {
		t.Fatalf("want an error naming TestGhost, got %v", err)
	}
}

func TestParseListKeepsOnlyTopLevelTests(t *testing.T) {
	out := "tests/sim: using /usr/bin/git\nTestB\nTestA\nBenchmarkX\nExampleY\nTestA\nok  \tgithub.com/x/tests/sim\t0.012s\n"
	got := ParseList(out)
	want := []string{"TestA", "TestB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRunRegexIsAnchoredAndMatchesExactlyTheShard(t *testing.T) {
	names := []string{"TestA", "TestAB", "TestC_x"}
	re := regexp.MustCompile(RunRegex([]string{"TestA", "TestC_x"}))
	for _, n := range []string{"TestA", "TestC_x"} {
		if !re.MatchString(n) {
			t.Errorf("%s should match", n)
		}
	}
	if re.MatchString(names[1]) {
		t.Error("TestAB must not match: pattern is not anchored")
	}
}

// TestLiveSimSuitePartitionIsComplete runs the real `go test -list` against
// tests/sim and requires the partition CI uses (K = 2, 3, 4) to cover the live
// suite exactly once. It is the static completeness check of #2228: a test
// that a shard's -run pattern would lose or double-run fails here.
func TestLiveSimSuitePartitionIsComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("lists tests/sim via go test")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	names, err := ListTests("../../tests/sim")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 100 {
		t.Fatalf("suspiciously few sim tests listed: %d", len(names))
	}
	for k := 2; k <= 4; k++ {
		shards := Partition(names, k)
		if err := Verify(names, shards); err != nil {
			t.Fatalf("k=%d: %v", k, err)
		}
		for i, s := range shards {
			if len(s) == 0 {
				t.Errorf("k=%d: shard %d is empty", k, i)
			}
		}
	}
}
