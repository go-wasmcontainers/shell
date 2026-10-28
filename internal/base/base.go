// Package base lays out the root filesystem of the shell base image:
// /bin/sh, the coreutils multicall module with one symbolic link per
// tool, a minimal /etc, /tmp and /root.
package base

import (
	"fmt"
	"slices"
	"strings"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/shell/internal/oci"
)

// Coreutils is the multicall module's file name under /bin. Each tool
// is a symbolic link to it; the module picks the tool from argv[0].
const Coreutils = "coreutils.wasm"

// PATH is the image's search path.
const PATH = "/bin"

// Shell is the image's SHELL: what a shell-form RUN, CMD or ENTRYPOINT
// runs (Docker's default is the same on Linux).
var Shell = []string{"/bin/sh", "-c"}

// Entries returns the layer of the image. tools are the names the
// multicall module answers to; a name that would shadow sh or the module
// itself, or that is not a plain file name, is refused.
func Entries(sh, coreutils []byte, tools []string, version string) ([]oci.Entry, error) {
	if len(sh) < 8 || string(sh[:4]) != "\x00asm" || len(coreutils) < 8 || string(coreutils[:4]) != "\x00asm" {
		return nil, fmt.Errorf("sh and coreutils must be WebAssembly modules")
	}
	es := []oci.Entry{
		{Path: "bin", Dir: true, Mode: 0o755},
		{Path: "bin/sh", Mode: 0o755, Data: sh},
		{Path: "bin/" + Coreutils, Mode: 0o755, Data: coreutils},
		{Path: "etc", Dir: true, Mode: 0o755},
		{Path: "etc/passwd", Mode: 0o644, Data: []byte("root:x:0:0:root:/root:/bin/sh\n")},
		{Path: "etc/group", Mode: 0o644, Data: []byte("root:x:0:\n")},
		{Path: "etc/os-release", Mode: 0o644, Data: []byte(
			"NAME=\"go-wasmcontainers shell\"\nID=wasmcontainers-shell\nVERSION_ID=" + version +
				"\nHOME_URL=\"https://github.com/go-wasmcontainers/shell\"\n")},
		{Path: "root", Dir: true, Mode: 0o700},
		{Path: "tmp", Dir: true, Mode: 0o1777},
	}
	sorted := slices.Clone(tools)
	slices.Sort(sorted)
	for i, t := range sorted {
		if i > 0 && sorted[i-1] == t {
			continue
		}
		if t == "" || t == "sh" || t == Coreutils || strings.ContainsAny(t, "/\x00") || t == "." || t == ".." {
			return nil, fmt.Errorf("tool name %q cannot be a /bin entry", t)
		}
		es = append(es, oci.Entry{Path: "bin/" + t, Link: Coreutils})
	}
	return es, nil
}

// Exec is the image's run configuration.
func Exec(version string) image.ExecConfig {
	return image.ExecConfig{
		Env:        []string{"PATH=" + PATH},
		Cmd:        []string{"/bin/sh"},
		WorkingDir: "/",
		Shell:      Shell,
		Labels: map[string]string{
			"org.opencontainers.image.source":  "https://github.com/go-wasmcontainers/shell",
			"org.opencontainers.image.version": version,
			"org.opencontainers.image.title":   "shell",
			"org.opencontainers.image.description": "POSIX sh and coreutils for WASI (wasip1) containers; " +
				"programs start through the WASIX process subset",
			"org.opencontainers.image.licenses": "BSD-3-Clause",
		},
	}
}
