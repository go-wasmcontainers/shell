// Package e2e runs the wasip1 shell under an independent WASIX host
// (wasmer), which is the oracle for the guest side of the ABI.
//
// The test is skipped when no wasmer binary is found (set WASMER to its
// path); CI sets E2E_REQUIRED=1 so that a missing runtime fails instead.
package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wasmer(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WASMER"); p != "" {
		return p
	}
	if p, err := exec.LookPath("wasmer"); err == nil {
		return p
	}
	if os.Getenv("E2E_REQUIRED") != "" {
		t.Fatal("wasmer not found and E2E_REQUIRED is set")
	}
	t.Skip("wasmer not found (set WASMER)")
	return ""
}

func build(t *testing.T, pkg, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// rootfs lays out a container root: /bin/sh, the tbox tools (some as
// <name>.wasm, the pkgx bottle layout), two scripts and /tmp.
func rootfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	for _, d := range []string{bin, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	build(t, "github.com/go-wasmcontainers/shell/cmd/sh", filepath.Join(bin, "sh"))
	tbox := filepath.Join(t.TempDir(), "tbox.wasm")
	build(t, "./testdata/tbox", tbox)
	for _, n := range []string{"cat", "echo", "pwd", "env", "fail"} {
		copyFile(t, tbox, filepath.Join(bin, n))
	}
	for _, n := range []string{"argv", "wc"} {
		copyFile(t, tbox, filepath.Join(bin, n+".wasm"))
	}
	scripts := map[string]string{
		"hello": "#!/bin/sh\necho hello \"$1\"\n",
		"plain": "echo plain $#\n",
	}
	for n, s := range scripts {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestBaseImageUnderWasmer builds the base image's root filesystem with
// mkimage (the real coreutils multicall module, linked under /bin) and
// runs sh in it. Coreutils reading a pipe is left to the engine's tests:
// under wasmer a Go child whose stdin is a pipe trips wasmer's fdstat of
// fds 0-2 (docs/REFERENCES.md); the tbox tests above cover the shell's
// side of pipes.
func TestBaseImageUnderWasmer(t *testing.T) {
	w := wasmer(t)
	tmp := t.TempDir()
	sh, cu := filepath.Join(tmp, "sh.wasm"), filepath.Join(tmp, "coreutils.wasm")
	build(t, "github.com/go-wasmcontainers/shell/cmd/sh", sh)
	build(t, "github.com/wasmdesk/coreutils/cmd/multicall", cu)
	root := filepath.Join(tmp, "root")
	mk := exec.Command("go", "run", "github.com/go-wasmcontainers/shell/cmd/mkimage",
		"--sh", sh, "--coreutils", cu, "--version", "test", "--rootfs", root)
	if b, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("mkimage: %v\n%s", err, b)
	}
	cases := []struct{ script, out string }{
		{"cat /etc/passwd", "root:x:0:0:root:/root:/bin/sh\n"},
		{"wc -l /etc/passwd", "1 /etc/passwd\n"},
		{"wc -l < /etc/passwd", "1\n"},
		{"cd /tmp && touch f && ls", "f\n"},
		{"mkdir -p /tmp/a/b && cd /tmp/a && pwd && ls", "/tmp/a\nb/\n"}, // this ls marks directories with a slash
		{"echo hi > /tmp/x && cat /tmp/x && rm /tmp/x && ls /tmp", "hi\n"},
		{"basename /a/b/c.txt .txt; dirname /a/b/c", "c\n/a/b\n"},
		{"seq 3 > /tmp/s; sort -r /tmp/s", "3\n2\n1\n"},
		{"grep root /etc/passwd > /dev/null; echo $?; grep nobody /etc/passwd; echo $?", "0\n1\n"},
		{"true && false; echo $?", "1\n"},
	}
	for _, c := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, w, "run", "--volume", root+":/",
			"--env", "PATH=/bin", "--env", "PWD=/", filepath.Join(root, "bin", "sh"), "--", "-c", c.script)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		cancel()
		if err != nil || out.String() != c.out {
			t.Errorf("%q: got %q (%v), want %q; stderr %s", c.script, out.String(), err, c.out, strings.TrimSpace(errb.String()))
		}
	}
}

