package pruefer

import (
	"strings"
	"testing"
)

const baseDiffA = "diff --git a/a.go b/a.go\n" +
	"index 1111111..2222222 100644\n" +
	"--- a/a.go\n" +
	"+++ b/a.go\n" +
	"@@ -10,3 +10,4 @@ func old() {\n" +
	" ctx1\n" +
	"+added\n" +
	" ctx2\n" +
	" ctx3\n"

func mustPrint(t *testing.T, d string) patchPrint {
	t.Helper()
	p, ok := patchFingerprint(d)
	if !ok {
		t.Fatalf("patchFingerprint not comparable for:\n%s", d)
	}
	return p
}

func TestPatchFingerprint_IgnoresIndexAndHunkPositions(t *testing.T) {
	shifted := strings.NewReplacer(
		"index 1111111..2222222", "index aaaaaaa..bbbbbbb",
		"@@ -10,3 +10,4 @@ func old() {", "@@ -250,3 +262,4 @@ func renamedSince() {",
	).Replace(baseDiffA)
	if shifted == baseDiffA {
		t.Fatal("test bug: replacement changed nothing")
	}
	if mustPrint(t, baseDiffA).Whole != mustPrint(t, shifted).Whole {
		t.Fatal("index/@@ position drift must not change the fingerprint")
	}
}

func TestPatchFingerprint_RealChangesDiffer(t *testing.T) {
	want := mustPrint(t, baseDiffA).Whole
	cases := map[string]string{
		"changed added line":      strings.Replace(baseDiffA, "+added", "+Added", 1),
		"whitespace only":         strings.Replace(baseDiffA, "+added", "+added ", 1),
		"crlf":                    strings.Replace(baseDiffA, "+added\n", "+added\r\n", 1),
		"changed context":         strings.Replace(baseDiffA, " ctx2", " ctx2x", 1),
		"extra no-newline marker": baseDiffA + "\\ No newline at end of file\n",
		"mode change":             strings.Replace(baseDiffA, "index 1111111..2222222 100644", "old mode 100644\nnew mode 100755\nindex 1111111..2222222", 1),
		"other path":              strings.NewReplacer("a/a.go", "a/b.go", "b/a.go", "b/b.go").Replace(baseDiffA),
		"extra file":              baseDiffA + "diff --git a/c.go b/c.go\nindex 3..4 100644\n--- a/c.go\n+++ b/c.go\n@@ -1 +1 @@\n-x\n+y\n",
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if mustPrint(t, d).Whole == want {
				t.Fatalf("%s: fingerprint unchanged", name)
			}
		})
	}
}

func TestPatchFingerprint_RenameLinesAreHashed(t *testing.T) {
	a := "diff --git a/old.go b/new.go\nsimilarity index 90%\nrename from old.go\nrename to new.go\nindex 1..2 100644\n--- a/old.go\n+++ b/new.go\n@@ -1 +1 @@\n-x\n+y\n"
	b := strings.Replace(a, "similarity index 90%", "similarity index 80%", 1)
	if mustPrint(t, a).Whole == mustPrint(t, b).Whole {
		t.Fatal("rename similarity must be part of the fingerprint")
	}
}

func TestPatchFingerprint_FileOrderIrrelevant(t *testing.T) {
	f2 := "diff --git a/z.go b/z.go\nindex 3..4 100644\n--- a/z.go\n+++ b/z.go\n@@ -1 +1 @@\n-x\n+y\n"
	if mustPrint(t, baseDiffA+f2).Whole != mustPrint(t, f2+baseDiffA).Whole {
		t.Fatal("file order must not matter")
	}
}

func TestPatchFingerprint_NotComparable(t *testing.T) {
	cases := map[string]string{
		"empty":    "",
		"preamble": "garbage\n" + baseDiffA,
		"binary":   "diff --git a/x.png b/x.png\nindex 1..2 100644\nBinary files a/x.png and b/x.png differ\n",
		"gitbin":   "diff --git a/x.bin b/x.bin\nindex 1..2 100644\nGIT binary patch\nliteral 1\nAcmZ\n",
		"dup path": baseDiffA + baseDiffA,
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := patchFingerprint(d); ok {
				t.Fatalf("%s must not be comparable", name)
			}
		})
	}
}

func TestPatchFingerprint_PerFileKeys(t *testing.T) {
	f2 := "diff --git a/z.go b/z.go\nindex 3..4 100644\n--- a/z.go\n+++ b/z.go\n@@ -1 +1 @@\n-x\n+y\n"
	p := mustPrint(t, baseDiffA+f2)
	if len(p.PerFile) != 2 || p.PerFile["a.go"] == "" || p.PerFile["z.go"] == "" {
		t.Fatalf("PerFile = %v", p.PerFile)
	}
}
