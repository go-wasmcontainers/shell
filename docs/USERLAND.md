# The container userland: inventory

What the `shell` image's userland has today, what a container or cloud
lab still needs, and which pure-Go programs could fill each gap.
Measured on 2026-10-29. Every "builds" was checked with `GOOS=wasip1
GOARCH=wasm go build`, one package at a time, by
`go run ./internal/tools/wasip1check -C DIR PKG...`; a failure gives the
first compiler error. Building is not running: the runtime caveats below
say what a build cannot show.

Sources opened: the wasmdesk/coreutils v0.4.0 README and sources; the
READMEs and licences of u-root/u-root (BSD-3-Clause, last commit
2026-10-27), benhoyt/goawk (MIT, 2026-10-28), itchyny/gojq (MIT,
2026-10-29), rwtodd/Go.Sed (MIT, 2025-03-25), walles/moor,
zyedidia/micro and ameshkov/dnslookup, cloned at their default branch.

## 1. What wasmdesk/coreutils v0.4.0 provides (42 tools)

One multicall module; `/bin/<tool>` links to it.

| Tool | Has | Missing, by common lab use |
|---|---|---|
| `cat` | `-n`, stdin, `-` | `-A`, `-s` |
| `head`, `tail` | `-n N`, stdin | `-c`, `-n +N`, `tail -f` |
| `wc` | `-l -w -c`, stdin | `-m` |
| `grep` | `-i -v -n -E` (Onigmo subset), stdin | `-r`, `-l`, `-c`, `-o`, `-w`, `-q`, `-F`, BRE; default is a literal substring |
| `ls` | `-l`, `-a` (no-op) | `-R`, `-h`, `-t`, `-1`; marks directories with `/` |
| `find` | `-name GLOB` | `-type`, `-maxdepth`, `-exec`, `-print0`, `-size`, `-mtime` |
| `cp`, `rm` | `-r`; `rm -f` | `cp -p`, `-a`, `-i` |
| `mv`, `mkdir`, `rmdir`, `touch`, `pwd`, `basename`, `dirname` | basics, `mkdir -p` | `touch` cannot set times |
| `sort` | `-r -n -u` | `-k`, `-t`, `-h`, `-V` |
| `uniq` | `-c -d -u` | `-i`, `-f` |
| `cut` | `-f -d -c -b` (`-b` acts as `-c`) | `--complement` |
| `tr` | sets, ranges, `-d -s` | `[:class:]`, `-c` |
| `printf` | `%s %d %x %o %c %%`, `\n \t \\` | widths, `%f`, `%b` |
| `echo` | arguments | `-n`, `-e` (sh's own echo has them) |
| `date` | `-d RFC3339` | `+FORMAT`, `-u` |
| `seq`, `sleep`, `yes`, `true`, `false` | integer `seq`; `yes -n` | `seq -w`, float steps; `yes` stops after 1 line by default |
| `env` | prints the environment | `env K=V CMD` (needs spawning) |
| `expr` | `A OP B`, `:` | parentheses |
| `nl`, `tac`, `rev`, `fold`, `expand`, `unexpand`, `paste` | basics | `paste -s` |
| `md5sum`, `sha1sum`, `sha256sum`, `base64`, `base32` | hashes and `-d` | `-c` (check) |

## 2. Missing tools, candidates, wasip1 builds

| Need | Candidate (licence) | wasip1 build | Runtime caveat |
|---|---|---|---|
| sed | rwtodd/Go.Sed `cmd/sed-go` (MIT) | PASS 3.6 MB | Go RE2 syntax, not POSIX BRE (`\(..\)`, `\{n\}`); no `-i` |
| awk | benhoyt/goawk (MIT, POSIX, tested against onetrue-awk and gawk suites) | PASS 5.7 MB | `system()` and pipes use `os/exec`: ENOSYS on wasip1 |
| xargs | u-root `core/xargs` (BSD-3) | PASS 3.2 MB | spawns through `os/exec`: needs a WASIX port |
| find (full) | u-root `core/find` | FAIL `pkg/ls`: `s.Rdev undefined` | extend coreutils `find` instead |
| tar, gzip | u-root `core/tar`, `core/gzip` | PASS 3.5 / 3.4 MB | — |
| jq | itchyny/gojq `cmd/gojq` (MIT) | PASS 8.9 MB | — |
| tee, which, readlink, realpath, mktemp, cmp, comm, strings, hexdump, truncate, dd, tac | u-root | PASS 2.8–3.0 MB each | — |
| ln | u-root `core/ln` | PASS | symlinks: yes; hard links: as far as the engine's rootfs allows |
| chmod | u-root `core/chmod` | PASS 3.4 MB | WASI preview 1 cannot change modes or owners: it builds, but cannot work. Keep a no-op that says so |
| id, whoami | u-root `core/id` | PASS | there are no users: write a 3-line `id` (`uid=0(root)`) |
| hostname | u-root `core/hostname` | FAIL: no Go files for wasip1 | print `$HOSTNAME`: write it |
| uname | u-root `core/uname` | FAIL `unix.Utsname` | write it: static `WASI wasip1 wasm` |
| date with formats | u-root `core/date` | FAIL `syscall.Settimeofday` | extend coreutils `date +FMT` |
| df, du | u-root `core/df`, `core/du` | FAIL `Statfs_t`, `st.Blocks` | write `du` from file sizes; `df` needs the engine's quota |
| diff | none found as a pure-Go CLI | — | write one on a diff library |
| more | u-root `core/more` | PASS 2.8 MB | — |
| less | walles/moor | FAIL `adrg/xdg`: `ExpandHome` undefined | none; `more` instead |
| vi, editor | zyedidia/micro | FAIL tcell: `*tScreen` misses `Beep` | u-root `exp/ed` PASS 3.9 MB (line editor); a full-screen editor needs a terminal layer |
| wget, curl | u-root `core/wget` | PASS 10.7 MB | Go's `net` cannot dial on wasip1: needs a dialer over the engine's WasmEdge sockets |
| nc | u-root `core/netcat` | PASS 9.8 MB | same as wget |
| dig, nslookup | ameshkov/dnslookup | FAIL AdGuard `golibs`: `rootDirFS` undefined | a small resolver CLI on miekg/dns, with the dialer |
| ping | u-root `core/ping` | PASS 4.1 MB | raw ICMP sockets do not exist in WASI: needs the planned ICMP extension |
| ps, top, kill | u-root `core/ps` FAIL (no wasip1 files); `core/kill` FAIL `signums` | — | need engine support (a process host module or `/proc`); WASIX here has no signals |
| timeout, watch, time | u-root | timeout FAIL `sigmap`; watch, time PASS | `watch` and `time` spawn: WASIX port |
| clear | — | — | one escape sequence: write it |
| pkgx | — | — | out of scope here (a separate client) |

## 3. Order and placement (proposed)

1. **Into wasmdesk/coreutils** (small, shared with wasmbox): `hostname`,
   `uname`, `id`/`whoami`, `clear`, `tee`, `which`; then the missing
   flags of `grep`, `find`, `sort`, `date +FMT`, `head`/`tail -c`;
   `du`; a no-op `chmod` that explains why it cannot work.
2. **sed and awk** (largest lab value): goawk as is; for sed, sed-go plus
   a BRE→RE2 translation, or a sed written for POSIX BRE.
3. **xargs, `env CMD`, `find -exec`, awk `system()`**: one `os/exec`-like
   package over the shell's `wasix` (Spawn/Wait/Pipe), then small ports.
4. **tar, gzip, jq, diff, more, ed**.
5. **Network** (wget/curl, nc, dig): a wasip1 dialer over the engine's
   sockets first.
6. **ps/top/kill, df**: engine work (a process host module, the quota).

**Images**: keep `shell` minimal (sh + coreutils, about 11 MB of
modules). Add a second image, `shell-full`, layered on it, carrying a
second multicall module built in this repository from the third-party
packages (goawk, gojq, sed, u-root subset). That keeps their licences,
binary sizes (3–11 MB each) and update cadence out of the base image.
