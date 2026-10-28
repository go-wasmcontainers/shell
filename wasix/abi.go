// Package wasix binds the subset of WASIX that a shell needs to start
// and wait for other programs: proc_spawn3, proc_join, fd_pipe, proc_id,
// proc_parent, tty_get and tty_set, in the import module "wasix_32v1".
//
// The layouts and signatures come from wasix-libc
// (libc-bottom-half/headers/public/wasi/api_wasix.h and
// libc-bottom-half/sources/__wasixlibc_real.c) and are checked against
// wasmer's implementation (lib/wasix/src/syscalls/wasix/*.rs); see
// docs/REFERENCES.md.
//
// The encoding helpers in this file are portable so that they can be
// tested and fuzzed natively; the system calls themselves exist only on
// GOOS=wasip1 (elsewhere they return [ENOSYS]).
package wasix

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
)

// ImportModule is the WASIX import module for 32-bit memories.
const ImportModule = "wasix_32v1"

// Errno is a WASI error number (__wasi_errno_t, a u16).
type Errno uint16

// The WASI errno values this package produces or interprets
// (api_wasix.h, __WASI_ERRNO_*).
const (
	ESUCCESS Errno = 0
	E2BIG    Errno = 1
	EACCES   Errno = 2
	EAGAIN   Errno = 6
	EBADF    Errno = 8
	ECHILD   Errno = 12
	EINVAL   Errno = 28
	EISDIR   Errno = 31
	ELOOP    Errno = 32
	EMFILE   Errno = 33
	ENOENT   Errno = 44
	ENOEXEC  Errno = 45
	ENOMEM   Errno = 48
	ENOSYS   Errno = 52
	ENOTTY   Errno = 59
	EPERM    Errno = 63
	EPIPE    Errno = 64
	// ENOTCAPABLE is WASI's "the capability does not allow it".
	ENOTCAPABLE Errno = 76
)

var errnoNames = map[Errno]string{
	ESUCCESS: "success", E2BIG: "argument list too long",
	EACCES: "permission denied", EAGAIN: "resource temporarily unavailable",
	EBADF: "bad file descriptor", ECHILD: "no child processes",
	EINVAL: "invalid argument", EISDIR: "is a directory",
	ELOOP: "too many levels of symbolic links", EMFILE: "too many open files",
	ENOENT: "no such file or directory", ENOEXEC: "exec format error",
	ENOMEM: "cannot allocate memory", ENOSYS: "function not implemented",
	ENOTTY: "not a tty", EPERM: "operation not permitted",
	EPIPE: "broken pipe", ENOTCAPABLE: "capabilities insufficient",
}

func (e Errno) Error() string {
	if s, ok := errnoNames[e]; ok {
		return s
	}
	return fmt.Sprintf("wasi errno %d", uint16(e))
}

// Is lets errors.Is match the portable sentinels.
func (e Errno) Is(target error) bool {
	switch target {
	case fs.ErrNotExist:
		return e == ENOENT
	case fs.ErrPermission:
		return e == EACCES || e == EPERM || e == ENOTCAPABLE
	case errors.ErrUnsupported:
		return e == ENOSYS
	}
	return false
}

// errnoErr turns a raw return value into an error (nil on success).
func errnoErr(r uint32) error {
	if r == 0 {
		return nil
	}
	return Errno(uint16(r))
}

// FdOpCmd is __wasi_proc_spawn_fd_op_name_t.
type FdOpCmd uint8

// The fd operations proc_spawn2/3 apply in the child, in order.
const (
	FdOpClose  FdOpCmd = 0
	FdOpDup2   FdOpCmd = 1
	FdOpOpen   FdOpCmd = 2
	FdOpChdir  FdOpCmd = 3
	FdOpFchdir FdOpCmd = 4
)

// FdOp is one entry of the fd_ops array of proc_spawn2/3. Path is used by
// FdOpOpen and FdOpChdir only.
type FdOp struct {
	Cmd   FdOpCmd
	Fd    uint32
	SrcFd uint32
	Path  string
	// The FdOpOpen fields, as in path_open.
	DirFlags         uint32
	OFlags           uint16
	RightsBase       uint64
	RightsInheriting uint64
	FdFlags          uint16
	FdFlagsExt       uint16
}

