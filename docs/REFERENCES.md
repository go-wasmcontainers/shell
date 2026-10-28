# References

Everything listed here was opened while designing and writing this
repository; line numbers refer to the revisions given.

## WASIX: the ABI the guest calls

- **wasix-libc**, `libc-bottom-half/headers/public/wasi/api_wasix.h`, main at
  `2025b44a5d2eb22b64ecdbf1bb16f3e7699dfb60`
  (<https://github.com/wasix-org/wasix-libc>):
  - `__wasi_proc_spawn_fd_op_t` (l. 3659–3732): names CLOSE 0, DUP2 1,
    OPEN 2, CHDIR 3, FCHDIR 4; size 56, align 8; offsets cmd 0, fd 4,
    src_fd 8, path 12, path_len 16, dirflags 20, oflags 24,
    fs_rights_base 32, fs_rights_inheriting 40, fdflags 48, fdflagsext 50.
  - `__wasi_proc_spawn2` (l. 4392) — args and envs as line-feed separated
    strings; `__wasi_proc_spawn3` (l. 4441) — "proper pointer arrays of
    null-terminated strings", a null `envs` inherits.
  - `__wasi_proc_join` (l. 4515), `__wasi_join_flags_t` (l. 3290:
    NON_BLOCKING 1, WAKE_STOPPED 2), `__wasi_join_status_t` (l. 3305–3366:
    tag u8, union aligned to 2; size 6), `__wasi_option_pid_t`
    (l. 1597–1610: tag u8 @0, pid i32 @4; size 8).
  - `__wasi_proc_id`, `__wasi_proc_parent` (l. 4493–4505), `__wasi_pid_t`
    is a 32-bit int (l. 1584).
  - `__wasi_fd_pipe` (l. 3898), `__wasi_tty_t` (l. 1664–1728: four u32,
    then six bools at 16–21; size 24) with `tty_get`/`tty_set`
    (l. 3905–3913).
  - errno values (l. 73–468), e.g. NOENT 44, NOEXEC 45, NOSYS 52,
    UNKNOWN 79.
- **wasix-libc**, `libc-bottom-half/sources/__wasixlibc_real.c`, same
  revision: the import module is **`wasix_32v1`**, every argument is an
  i32; `proc_spawn2`/`proc_spawn3` take 14 (name, name_len, args,
  args_len, envs, envs_len, fd_ops, fd_ops_len, signals, signals_len,
  search_path, path, path_len, ret_pid) (l. 488–536); `proc_join`
  (pid_ptr, flags, status_ptr) (l. 563–575); `fd_pipe` (l. 71–80).

### Why `proc_spawn3` and not `proc_spawn2`

wasmer's `parse_delimited_string_list` (`lib/wasix/src/syscalls/mod.rs`,
l. 899–904) splits `proc_spawn2`'s strings on `\n` and `\r` **and drops
empty entries**, and wasmer documents `proc_spawn2` as "Legacy
delimiter-based API … Prefer `proc_spawn3`". A shell must pass `""` and
arguments containing newlines (`test -z ""`), so the guest calls
`proc_spawn3`, which is part of the same module and the same subset. The
host side should implement both (C programs built with wasix-libc call
`proc_spawn2` through `posix_spawn`).

## wasmer, the independent host used as the oracle

wasmer v7.5.0 (release 2026-10-01; tarballs checksummed in CI) and main at
`8c605a073c58f2b02766860225b00549e30e5442`, `lib/wasix/src/`:

- `syscalls/wasix/proc_spawn2.rs`, `proc_spawn3.rs`: spawn = fork of the
  WASI env (fd table copied), then the fd ops in order. `apply_fd_op`:
  CLOSE skips pre-opened directories, DUP2 via `dup2_at`, OPEN relative to
  the cwd, CHDIR, FCHDIR. With `search_path`, PATH defaults to
  `/usr/local/bin:/bin:/usr/bin`; nothing found is ENOENT, a file that
  will not run is ENOEXEC.
- `syscalls/wasix/proc_join.rs`: none = any child; NON_BLOCKING returns
  status NOTHING while running; the pid is written back.
- `syscalls/wasix/fd_pipe.rs`: first fd is the read end, second the write
  end; both with empty fd flags.
- `fs/mod.rs`, `dup2_at` (l. 2155–2211): refuses a pre-opened target,
  copies the fd flags and clears CLOEXEC.
- `virtual-fs/src/pipe.rs`: an unbounded channel (writes never block).

Defects of that oracle found while testing (the guest works around them,
the engine's host must not have them):

1. `WasiFs::fdstat` (`fs/mod.rs` l. 1841–1866) answers fds 0–2 with
   hard-coded flags, so after a dup2 of a pipe onto fd 0 a Go child cannot
   see the NONBLOCK that Go's `syscall` init set (Go 1.27.3
   `syscall/fs_wasip1.go` l. 16–28), treats stdin as blocking and surfaces
   EAGAIN ("Try again").