// TestProbe runs the script in the file $E2E_PROBE and logs its output
// and time: a tool for investigating, not a check.
func TestProbe(t *testing.T) {
	file := os.Getenv("E2E_PROBE")
	if file == "" {
		t.Skip("E2E_PROBE not set")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	w := wasmer(t)
	root := rootfs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, w, "run", "--volume", root+":/",
		"--env", "PATH=/bin", "--env", "PWD=/", filepath.Join(root, "bin", "sh"), "--", "-c", script)
	out, err := cmd.CombinedOutput()
	t.Logf("%v in %v\n%s", err, time.Since(start), out)
}

func TestShellUnderWasmer(t *testing.T) {
	w := wasmer(t)
	root := rootfs(t)
	cases := []struct {
		name, script, out string
		code              int
	}{
		{"pipe and exit status", "echo a | cat; exit 3", "a\n", 3},
		{"external pipeline", "/bin/echo x | cat | cat - | wc", "1\n", 0},
		{"argv survives empty and newline args", "argv '' 'a b' 'c\nd'", "[][a b][c\nd]\n", 0},
		{"cd, redirections", "cd /tmp && pwd && echo hi > f && cat < f", "/tmp\nhi\n", 0},
		{"external pwd follows cd", "cd /tmp; pwd; /bin/pwd", "/tmp\n/tmp\n", 0},
		{"env reaches nested sh", "FOO=bar sh -c 'echo $FOO'", "bar\n", 0},
		{"exit status in $?", "fail 7; echo $?", "7\n", 0},
		{"not found is 127", "nosuch 2>/dev/null; echo $?", "127\n", 0},
		{"shebang script", "hello w", "hello w\n", 0},
		{"script without shebang", "plain 1 2", "plain 2\n", 0},
		{"nested spawn", "sh -c \"sh -c 'echo deep'\"", "deep\n", 0},
		{"command substitution of a pipeline", "x=$(echo sub | cat); echo \"[$x]\"", "[sub]\n", 0},
		{"here-document to a child", "cat <<EOF\nline\nEOF", "line\n", 0},
		{"set -e", "set -e; fail 4; echo no", "", 4},
		{"exec", "exec fail 5; echo no", "", 5},
		{"stdin of the shell reaches a child", "cat", "", 0},
		// builtin -> child -> builtin through fd_pipe pipes.
		{"pipeline 100", "i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done | cat | { n=0; while read l; do n=$((n+1)); done; echo $n; }", "100\n", 0},
		{"pipeline 100 to child", "i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done | cat | wc", "100\n", 0},
		{"pipeline 1000 to child", "i=0; while [ $i -lt 1000 ]; do echo line$i; i=$((i+1)); done | wc", "1000\n", 0},
		{"pipeline exit status is the last command's", "fail 3 | cat; echo $?", "0\n", 0},
		{"stderr to a pipe", "fail 0 2>&1 | wc", "0\n", 0},
	}
	// Larger pipelines make a goroutine of the shell park on the poller
	// waiting for a pipe end; wasmer 7.5.0's poll_oneoff never returns
	// then (see wasix.TestPipeBetweenGoroutines), so they only run on
	// hosts that poll pipes (WASIX_HOST_POLLS_PIPES=1).
	if os.Getenv("WASIX_HOST_POLLS_PIPES") != "" {
		cases = append(cases, []struct {
			name, script, out string
			code              int
		}{
			{"pipeline 20000 to child", "i=0; while [ $i -lt 20000 ]; do echo line$i; i=$((i+1)); done | wc", "20000\n", 0},
			{"large pipeline", "i=0; while [ $i -lt 20000 ]; do echo line$i; i=$((i+1)); done | cat | { n=0; while read l; do n=$((n+1)); done; echo $n; }", "20000\n", 0},
		}...)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, w, "run", "--volume", root+":/",
				"--env", "PATH=/bin", "--env", "PWD=/",
				filepath.Join(root, "bin", "sh"), "--", "-c", c.script)
			var out, errb bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errb
			err := cmd.Run()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if out.String() != c.out || code != c.code {
				t.Errorf("script %q:\n got %q, exit %d\nwant %q, exit %d\nstderr: %s",
					c.script, out.String(), code, c.out, c.code, strings.TrimSpace(errb.String()))
			}
		})
	}
}
