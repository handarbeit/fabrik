package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultEqualsLegacyPaths(t *testing.T) {
	bed := t.TempDir()
	l := Default(bed)
	for _, n := range Names {
		if got, want := l.Dir(n), filepath.Join(bed, ".fabrik", n); got != want {
			t.Errorf("Dir(%s) = %s, want %s", n, got, want)
		}
	}
	if got, want := l.WorktreePath("o-r", 7), filepath.Join(bed, ".fabrik", "worktrees", "o-r", "issue-7"); got != want {
		t.Errorf("WorktreePath = %s, want %s", got, want)
	}
	if got, want := l.WorktreePath("", 7), filepath.Join(bed, ".fabrik", "worktrees", "issue-7"); got != want {
		t.Errorf("flat WorktreePath = %s, want %s", got, want)
	}
	if !l.IsDefault() {
		t.Error("Default layout must report IsDefault")
	}
	if (Layout{Bed: bed}).Dir(Sessions) != l.Dir(Sessions) {
		t.Error("zero Root must behave as the default")
	}
}

func TestResolvePrecedenceAndForms(t *testing.T) {
	bed := t.TempDir()
	abs := filepath.Join(t.TempDir(), "ws")
	other := filepath.Join(t.TempDir(), "other")
	third := filepath.Join(t.TempDir(), "third")

	l, err := Resolve(bed, abs, other, third)
	if err != nil || l.Root != abs {
		t.Fatalf("flag must win: %v %+v", err, l)
	}
	l, _ = Resolve(bed, "", other, third)
	if l.Root != other {
		t.Errorf("env must beat yaml: %s", l.Root)
	}
	l, _ = Resolve(bed, "", "  ", third)
	if l.Root != third {
		t.Errorf("yaml used when others blank: %s", l.Root)
	}
	l, _ = Resolve(bed, "", "", "")
	if !l.IsDefault() || l.Root != filepath.Join(bed, ".fabrik") {
		t.Errorf("unset must be the default: %+v", l)
	}
	// Relative resolves against the bed, not the process cwd.
	l, _ = Resolve(bed, "scratch/ws", "", "")
	if l.Root != filepath.Join(bed, "scratch", "ws") {
		t.Errorf("relative root: %s", l.Root)
	}
	t.Setenv("HOME", "/home/someone")
	l, err = Resolve(bed, "~/ws", "", "")
	if err != nil || l.Root != "/home/someone/ws" {
		t.Errorf("tilde: %v %s", err, l.Root)
	}
}