2. The exit code travels in a `__wasi_errno_t` and wasmer converts it
   through its `Errno` enum: codes above 80 come back as 79 (UNKNOWN).
3. Observed, not traced to a line: with `fd_filestat_get`
   (`syscall.Fstat`) as the liveness probe of tracked fds, open pipe ends
   were taken for closed, children kept a write end and pipelines of more
   than ~1000 lines hung; probing with `fd_fdstat_get` fixed it.
4. A goroutine parked on Go's poller (`poll_oneoff`) for an empty pipe end
   never wakes, not even for the clock subscription: the whole module
   freezes (`wasix.TestPipeBetweenGoroutines` with
   `WASIX_HOST_POLLS_PIPES=1`). The tests that need it are gated.

## mvdan.cc/sh v3.14.1 (the interpreter)

- `interp/handler.go`: `ExecHandlerFunc` (l. 113–123: called for commands
  that are neither functions nor builtins; nil = status 0;
  `NewExitStatus`/`ExitStatus` for others), `DefaultExecHandler`
  (l. 141–213: 127 when not found, 126 when it cannot start, 128+signal),
  `LookPathDir`, `OpenHandlerFunc` (l. 377–392), `DefaultOpenHandler`
  (l. 400–414).
- `interp/stdin_os.go`: pipes come from `os.Pipe` (patched, see
  `third_party/mvdan-sh/PATCHED`); `interp/runner.go` l. 500–531: a
  pipeline runs its left side in a goroutine.
- `interp/vars.go` `execEnv` (l. 132–152): exported string variables only.

## Go's wasip1 port (go1.27.3)

- `os/pipe_wasm.go`: `os.Pipe` is ENOSYS on wasip1 (and js).
- `syscall/fs_wasip1.go` l. 16–28: stdio is put in non-blocking mode at
  init so that the runtime can use the poller.
- `os/file_unix.go` l. 144–227: files opened with `os.OpenFile` are made
  non-blocking and pollable; `os.NewFile` respects the current flags.
- `internal/syscall/unix/fcntl_wasip1.go`: F_GETFL is `fd_fdstat_get`.
- `cmd/compile/internal/ssagen/abi.go` `paramsToWasmFields`: what a
  `//go:wasmimport` may take — 32/64-bit ints, floats, `unsafe.Pointer`,
  `uintptr`, bool, string (ptr, len), pointers to allowed types; at most
  one result.
- `runtime/netpoll_wasip1.go`: the poller is `poll_oneoff`, level
  triggered, with a clock subscription for timeouts.
- `lib/wasm/go_wasip1_wasm_exec`: `GOWASIRUNTIME=wasmer` runs tests with
  `wasmer run --dir=/`.
- There is no fork or exec in the port: starting a program needs the
  host (hence WASIX).

## OCI: the base image

- OCI image-spec `image-layout.md`: `blobs/<alg>/<encoded>` must match
  the digest, the `oci-layout` file carries `imageLayoutVersion`, and
  `index.json` is the entry point. `layer.md`: the layer is a tar
  changeset whose entries record `mode`, `mtime` and, for symbolic
  links, `linkname`; whiteouts are `.wh.` files (none here).
- OCI distribution-spec `spec.md`: HEAD a blob (end-2, 200 if present,
  404 if not), open an upload with POST `/v2/<name>/blobs/uploads/`
  (end-4a, 202 + `Location`), finish it with PUT
  `<location>?digest=<digest>` (end-6, 201), and PUT
  `/v2/<name>/manifests/<tag>` with the manifest's media type as
  `Content-Type` (end-7a, 201).
- distribution's token authentication
  (<https://distribution.github.io/distribution/spec/auth/token/>):
  a 401 carries `Bearer realm=…,service=…,scope=…`; the client GETs the
  realm with `service` and `scope`, reads `token` (or `access_token`)
  and retries with `Authorization: Bearer`.
- go-wasmcontainers/engine v0.12.0, `image` (types, `ParseChallenge`,
  layout and registry pulls) and `rootfs` (layer unpacking): the tests
  pull the built layout and unpack the layer with them.

## POSIX shell

- The Open Group Base Specifications Issue 8, XCU chapter 2
  (<https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html>):
  2.8.2 (127 not found, 126 found but not executable, > 128 signal),
  2.9.1.4 (command search through PATH, 127 and an error message when not
  found), 2.9.1.6 (ENOEXEC: run the file as a script with a new shell,
  `$0` may be the command name; otherwise 126).
- XCU `cat` (<https://pubs.opengroup.org/onlinepubs/9799919799/utilities/cat.html>):
  "If no file operands are specified, the standard input shall be used",
  and `-` reads standard input at that point — the rule
  wasmdesk/coreutils follows (from #15) for cat, head, tail, wc, grep and
  paste.
