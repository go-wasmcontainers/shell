# shell

[![Docs](https://img.shields.io/badge/docs-hugo%20%2B%20hextra-0079A8)](https://go-wasmcontainers.github.io/docs/)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![CI](https://github.com/go-wasmcontainers/shell/actions/workflows/ci.yml/badge.svg)](https://github.com/go-wasmcontainers/shell/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-wasmcontainers/shell.svg)](https://pkg.go.dev/github.com/go-wasmcontainers/shell)
![status](https://img.shields.io/badge/status-guest%20side-yellow)

**A POSIX shell for WASI containers**, in pure Go, built for
`GOOS=wasip1 GOARCH=wasm`. It is what makes shell-form commands work in
[go-wasmcontainers](https://go-wasmcontainers.github.io/): `RUN a && b`
in a Containerfile, `wctr run IMAGE sh -c '…'`, `wctr exec -it CONTAINER sh`.

A wasip1 module cannot start another program: Go's port has no fork or
exec, and WASI preview 1 has no process calls. This repository uses a
**small subset of [WASIX](https://wasix.org/)**, in WASIX's own import
module (`wasix_32v1`) and ABI — `proc_spawn3`, `proc_join`, `fd_pipe`,
`proc_id`, `proc_parent`, `tty_get`, `tty_set` — and nothing else of it
(no fork, no exec, no threads, no signals).

| | |
|---|---|
| [`wasix`](wasix) | `//go:wasmimport` bindings for the subset, with a Go API: `Spawn(name, argv, *Attr)`, `Wait(ctx, pid)`, `Pipe()`, `TtyGet`/`TtySet`. The struct encoders and decoders are portable and fuzzed, for hosts to reuse. |
| [`cmd/sh`](cmd/sh) | `sh`: the [mvdan.cc/sh](https://github.com/mvdan/sh) v3.14.1 interpreter, whose external commands are started with `proc_spawn3` and waited for with `proc_join`. |
| [`internal/run`](internal/run) | PATH lookup (`<dir>/<name>`, then `<dir>/<name>.wasm`, the pkgx bottle layout), `#!` scripts, plain scripts (POSIX ENOEXEC rule), the fd plan of a child. |

## What sh does

- Pipelines between builtins and programs (`fd_pipe`), redirections,
  here-documents, command substitution, `cd` (the child gets the
  directory both as a `chdir` fd op and as `$PWD`, which is where Go's
  wasip1 port reads it), exported variables, `$?`, `set -e/-u/-x/-f/-a`,
  `exec`, `sh -c`, `sh file`, `sh -s`, an interactive prompt on a TTY.
- Exit statuses as POSIX says: 127 not found, 126 found but cannot run,
  128+n for a signal reported by the host.
- Empty arguments and arguments with newlines reach the child intact
  (`proc_spawn3`, not the line-feed separated `proc_spawn2`).
- Children inherit only fds 0–2: the shell closes its pipe ends and
  redirection files in the child (fd ops), so readers see EOF.

Not available, by design of the subset: signals (`kill`, `trap` on
signals, `Ctrl-C` forwarding), job control (`&` runs in-process, `fg`,
`bg`), process substitution `<(…)`. `exec` runs the program and exits
with its status rather than replacing the shell.

## Build

```sh
GOOS=wasip1 GOARCH=wasm go build -o sh ./cmd/sh
```

It needs a host that implements the subset: wasmer (used in CI as the
independent oracle) or the go-wasmcontainers engine (host side in
progress). Outside such a host, starting a program fails with status 126
and says why.

```sh
wasmer run --volume ./rootfs:/ --env PATH=/bin rootfs/bin/sh -- -c 'echo a | cat; exit 3'
```

## Tests

- natively (`go test -race ./...`): the ABI encoders, the fd plan
  (fuzzed against a simulated fd table), PATH lookup, the exec handler
  with a fake host, the `sh` command line;
- under wasmer: the `wasix` package against a real WASIX host, and the
  shell end to end (`internal/e2e`).

Four defects of the oracle found on the way are listed in
[docs/REFERENCES.md](docs/REFERENCES.md), with the bibliography (WASIX
header and libc shims, wasmer's implementation, mvdan.cc/sh, Go's wasip1
port, POSIX).

## mvdan.cc/sh is patched

`third_party/mvdan-sh` is mvdan.cc/sh v3.14.1 with one change,
`interp.SetPipeFunc`: upstream creates pipes with `os.Pipe`, which is
ENOSYS on wasip1. See [third_party/mvdan-sh/PATCHED](third_party/mvdan-sh/PATCHED);
a test fails if go.mod and the copy disagree. Because of the `replace`
directive, `go install …@version` does not work for `cmd/sh`; build from a
clone.

## License

BSD-3-Clause. `third_party/mvdan-sh` keeps its own BSD-3-Clause license.
