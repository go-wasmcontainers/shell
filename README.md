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
| [`wasix`](wasix) | `//go:wasmimport` bindings for the subset, with a Go API: `Spawn(name, argv, *Attr)`, `Wait(ctx, pid)`, `Pipe()`, `TtyGet`/`TtySet`. The struct encoders and decoders are portable and fuzzed, for hosts to reuse. A **module of its own**, `github.com/go-wasmcontainers/shell/wasix`, with no dependencies, so programs can import it without the shell. |
| [`wasix/exec`](wasix/exec) | `os/exec` for WASI containers: `Command`, `Cmd` (`Path Args Env Dir Stdin Stdout Stderr`), `Start`/`Wait`/`WaitContext`/`Run`/`Output`/`CombinedOutput`, `StdinPipe`/`StdoutPipe`/`StderrPipe`, `ExitError`, `LookPath` (`<dir>/<name>` then `<dir>/<name>.wasm`). A pluggable `System` (WASIX by default) for tests and tracing. No `Kill`/`CommandContext`: signals are not in the WASIX subset. `sh` itself starts its children through it. |
| [`cmd/sh`](cmd/sh) | `sh`: the [mvdan.cc/sh](https://github.com/mvdan/sh) v3.14.1 interpreter, whose external commands are started with `proc_spawn3` and waited for with `proc_join`. |
| [`internal/run`](internal/run) | the shell's exec handler: PATH lookup (`wasix/exec`'s rule, the pkgx bottle layout), `#!` scripts, plain scripts (POSIX ENOEXEC rule), children started through `wasix/exec`. |
| [`cmd/mkimage`](cmd/mkimage) | builds the base image (below) as an OCI image layout, and pushes it to a registry. |

## The base image

`ghcr.io/go-wasmcontainers/shell`, platform `wasip1/wasm`, one layer:

| path | |
|---|---|
| `/bin/sh` | this shell |
| `/bin/coreutils.wasm` | [wasmdesk/coreutils](https://github.com/wasmdesk/coreutils), the multicall module |
| `/bin/cat`, `/bin/ls`, … | one symbolic link to `coreutils.wasm` per tool (42 tools); the module picks the tool from `argv[0]` |
| `/etc/passwd`, `/etc/group`, `/etc/os-release` | root only |
| `/tmp` (1777), `/root` | |

Config: `Cmd ["/bin/sh"]`, `Env PATH=/bin`, `WorkingDir /`, and
`Shell ["/bin/sh","-c"]`, which is what a shell-form `RUN`, `CMD` or
`ENTRYPOINT` in a Containerfile runs. The build is reproducible (every
mtime is the commit time). It is published by the `image` workflow when a
release tag is pushed (tags `X.Y.Z` and `latest`); every CI run keeps the
layout as an artifact.

```sh
go run ./cmd/mkimage --sh sh.wasm --coreutils coreutils.wasm --version 0.1.0 --layout ./layout
wctr pull oci:./layout:0.1.0
```

It runs on a host that implements the WASIX process subset: the
go-wasmcontainers engine from v0.17.0 (`wctrd --wasi engine`, the
default), where pipelines through the coreutils work.

## What sh does

- Pipelines between builtins and programs (`fd_pipe`), redirections,
  here-documents, command substitution, `cd` (the child gets the
  directory both as a `chdir` fd op and as `$PWD`, which is where Go's
  wasip1 port reads it), exported variables, `$?`, `set -e/-u/-x/-f/-a`,
  `exec`, `sh -c`, `sh file`, `sh -s`, an interactive prompt on a TTY.
- A line editor on a terminal (`wctr exec -it c sh`): the arrow keys,
  Home/End, Backspace/Delete, history (Up/Down, ^P/^N), ^A ^E ^U ^K ^W ^L,
  ^C to abandon a line, ^D on an empty line to end the shell with the
  last command's status. The terminal is in raw mode (WASIX `tty_set`)
  only while a line is typed; commands run with it in line mode. Unknown
  keys and escape sequences are ignored, never inserted. No Tab
  completion yet; one column per character is assumed, and a line wider
  than the screen scrolls sideways.
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

It needs a host that implements the subset: the go-wasmcontainers engine
(v0.17.0 and later), or wasmer (used in CI as the independent oracle).
Outside such a host, starting a program fails with status 126 and says
why. A redirection to `/dev/null` works in a container without `/dev`.

```sh
wasmer run --volume ./rootfs:/ --env PATH=/bin rootfs/bin/sh -- -c 'echo a | cat; exit 3'
```

## Tests

- natively (`go test -race ./...`): the ABI encoders, the fd plan
  (fuzzed against a simulated fd table), PATH lookup, the exec handler
  with a fake host, the `sh` command line;
- under wasmer: the `wasix` package against a real WASIX host, and the
  shell end to end (`internal/e2e`);
- in the engine itself (`TestBaseImageUnderEngine`): the base image's
  layer, sh and the real coreutils, pipelines included (a 20000-line one),
  which wasmer cannot run; and `wctr exec c sh -c 'ls /bin | wc -l'` into
  a running container of the image (`TestExecIntoBaseImage`, engine
  v0.20.0 and later).

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
