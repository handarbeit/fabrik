package sessionreap

import (
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"testing"
)

func procArgs2(argc int, path string, argv, env []string) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(argc))
	b = append(b, path...)
	b = append(b, 0, 0, 0)
	for _, a := range argv {
		b = append(b, a...)
		b = append(b, 0)
	}
	for _, e := range env {
		b = append(b, e...)
		b = append(b, 0)
	}
	return b
}

func TestParseProcArgs2(t *testing.T) {
	buf := procArgs2(2, "/usr/bin/x", []string{"x", "-a"}, []string{"A=1", "FABRIK_WORKER_OWNER=n1", "B="})
	env, err := parseProcArgs2(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 3 || !HasEnv(env, "FABRIK_WORKER_OWNER", "n1") || env[2] != "B=" {
		t.Fatalf("env = %q", env)
	}
	if v, ok := LookupEnv(env, "A"); !ok || v != "1" {
		t.Fatalf("LookupEnv A = %q,%v", v, ok)
	}
	if HasEnv(env, "FABRIK_WORKER_OWNER", "n") {
		t.Fatal("HasEnv must match the whole value")
	}
}

func TestParseProcArgs2Unreadable(t *testing.T) {
	// The 34-byte stub the kernel returns for SIP-protected binaries, and
	// truncated/garbage buffers, are unreadable — never an empty-but-valid env.
	for name, buf := range map[string][]byte{
		"empty":     nil,
		"short":     {1, 0, 0, 0},
		"no-path":   {1, 0, 0, 0, 'a', 'b'},
		"argc-over": procArgs2(5, "/x", []string{"x"}, nil),
		"negative":  procArgs2(-1, "/x", nil, nil),
	} {
		if _, err := parseProcArgs2(buf); !errors.Is(err, ErrEnvUnreadable) {
			t.Errorf("%s: err = %v, want ErrEnvUnreadable", name, err)
		}
	}
}

func TestEnvironSelf(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no environment reader on this platform")
	}
	t.Setenv("SESSIONREAP_ENVIRON_PROBE", "yes")
	// The exec-time environment is what is read, so a variable set after exec is
	// not visible — assert the reader works on a variable present at exec time.
	env, err := Environ(os.Getpid())
	if err != nil {
		t.Fatalf("Environ(self): %v", err)
	}
	if len(env) == 0 {
		t.Fatal("empty environment for self")
	}
	if _, ok := LookupEnv(env, "PATH"); !ok && os.Getenv("PATH") != "" {
		t.Fatal("PATH missing from self environment")
	}
}

func TestEnvironPid1Unreadable(t *testing.T) {
	if _, err := Environ(1); err == nil {
		t.Fatal("pid 1 environment must read as unreadable")
	}
}

func TestStartTokenSelf(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip()
	}
	if StartToken(os.Getpid()) == "" {
		t.Fatal("empty start token for self")
	}
}
