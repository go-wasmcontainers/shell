package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func modules(t *testing.T) (string, string) {
	d := t.TempDir()
	sh, cu := filepath.Join(d, "sh.wasm"), filepath.Join(d, "cu.wasm")
	os.WriteFile(sh, []byte("\x00asm\x01\x00\x00\x00sh"), 0o644)
	os.WriteFile(cu, []byte("\x00asm\x01\x00\x00\x00cu"), 0o644)
	return sh, cu
}

func TestLayoutAndRootfs(t *testing.T) {
	sh, cu := modules(t)
	d := t.TempDir()
	out, err := run(t, "", "--sh", sh, "--coreutils", cu, "--version", "0.1.0",
		"--layout", filepath.Join(d, "layout"), "--rootfs", filepath.Join(d, "root"))
	if err != nil {
		t.Fatal(err, out)
	}
	if l, err := os.Readlink(filepath.Join(d, "root", "bin", "cat")); err != nil || l != "coreutils.wasm" {
		t.Errorf("bin/cat: %q %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(d, "layout", "index.json")); err != nil {
		t.Error(err)
	}
	// Reproducible: the same inputs give the same manifest.
	out2, err := run(t, "", "--sh", sh, "--coreutils", cu, "--version", "0.1.0", "--layout", filepath.Join(d, "layout2"))
	if err != nil || strings.Split(out, "\n")[1] != strings.Split(out2, "\n")[0] {
		t.Errorf("not reproducible:\n%s\n%s", out, out2)
	}
	if _, err := run(t, "", "--sh", sh, "--coreutils", cu, "--version", "0.1.0", "--rootfs", filepath.Join(d, "root")); err == nil {
		t.Error("overwrote an existing rootfs")
	}
}

func TestRefusals(t *testing.T) {
	sh, cu := modules(t)
	for _, args := range [][]string{
		{"--sh", sh},
		{"--sh", sh, "--coreutils", cu, "--version", "v"},
		{"--sh", sh, "--coreutils", cu, "--version", "v", "--layout", t.TempDir(), "--created", "yesterday"},
		{"--sh", "/nope", "--coreutils", cu, "--version", "v", "--layout", t.TempDir()},
		{"--sh", sh, "--coreutils", "/nope", "--version", "v", "--layout", t.TempDir()},
		{"--sh", cu, "--coreutils", sh, "--version", "v", "--push", "noregistry", "-t", "x"},
		{"--sh", sh, "--coreutils", cu, "--version", "v", "--push", "127.0.0.1:1/org/x", "-t", "x", "--password-stdin"},
	} {
		if _, err := run(t, "tok\n", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
