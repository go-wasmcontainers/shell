package e2e

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/wasi"
	"github.com/wasmdesk/coreutils/multicall"

	"github.com/go-wasmcontainers/shell/internal/base"
)

// TestExecIntoBaseImage is `wctr exec c sh -c 'ls /bin | wc -l'` against a
// running container of the base image, through the engine's container
// manager (what the exec endpoints drive): a further process in the
// container, its pipeline spawned through WASIX, its exit code reported.
func TestExecIntoBaseImage(t *testing.T) {
	img := baseImageTree(t)
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	defer e.Close(context.Background())
	m := container.NewManager(container.Options{Executor: e})
	c, err := m.Create(container.CreateOptions{Name: "/c", Lower: img, Config: container.Config{
		Image: "shell", ImageID: "sha256:test",
		Cmd: []string{"/bin/sh", "-c", "while :; do sleep 1; done"}, Env: []string{"PATH=" + base.PATH},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Start(ctx, c.ID()); err != nil {
		t.Fatal(err)
	}
	defer func() { m.Kill(c.ID()); m.Wait(ctx, c.ID(), container.WaitNotRunning) }()

	run := func(cmd ...string) (string, int) {
		t.Helper()
		x, err := m.CreateExec("c", container.ExecConfig{Cmd: cmd, AttachStdout: true, AttachStderr: true})
		if err != nil {
			t.Fatal(err)
		}
		var out, errb syncBuf
		if err := m.StartExec(ctx, x, container.ExecIO{Stdout: &out, Stderr: &errb}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-x.Done():
		case <-time.After(2 * time.Minute):
			t.Fatalf("%v did not end", cmd)
		}
		if s := errb.String(); s != "" {
			t.Logf("%v stderr: %s", cmd, s)
		}
		return out.String(), x.Info().ExitCode
	}
	// /bin holds sh, coreutils.wasm and one link per tool.
	want := fmt.Sprintf("%d\n", len(multicall.Names())+2)
	if out, code := run("sh", "-c", "ls /bin | wc -l"); out != want || code != 0 {
		t.Errorf("ls /bin | wc -l: %q exit %d, want %q", out, code, want)
	}
	if out, code := run("sh", "-c", "echo $HOSTNAME | wc -c; exit 5"); strings.TrimSpace(out) != "13" || code != 5 {
		t.Errorf("hostname and exit status: %q %d", out, code)
	}
}

// syncBuf is written by the processes of a pipeline, concurrently.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