func TestLoadReadsEnvAndConfig(t *testing.T) {
	bed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bed, ".fabrik"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgRoot := filepath.Join(t.TempDir(), "fromyaml")
	if err := os.WriteFile(filepath.Join(bed, ".fabrik", "config.yaml"), []byte("workspace_root: "+cfgRoot+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, "")
	l, err := Load(bed, "")
	if err != nil || l.Root != cfgRoot {
		t.Fatalf("yaml layer: %v %s", err, l.Root)
	}
	envRoot := filepath.Join(t.TempDir(), "fromenv")
	t.Setenv(EnvVar, envRoot)
	l, _ = Load(bed, "")
	if l.Root != envRoot {
		t.Errorf("env layer: %s", l.Root)
	}
	flagRoot := filepath.Join(t.TempDir(), "fromflag")
	l, _ = Load(bed, flagRoot)
	if l.Root != flagRoot {
		t.Errorf("flag layer: %s", l.Root)
	}
	// A malformed config is treated as unset.
	t.Setenv(EnvVar, "")
	if err := os.WriteFile(filepath.Join(bed, ".fabrik", "config.yaml"), []byte(":\n\t- bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err = Load(bed, "")
	if err != nil || !l.IsDefault() {
		t.Errorf("malformed config must read as unset: %v %+v", err, l)
	}
}

func TestSameAcrossSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !Same(link, real) || !Same(filepath.Join(link, "sub"), filepath.Join(real, "sub")) {
		t.Error("symlinked and resolved forms must compare equal")
	}
	// A not-yet-existing tail compares equal through the symlink too.
	if !Same(filepath.Join(link, "later", "x"), filepath.Join(real, "later", "x")) {
		t.Error("missing tail must resolve through its existing ancestor")
	}
	if Same(link, filepath.Join(real, "sub")) {
		t.Error("different paths must not compare equal")
	}
	if !Within(link, filepath.Join(real, "sub")) || Within(filepath.Join(real, "sub"), link) {
		t.Error("Within must follow resolution and direction")
	}
}

func TestPrepare(t *testing.T) {
	bed := t.TempDir()
	vol := t.TempDir()

	t.Run("creates leaf and subdirs", func(t *testing.T) {
		l := Layout{Bed: bed, Root: filepath.Join(vol, "ws")}
		if err := l.Prepare(); err != nil {
			t.Fatal(err)
		}
		for _, n := range Names {
			if fi, err := os.Stat(l.Dir(n)); err != nil || !fi.IsDir() {
				t.Errorf("%s not created", n)
			}
		}
	})
	t.Run("missing parent creates nothing", func(t *testing.T) {
		missing := filepath.Join(vol, "unmounted", "fabrik")
		l := Layout{Bed: bed, Root: missing}
		err := l.Prepare()
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("want error naming the path, got %v", err)
		}
		if _, serr := os.Stat(filepath.Join(vol, "unmounted")); !os.IsNotExist(serr) {
			t.Error("a missing mount path must never be created")
		}
	})
	t.Run("root is a file", func(t *testing.T) {
		f := filepath.Join(vol, "afile")
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := (Layout{Bed: bed, Root: f}).Prepare(); err == nil || !strings.Contains(err.Error(), f) {
			t.Errorf("want error naming %s, got %v", f, err)
		}
	})
	t.Run("not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores permissions")
		}
		ro := filepath.Join(vol, "ro")
		if err := os.Mkdir(ro, 0o755); err != nil {
			t.Fatal(err)
		}
		// Pre-create subdirs so only the probe can fail.
		l := Layout{Bed: bed, Root: ro}
		for _, n := range Names {
			if err := os.Mkdir(l.Dir(n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(ro, 0o755)
		if err := l.Prepare(); err == nil || !strings.Contains(err.Error(), ro) {
			t.Errorf("want write error naming %s, got %v", ro, err)
		}
	})
	t.Run("default creates nothing", func(t *testing.T) {
		b := t.TempDir()
		if err := Default(b).Prepare(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(b, ".fabrik")); !os.IsNotExist(err) {
			t.Error("the default layout must leave lazy creation alone")
		}
	})
}

func mkWorktree(t *testing.T, root, ownerRepo string, n string) string {
	t.Helper()
	p := filepath.Join(root, Worktrees, ownerRepo, "issue-"+n)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func snapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestCheckMigration(t *testing.T) {
	t.Run("pre-feature bed with live worktrees refuses and changes nothing", func(t *testing.T) {
		bed := t.TempDir()
		def := Default(bed)
		wt := mkWorktree(t, def.root(), "o-r", "5")
		l := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "ws")}
		before := snapshot(t, bed)
		err := l.Establish()
		if err == nil {
			t.Fatal("want refusal")
		}
		for _, want := range []string{def.root(), l.Root, wt, "set workspace_root back"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q: %v", want, err)
			}
		}
		if after := snapshot(t, bed); strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Error("refusal must not modify the bed")
		}
		if _, serr := os.Stat(l.Root); !os.IsNotExist(serr) {
			t.Error("the new root must not be created before the refusal check")
		}
	})
	t.Run("no worktrees accepts and records", func(t *testing.T) {
		bed := t.TempDir()
		l := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "ws")}
		if err := l.Establish(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(l.markerPath())
		if err != nil || strings.TrimSpace(string(data)) != l.Root {
			t.Fatalf("marker: %v %q", err, data)
		}
		// Same setting on restart passes silently.
		if err := l.Establish(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("external to external change refuses", func(t *testing.T) {
		bed := t.TempDir()
		a := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "a")}
		if err := a.Establish(); err != nil {
			t.Fatal(err)
		}
		mkWorktree(t, a.Root, "o-r", "9")
		b := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "b")}
		if err := b.Establish(); err == nil || !strings.Contains(err.Error(), a.Root) {
			t.Fatalf("want refusal naming %s, got %v", a.Root, err)
		}
	})
	t.Run("back to unset refuses while the old root is live, then clears the marker", func(t *testing.T) {
		bed := t.TempDir()
		ext := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "ws")}
		if err := ext.Establish(); err != nil {
			t.Fatal(err)
		}
		wt := mkWorktree(t, ext.Root, "o-r", "3")
		def := Default(bed)
		if err := def.Establish(); err == nil {
			t.Fatal("want refusal")
		}
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		if err := def.Establish(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(def.markerPath()); !os.IsNotExist(err) {
			t.Error("unset must leave no marker")
		}
	})
	t.Run("unset bed never gains a marker", func(t *testing.T) {
		bed := t.TempDir()
		if err := Default(bed).Establish(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(bed, ".fabrik")); !os.IsNotExist(err) {
			t.Error("unset must create nothing")
		}
	})
	t.Run("corrupt marker is treated as the default root", func(t *testing.T) {
		bed := t.TempDir()
		l := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "ws")}
		if err := os.MkdirAll(l.StateDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.markerPath(), []byte("\x00garbage not a path"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := l.Establish(); err != nil {
			t.Fatalf("a corrupt marker must not brick a healthy start: %v", err)
		}
		mkWorktree(t, Default(bed).root(), "o-r", "1")
		other := Layout{Bed: bed, Root: filepath.Join(t.TempDir(), "w2")}
		_ = os.WriteFile(other.markerPath(), []byte(""), 0o600)
		if err := other.Establish(); err == nil {
			t.Error("a corrupt marker must still catch live work in the default root")
		}
	})
	t.Run("symlinked spelling of the same root is not a change", func(t *testing.T) {
		bed := t.TempDir()
		base := t.TempDir()
		real := filepath.Join(base, "real")
		_ = os.Mkdir(real, 0o755)
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		a := Layout{Bed: bed, Root: real}
		if err := a.Establish(); err != nil {
			t.Fatal(err)
		}
		mkWorktree(t, real, "o-r", "4")
		if err := (Layout{Bed: bed, Root: link}).Establish(); err != nil {
			t.Errorf("same root through a symlink must pass: %v", err)
		}
	})
}
