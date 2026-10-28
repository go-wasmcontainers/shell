package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/image"
	enginefs "github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/wasi"

	"github.com/go-wasmcontainers/shell/internal/base"
	"github.com/go-wasmcontainers/shell/internal/oci"
	"github.com/wasmdesk/coreutils/multicall"
)

// TestBaseImageUnderEngine runs the base image in the go-wasmcontainers
// engine itself (wazero, the engine's WASI host with the WASIX module):
// the same layer mkimage publishes, sh and the real coreutils, pipelines
// included — what wasmer cannot run (docs/REFERENCES.md).
func TestBaseImageUnderEngine(t *testing.T) {
	img := baseImageTree(t)
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	defer e.Close(context.Background())
	runCases(t, e, img)
}

// baseImageTree builds the base image's layer and unpacks it into a
// frozen engine tree.
func baseImageTree(t *testing.T) *enginefs.FS {
	t.Helper()
	tmp := t.TempDir()
	shPath, cuPath := filepath.Join(tmp, "sh.wasm"), filepath.Join(tmp, "coreutils.wasm")
	build(t, "github.com/go-wasmcontainers/shell/cmd/sh", shPath)
	build(t, "github.com/wasmdesk/coreutils/cmd/multicall", cuPath)
	sh, err := os.ReadFile(shPath)
	if err != nil {
		t.Fatal(err)
	}
	cu, err := os.ReadFile(cuPath)
	if err != nil {
		t.Fatal(err)
	}
	es, err := base.Entries(sh, cu, multicall.Names(), "test")
	if err != nil {
		t.Fatal(err)
	}
	layer, _, err := oci.Layer(es, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	img := enginefs.New()
	if err := img.ApplyLayer(bytes.NewReader(layer), image.MediaTypeOCILayerGz, "", enginefs.NewBudget(enginefs.DefaultLimits)); err != nil {
		t.Fatal(err)
	}
	img.Freeze()
	return img
}

func runCases(t *testing.T, e *wazero.Executor, img *enginefs.FS) {
	cases := []struct {
		script, out string
		code        int
	}{
		{"echo a | cat; exit 3", "a\n", 3},
		{"seq 5 | sort -r | head -n 2", "5\n4\n", 0},
		{"printf 'b\\na\\nb\\n' | sort | uniq -c", "      1 a\n      2 b\n", 0},
		{"cat /etc/passwd | wc -l", "1\n", 0},
		{"grep -c root /etc/passwd || true", "", 0},
		{"i=0; while [ $i -lt 20000 ]; do echo line$i; i=$((i+1)); done | cat | wc -l", "20000\n", 0},
		{"cd /tmp && echo x > f && ls && cat f | tr x y", "f\ny\n", 0},
		{"sh -c 'exit 7'; echo $?", "7\n", 0},
		{"nosuch 2>/dev/null; echo $?", "127\n", 0},
		{"x=$(echo hi | cat); echo \"[$x]\"", "[hi]\n", 0},
		{"false | true; echo $?", "0\n", 0},
	}
	for _, c := range cases {
		t.Run(c.script, func(t *testing.T) {
			var out, errb bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			p, err := e.Start(ctx, runtime.Spec{
				FS: img.Clone(enginefs.Quota{}), Path: "/bin/sh",
				Args: []string{"sh", "-c", c.script}, Env: []string{"PATH=" + base.PATH, "PWD=/"},
				Stdout: &out, Stderr: &errb,
			})
			if err != nil {
				t.Fatal(err)
			}
			res := p.Wait()
			if c.out != "" || c.code != 0 {
				if out.String() != c.out || res.ExitCode != c.code {
					t.Errorf("got %q exit %d (%s), want %q exit %d; stderr %s", out.String(), res.ExitCode, res.Error, c.out, c.code, strings.TrimSpace(errb.String()))
				}
			} else if res.ExitCode != 0 {
				t.Errorf("exit %d (%s); stderr %s", res.ExitCode, res.Error, errb.String())
			}
		})
	}
}