// FdOpSize is sizeof(__wasi_proc_spawn_fd_op_t) on wasm32.
const FdOpSize = 56

// PutFdOp writes op into b (len >= FdOpSize) in the wasm32 layout of
// __wasi_proc_spawn_fd_op_t, with pathPtr the guest address of op.Path.
//
//	cmd u8 @0, fd u32 @4, src_fd u32 @8, path ptr32 @12, path_len u32 @16,
//	dirflags u32 @20, oflags u16 @24, fs_rights_base u64 @32,
//	fs_rights_inheriting u64 @40, fdflags u16 @48, fdflagsext u16 @50.
func PutFdOp(b []byte, op FdOp, pathPtr uint32) {
	_ = b[FdOpSize-1]
	clear(b[:FdOpSize])
	le := binary.LittleEndian
	b[0] = byte(op.Cmd)
	le.PutUint32(b[4:], op.Fd)
	le.PutUint32(b[8:], op.SrcFd)
	if op.Path != "" {
		le.PutUint32(b[12:], pathPtr)
		le.PutUint32(b[16:], uint32(len(op.Path)))
	}
	le.PutUint32(b[20:], op.DirFlags)
	le.PutUint16(b[24:], op.OFlags)
	le.PutUint64(b[32:], op.RightsBase)
	le.PutUint64(b[40:], op.RightsInheriting)
	le.PutUint16(b[48:], op.FdFlags)
	le.PutUint16(b[50:], op.FdFlagsExt)
}

// RawFdOp is a decoded fd op as a host sees it: the path is still a
// guest pointer and length.
type RawFdOp struct {
	FdOp
	PathPtr, PathLen uint32
}

// DecodeFdOp is the inverse of PutFdOp (Path is left empty).
func DecodeFdOp(b []byte) (RawFdOp, error) {
	if len(b) < FdOpSize {
		return RawFdOp{}, EINVAL
	}
	le := binary.LittleEndian
	var r RawFdOp
	r.Cmd = FdOpCmd(b[0])
	if r.Cmd > FdOpFchdir {
		return RawFdOp{}, EINVAL
	}
	r.Fd = le.Uint32(b[4:])
	r.SrcFd = le.Uint32(b[8:])
	r.PathPtr = le.Uint32(b[12:])
	r.PathLen = le.Uint32(b[16:])
	r.DirFlags = le.Uint32(b[20:])
	r.OFlags = le.Uint16(b[24:])
	r.RightsBase = le.Uint64(b[32:])
	r.RightsInheriting = le.Uint64(b[40:])
	r.FdFlags = le.Uint16(b[48:])
	r.FdFlagsExt = le.Uint16(b[50:])
	return r, nil
}

// JoinFlags is __wasi_join_flags_t.
type JoinFlags uint32

// proc_join flags.
const (
	JoinNonBlocking JoinFlags = 1 << 0
	JoinWakeStopped JoinFlags = 1 << 1
)

// StatusKind is __wasi_join_status_type_t.
type StatusKind uint8

// The kinds of join status.
const (
	StatusNothing  StatusKind = 0
	StatusExited   StatusKind = 1
	StatusSignaled StatusKind = 2
	StatusStopped  StatusKind = 3
)

// Status is a decoded __wasi_join_status_t.
type Status struct {
	Kind   StatusKind
	Code   uint16 // exit code (StatusExited, StatusSignaled)
	Signal uint8  // StatusSignaled, StatusStopped
}

// JoinStatusSize is sizeof(__wasi_join_status_t).
const JoinStatusSize = 6

