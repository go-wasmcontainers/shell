//go:build wasip1

package wasix

import (
	"runtime"
	"syscall"
	"unsafe"
)

func setNonblock(fd int) error { return syscall.SetNonblock(fd, true) }
func setBlocking(fd int) error { return syscall.SetNonblock(fd, false) }

//go:wasmimport wasi_snapshot_preview1 fd_fdstat_get
//go:noescape
func fdFdstatGet(fd uint32, buf unsafe.Pointer) uint32

// fdAlive reports whether fd is open. It asks fd_fdstat_get, not
// fd_filestat_get (syscall.Fstat): under wasmer 7.5.0, probing with the
// latter took open pipe ends for closed (observed, see
// docs/REFERENCES.md), so children kept a copy of a write end and their
// reader never saw EOF. Every fd has an fdstat.
func fdAlive(fd int) bool {
	var st [3]uint64 // __wasi_fdstat_t is 24 bytes, align 8
	return fdFdstatGet(uint32(fd), unsafe.Pointer(&st[0])) == 0
}

// The raw imports. Every parameter is an i32 in the wasm32 ABI
// (__wasixlibc_real.c); pointers are passed as unsafe.Pointer, which the
// Go compiler lowers to an i32 linear-memory address.

//go:wasmimport wasix_32v1 proc_spawn3
//go:noescape
func procSpawn3(name unsafe.Pointer, nameLen uint32,
	args unsafe.Pointer, argsLen uint32,
	envs unsafe.Pointer, envsLen uint32,
	fdOps unsafe.Pointer, fdOpsLen uint32,
	signals unsafe.Pointer, signalsLen uint32,
	searchPath uint32,
	path unsafe.Pointer, pathLen uint32,
	retPid unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_join
//go:noescape
func procJoin(pid unsafe.Pointer, flags uint32, status unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 fd_pipe
//go:noescape
func fdPipe(readFd, writeFd unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_id
//go:noescape
func procID(pid unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_parent
//go:noescape
func procParent(pid uint32, parent unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 tty_get
//go:noescape
func ttyGet(state unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 tty_set
//go:noescape
func ttySet(state unsafe.Pointer) uint32

// addr is the guest address of p (wasm32 linear memory).
func addr(p unsafe.Pointer) uint32 { return uint32(uintptr(p)) }

// cstr returns s as a NUL-terminated byte slice.
func cstr(s string) []byte {
	b := make([]byte, len(s)+1)
	copy(b, s)
	return b
}

// strArray builds the `const uint8_t **` array of proc_spawn3: one
// 32-bit pointer per NUL-terminated string. The returned backing slices
// must stay alive until the call returns.
func strArray(list []string) (ptrs []uint32, keep [][]byte) {
	ptrs = make([]uint32, len(list))
	keep = make([][]byte, len(list))
	for i, s := range list {
		keep[i] = cstr(s)
		ptrs[i] = addr(unsafe.Pointer(&keep[i][0]))
	}
	return ptrs, keep
}

func ptrOrNil[T any](s []T) unsafe.Pointer {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Pointer(&s[0])
}

func rawSpawn(name string, argv, env []string, inheritEnv bool, ops []FdOp, searchPath bool, path string) (Pid, error) {
	nameB := cstr(name)
	argPtrs, argKeep := strArray(argv)
	var envPtrs []uint32
	var envKeep [][]byte
	if !inheritEnv {
		envPtrs, envKeep = strArray(env)
		if envPtrs == nil {
			// A non-null pointer with length 0 means "empty environment";
			// null would mean "inherit".
			envPtrs = make([]uint32, 0, 1)
		}
	}
	opPaths := make([][]byte, len(ops))
	opBuf := make([]byte, len(ops)*FdOpSize+8)
	// Align the array to 8 as __wasi_proc_spawn_fd_op_t requires.
	off := int((8 - addr(unsafe.Pointer(&opBuf[0]))%8) % 8)
	opArr := opBuf[off : off+len(ops)*FdOpSize]
	for i, op := range ops {
		var p uint32
		if op.Path != "" {
			opPaths[i] = []byte(op.Path)
			p = addr(unsafe.Pointer(&opPaths[i][0]))
		}
		PutFdOp(opArr[i*FdOpSize:], op, p)
	}
	pathB := cstr(path)
	var search uint32
	if searchPath {
		search = 1
	}
	var envArg unsafe.Pointer
	if envPtrs != nil {
		envArg = unsafe.Pointer(unsafe.SliceData(envPtrs[:cap(envPtrs)]))
	}
	var opArg unsafe.Pointer
	if len(ops) > 0 {
		opArg = unsafe.Pointer(&opArr[0])
	}
	var pid uint32
	r := procSpawn3(unsafe.Pointer(&nameB[0]), uint32(len(name)),
		ptrOrNil(argPtrs), uint32(len(argPtrs)),
		envArg, uint32(len(envPtrs)),
		opArg, uint32(len(ops)),
		nil, 0,
		search,
		unsafe.Pointer(&pathB[0]), uint32(len(path)),
		unsafe.Pointer(&pid))
	runtime.KeepAlive(argKeep)
	runtime.KeepAlive(envKeep)
	runtime.KeepAlive(opPaths)
	runtime.KeepAlive(opBuf)
	if err := errnoErr(r); err != nil {
		return 0, err
	}
	return Pid(pid), nil
}

func rawJoin(pid Pid, any bool, flags JoinFlags) (Pid, Status, error) {
	// __wasi_option_pid_t: tag u8 @0, pid i32 @4.
	var opt [2]uint32
	if !any {
		opt[0] = 1
		opt[1] = uint32(pid)
	}
	var st [JoinStatusSize + 2]byte
	// The status is aligned to 2; a [N]byte inside a Go variable on wasm
	// is 8-aligned by the allocator, but take no chance.
	p := unsafe.Pointer(&st[0])
	off := addr(p) % 2
	r := procJoin(unsafe.Pointer(&opt[0]), uint32(flags), unsafe.Pointer(&st[off]))
	if err := errnoErr(r); err != nil {
		return 0, Status{}, err
	}
	s, err := DecodeStatus(st[off:])
	if err != nil {
		return 0, Status{}, err
	}
	got := Pid(opt[1])
	if opt[0]&0xff == 0 {
		got = 0
	}
	return got, s, nil
}

func rawPipe() (int, int, error) {
	var fds [2]uint32
	r := fdPipe(unsafe.Pointer(&fds[0]), unsafe.Pointer(&fds[1]))
	if err := errnoErr(r); err != nil {
		return -1, -1, err
	}
	return int(fds[0]), int(fds[1]), nil
}

func rawGetpid() (Pid, error) {
	var pid uint32
	if err := errnoErr(procID(unsafe.Pointer(&pid))); err != nil {
		return 0, err
	}
	return Pid(pid), nil
}

func rawGetppid(pid Pid) (Pid, error) {
	var parent uint32
	if err := errnoErr(procParent(uint32(pid), unsafe.Pointer(&parent))); err != nil {
		return 0, err
	}
	return Pid(parent), nil
}

func rawTtyGet() (Tty, error) {
	var b [TtySize / 4]uint32
	bs := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), TtySize)
	if err := errnoErr(ttyGet(unsafe.Pointer(&b[0]))); err != nil {
		return Tty{}, err
	}
	return DecodeTty(bs)
}

func rawTtySet(t Tty) error {
	var b [TtySize / 4]uint32
	bs := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), TtySize)
	PutTty(bs, t)
	return errnoErr(ttySet(unsafe.Pointer(&b[0])))
}
