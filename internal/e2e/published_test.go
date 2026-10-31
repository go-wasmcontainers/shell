package e2e

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/image"
	enginefs "github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/wasi"
)

// TestPublishedImage pulls a published shell image with the engine and
// checks what a release must carry: ls FILE (coreutils >= v0.5), xargs and
// find -exec (>= v0.6), the PATH and the config's Cmd. Run it with
// SHELL_E2E_IMAGE=ghcr.io/go-wasmcontainers/shell:<version>.
func TestPublishedImage(t *testing.T) {
	ref := os.Getenv("SHELL_E2E_IMAGE")
	if ref == "" {
		t.Skip("SHELL_E2E_IMAGE not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src, _, err := image.OpenSource(ref, image.RegistryOptions{}, image.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	st, err := image.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := image.Pull(ctx, st, src, image.DefaultLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s, %d layer(s), labels %v", ref, res.RepoDigest, len(res.Image.Manifest.Layers), res.Image.Config.Config.Labels)
	tree := enginefs.New()
	for _, l := range res.Image.Manifest.Layers {
		rc, err := st.Open(l.Digest)
		if err != nil {
			t.Fatal(err)
		}
		err = tree.ApplyLayer(rc, l.MediaType, "", enginefs.NewBudget(enginefs.DefaultLimits))
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	tree.Freeze()
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	defer e.Close(context.Background())
	for _, c := range []struct{ script, out string }{
		{"echo ok > /ready; ls /ready", "/ready\n"},
		{"printf 'a b\\n' | xargs -n1 echo +", "+ a\n+ b\n"},
		{"find /etc -name passwd -exec cat {} ';'", "root:x:0:0:root:/root:/bin/sh\n"},
	} {
		var out, errb bytes.Buffer
		p, err := e.Start(ctx, runtime.Spec{
			FS: tree.Clone(enginefs.Quota{}), Path: "/bin/sh", Args: []string{"sh", "-c", c.script},
			Env: append(res.Image.Config.Config.Env, "PWD=/"), Stdout: &out, Stderr: &errb,
		})
		if err != nil {
			t.Fatal(err)
		}
		if r := p.Wait(); r.ExitCode != 0 || out.String() != c.out {
			t.Errorf("%q: %q exit %d, want %q; stderr %s", c.script, out.String(), r.ExitCode, c.out, strings.TrimSpace(errb.String()))
		}
	}
}
