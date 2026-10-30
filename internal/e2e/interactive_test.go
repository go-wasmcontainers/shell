package e2e

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	enginefs "github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/wasi"

	"github.com/go-wasmcontainers/shell/internal/base"
)

// TestInteractiveUnderEngine feeds commands to `sh` on its standard input,
// the way a terminal session does (sh -i, no -c): exit, PATH lookups after
// a pipeline, and the status at end of input.
func TestInteractiveUnderEngine(t *testing.T) {
	img := baseImageTree(t)
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	defer e.Close(context.Background())
	cases := []struct {
		name, args, stdin, out string
		code                   int
	}{
		{"exit builtin", "", "exit 4\necho no\n", "", 4},
		{"exit builtin -i", "-i", "exit 4\necho no\n", "", 4},
		{"exit after CRLF", "-i", "exit 4\r\n", "", 4},
		{"PATH after pipeline", "", "echo a | cat\nls /etc\necho b | cat\nls /etc\n", "a\ngroup\nos-release\npasswd\nb\ngroup\nos-release\npasswd\n", 0},
		{"PATH after pipeline -i", "-i", "echo a | cat\nls /etc\n", "a\ngroup\nos-release\npasswd\n", 0},
		{"EOF status true", "-i", "true\n", "", 0},
		{"EOF status false", "-i", "false\n", "", 1},
		{"EOF status 3", "-i", "sh -c 'exit 3'\n", "", 3},
		{"EOF after status 0", "-i", "false\ntrue\n", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errb, code := runStdin(t, e, img, c.args, c.stdin)
			if code != c.code || (c.out != "" && out != c.out) {
				t.Errorf("got %q exit %d, want %q exit %d; stderr %q", out, code, c.out, c.code, errb)
			}
		})
	}
}

func runStdin(t *testing.T, e *wazero.Executor, img *enginefs.FS, flag, stdin string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"sh"}
	if flag != "" {
		args = append(args, flag)
	}
	p, err := e.Start(ctx, runtime.Spec{
		FS: img.Clone(enginefs.Quota{}), Path: "/bin/sh", Args: args,
		Env:   []string{"PATH=" + base.PATH, "PWD=/"},
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Wait()
	return out.String(), errb.String(), res.ExitCode
}
