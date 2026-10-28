package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSh(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	code := 0
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	cmd := RootCmd()
	var out, errb bytes.Buffer
	// A file, like a real stdin: the interpreter would otherwise copy a
	// plain reader through a pipe and race the parser for its bytes.
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte(stdin), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd.SetIn(f)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestCommandString(t *testing.T) {
	out, _, code := runSh(t, "", "-c", `echo "$0:$1:$#"; exit 4`, "name", "a b")
	if out != "name:a b:1\n" || code != 4 {
		t.Errorf("got %q %d", out, code)
	}
	out, _, _ = runSh(t, "", "-c", `echo $0`)
	if out != "sh\n" {
		t.Errorf("$0 = %q", out)
	}
}

func TestOptions(t *testing.T) {
	_, _, code := runSh(t, "", "-ec", "false; echo no")
	if code != 1 {
		t.Errorf("-e: %d", code)
	}
	_, errs, code := runSh(t, "", "-uc", "echo $NOPE")
	if code == 0 || !strings.Contains(errs, "NOPE") {
		t.Errorf("-u: %d %q", code, errs)
	}
	_, errs, _ = runSh(t, "", "-xc", "true")
	if !strings.Contains(errs, "+ true") {
		t.Errorf("-x: %q", errs)
	}
	out, _, _ := runSh(t, "", "-fc", "echo /*")
	if out != "/*\n" {
		t.Errorf("-f: %q", out)
	}
	out, _, _ = runSh(t, "", "-ac", "X=1; echo $X")
	if out != "1\n" {
		t.Errorf("-a: %q", out)
	}
	_, errs, code = runSh(t, "", "-c")
	if code != 2 || !strings.Contains(errs, "requires an argument") {
		t.Errorf("-c alone: %d %q", code, errs)
	}
	_, errs, code = runSh(t, "", "-c", "if")
	if code != 2 || errs == "" {
		t.Errorf("syntax error: %d %q", code, errs)
	}
}

func TestVersion(t *testing.T) {
	out, _, _ := runSh(t, "", "--version")
	if !strings.HasPrefix(out, "sh (go-wasmcontainers/shell) ") {
		t.Errorf("%q", out)
	}
}

func TestScriptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.sh")
	os.WriteFile(p, []byte("echo \"$0 $*\"\nexit 3\n"), 0o644)
	out, _, code := runSh(t, "", p, "x", "y")
	if out != p+" x y\n" || code != 3 {
		t.Errorf("%q %d", out, code)
	}
	_, errs, code := runSh(t, "", "/no/such/script")
	if code != 127 || errs == "" {
		t.Errorf("missing script: %d %q", code, errs)
	}
}

func TestStdinScript(t *testing.T) {
	out, _, code := runSh(t, "echo one\necho two $1\nexit 5\n", "-s", "arg")
	if out != "one\ntwo arg\n" || code != 5 {
		t.Errorf("%q %d", out, code)
	}
	out, errs, _ := runSh(t, "if then\necho after\n", "-i")
	if !strings.Contains(errs, "$ ") || !strings.Contains(errs, "sh:") || out != "after\n" {
		t.Errorf("interactive: %q %q", out, errs)
	}
	out, errs, _ = runSh(t, "if true; then\necho in\nfi\n", "-i")
	if !strings.Contains(errs, "> ") || out != "in\n" {
		t.Errorf("continuation: %q %q", out, errs)
	}
	_, _, code = runSh(t, "if then\necho after\n")
	if code != 2 {
		t.Errorf("non-interactive syntax error: %d", code)
	}
}

func TestNoHostSpawn(t *testing.T) {
	// Outside wasip1 there is no WASIX host: starting a program must fail
	// cleanly with 126 and say why.
	p := filepath.Join(t.TempDir(), "prog")
	os.WriteFile(p, []byte("\x00asm\x01\x00\x00\x00"), 0o755)
	_, errs, code := runSh(t, "", "-c", p)
	if code != 126 || !strings.Contains(errs, "WASIX") {
		t.Errorf("%d %q", code, errs)
	}
}