// DecodeStatus decodes a __wasi_join_status_t: tag u8 @0, then a union
// aligned to 2 @2 — exit_normal errno u16, exit_signal {exit_code u16 @2,
// signal u8 @4}, stopped signal u8 @2.
func DecodeStatus(b []byte) (Status, error) {
	if len(b) < JoinStatusSize {
		return Status{}, EINVAL
	}
	le := binary.LittleEndian
	s := Status{Kind: StatusKind(b[0])}
	switch s.Kind {
	case StatusNothing:
	case StatusExited:
		s.Code = le.Uint16(b[2:])
	case StatusSignaled:
		s.Code = le.Uint16(b[2:])
		s.Signal = b[4]
	case StatusStopped:
		s.Signal = b[2]
	default:
		return Status{}, EINVAL
	}
	return s, nil
}

// PutStatus encodes s (the host side of DecodeStatus).
func PutStatus(b []byte, s Status) {
	_ = b[JoinStatusSize-1]
	clear(b[:JoinStatusSize])
	le := binary.LittleEndian
	b[0] = byte(s.Kind)
	switch s.Kind {
	case StatusExited:
		le.PutUint16(b[2:], s.Code)
	case StatusSignaled:
		le.PutUint16(b[2:], s.Code)
		b[4] = s.Signal
	case StatusStopped:
		b[2] = s.Signal
	}
}

// ExitCode is the shell's view of a status: the exit code, or 128+signal.
func (s Status) ExitCode() int {
	switch s.Kind {
	case StatusExited:
		return int(s.Code)
	case StatusSignaled, StatusStopped:
		return 128 + int(s.Signal)
	}
	return -1
}

// Tty is __wasi_tty_t (24 bytes, align 4).
type Tty struct {
	Cols, Rows, Width, Height uint32
	StdinTTY, StdoutTTY       bool
	StderrTTY, Echo           bool
	LineBuffered, LineFeeds   bool
}

// TtySize is sizeof(__wasi_tty_t).
const TtySize = 24

// PutTty encodes t: cols @0, rows @4, width @8, height @12, then six
// bools @16..@21.
func PutTty(b []byte, t Tty) {
	_ = b[TtySize-1]
	clear(b[:TtySize])
	le := binary.LittleEndian
	le.PutUint32(b[0:], t.Cols)
	le.PutUint32(b[4:], t.Rows)
	le.PutUint32(b[8:], t.Width)
	le.PutUint32(b[12:], t.Height)
	for i, v := range [...]bool{t.StdinTTY, t.StdoutTTY, t.StderrTTY, t.Echo, t.LineBuffered, t.LineFeeds} {
		if v {
			b[16+i] = 1
		}
	}
}

// DecodeTty is the inverse of PutTty.
func DecodeTty(b []byte) (Tty, error) {
	if len(b) < TtySize {
		return Tty{}, EINVAL
	}
	le := binary.LittleEndian
	return Tty{
		Cols: le.Uint32(b[0:]), Rows: le.Uint32(b[4:]),
		Width: le.Uint32(b[8:]), Height: le.Uint32(b[12:]),
		StdinTTY: b[16] != 0, StdoutTTY: b[17] != 0, StderrTTY: b[18] != 0,
		Echo: b[19] != 0, LineBuffered: b[20] != 0, LineFeeds: b[21] != 0,
	}, nil
}

// Limits on what Spawn marshals. The host enforces its own (stricter or
// equal) bounds; these keep the guest from building absurd requests.
const (
	MaxArgs     = 4096
	MaxEnv      = 4096
	MaxArgBytes = 1 << 20 // total argv + envp bytes, like ARG_MAX
	MaxFdOps    = 256
)

// CheckArgv validates an argv/envp pair the way Spawn will: bounded
// counts and total size, and no NUL byte inside an element (the strings
// travel NUL-terminated).
func CheckArgv(argv, env []string) error {
	if len(argv) == 0 {
		return EINVAL
	}
	if len(argv) > MaxArgs || len(env) > MaxEnv {
		return E2BIG
	}
	total := 0
	for _, list := range [][]string{argv, env} {
		for _, s := range list {
			total += len(s) + 1
			if total > MaxArgBytes {
				return E2BIG
			}
			for i := 0; i < len(s); i++ {
				if s[i] == 0 {
					return EINVAL
				}
			}
		}
	}
	return nil
}
