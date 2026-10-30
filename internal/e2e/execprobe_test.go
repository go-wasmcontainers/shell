package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enginefs "github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/wasi"

	"github.com/go-wasmcontainers/shell/internal/base"
)

// TestExecPackageUnderEngine runs wasix/exec from a guest program in the
// base image, under the engine: LookPath, Output, ExitError, pipes, Dir,
// Env, CombinedOutput, a file handed to the child.
func TestExecPackageUnderEngine(t *testing.T) {
	img := baseImageTree(t)
	probe := filepath.Join(t.TempDir(), "execprobe.wasm")
	build(t, "github.com/go-wasmcontainers/shell/internal/e2e/testdata/execprobe", probe)
	mod, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	fsys := img.Clone(enginefs.Quota{})
	if err := fsys.WriteFile("/bin/execprobe.wasm", mod, 0o755); err != nil {
		t.Fatal(err)
	}
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	defer e.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out, errb bytes.Buffer
	p, err := e.Start(ctx, runtime.Spec{
		FS: fsys, Path: "/bin/execprobe.wasm", Args: []string{"execprobe"},
		Env:    []string{"PATH=" + base.PATH, "PWD=/"},
		Stdout: &out, Stderr: &errb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := p.Wait(); res.ExitCode != 0 {
		t.Errorf("exit %d (%s); stderr %s", res.ExitCode, res.Error, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{"lookpath", "output", "exiterror", "pipes", "dir-env", "combined", "file-stdout"}
	oks := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "FAIL") {
			t.Error(l)
		}
		if strings.HasPrefix(l, "ok ") {
			oks++
		}
	}
	if oks != len(want) || !strings.Contains(out.String(), "direct\n") {
		t.Errorf("%d of %d checks passed; output:\n%s\nstderr: %s", oks, len(want), out.String(), errb.String())
	}
}
