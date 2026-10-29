package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstError(t *testing.T) {
	out := []byte("go: downloading example.org/y v1.0.0\n# example.org/x\n\n  x.go:3:2: undefined: syscall.Statfs\nmore\n")
	if got := firstError(out); got != "x.go:3:2: undefined: syscall.Statfs" {
		t.Errorf("%q", got)
	}
	if got := firstError(nil); got == "" {
		t.Error("empty output gave no message")
	}
}

// A planted failure and a planted success, through the same path as a
// real build: the report must tell them apart.
func TestCheckAndReport(t *testing.T) {
	fake := func(dir, pkg, out string) ([]byte, error) {
		if strings.Contains(pkg, "bad") {
			return []byte("# bad\nbad.go:1:1: undefined: unix.Termios\n"), errors.New("exit status 1")
		}
		return nil, os.WriteFile(out, make([]byte, 2_000_000), 0o644)
	}
	rs := check(fake, ".", t.TempDir(), []string{"./good", "./bad"})
	var b bytes.Buffer
	report(&b, rs)
	want := "PASS\t./good\t2.0 MB\nFAIL\t./bad\tbad.go:1:1: undefined: unix.Termios\n"
	if b.String() != want {
		t.Errorf("%q\nwant %q", b.String(), want)
	}
}

func TestRootCmd(t *testing.T) {
	cmd := RootCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("no package accepted")
	}
}

// The real builder on a package of this module that is known to build
// for wasip1, and one that cannot exist.
func TestGoBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds")
	}
	root, _ := filepath.Abs("../../..")
	rs := check(goBuild, root, t.TempDir(), []string{"./cmd/sh", "./no/such/package"})
	if !rs[0].OK || rs[1].OK || rs[1].Why == "" {
		t.Errorf("%+v", rs)
	}
}
