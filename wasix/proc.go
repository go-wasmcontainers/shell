package wasix

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Pid is a WASIX process id (__wasi_pid_t).
type Pid uint32

// Attr describes how a child starts.
type Attr struct {
	// Env is the child's environment as KEY=value strings. With
	// InheritEnv the child gets the parent's environment instead.
	Env        []string
	InheritEnv bool
	// Dir, if not empty, is the child's working directory (an FdOpChdir).
	Dir string
	// Files maps child fd i to parent fd Files[i]; a negative entry
	// closes fd i in the child. Fds past len(Files) are inherited.
	Files []int
	// Close lists further parent fds the child must not inherit.
	Close []int
	// SearchPath asks the host to look name up in Path.
	SearchPath bool
	Path       string
}

// FdOps turns the fd plan of a into the ordered list of proc_spawn fd
// operations. Sources that a previous dup2 would overwrite are first
// moved out of the way to fds above every fd involved.
func (a *Attr) FdOps() []FdOp {
	var ops []FdOp
	if a.Dir != "" {
		ops = append(ops, FdOp{Cmd: FdOpChdir, Path: a.Dir})
	}
	hi := len(a.Files)
	for _, f := range a.Files {
		hi = max(hi, f+1)
	}
	for _, f := range a.Close {
		hi = max(hi, f+1)
	}
	src := slices.Clone(a.Files)
	var temps []int
	// A source s < len(Files) that is the target of an earlier dup2
	// (index i < j where src[j] == i... i.e. s is overwritten before it
	// is read) is copied to a temporary fd first.
	for j, s := range src {
		if s >= 0 && s < j && s != j && src[s] != s {
			t := hi + len(temps)
			ops = append(ops, FdOp{Cmd: FdOpDup2, SrcFd: uint32(s), Fd: uint32(t)})
			temps = append(temps, t)
			for k := j; k < len(src); k++ {
				if src[k] == s {
					src[k] = t
				}
			}
		}
	}
	for i, s := range src {
		switch {
		case s < 0:
			ops = append(ops, FdOp{Cmd: FdOpClose, Fd: uint32(i)})
		case s != i:
			ops = append(ops, FdOp{Cmd: FdOpDup2, SrcFd: uint32(s), Fd: uint32(i)})
		}
	}
	closed := map[int]bool{}
	for _, f := range a.Close {
		if f >= len(a.Files) && !closed[f] {
			closed[f] = true
			ops = append(ops, FdOp{Cmd: FdOpClose, Fd: uint32(f)})
		}
	}
	for _, t := range temps {
		ops = append(ops, FdOp{Cmd: FdOpClose, Fd: uint32(t)})
	}
	return ops
}

// Spawn starts name as a new process in the same container (same
// filesystem), with argv (argv[0] included) and the fd plan of attr.
// Errors are [Errno] values: ENOENT when name does not exist, ENOEXEC
// when it is not a program the host can run.
func Spawn(name string, argv []string, attr *Attr) (Pid, error) {
	if attr == nil {
		attr = &Attr{InheritEnv: true}
	}
	if err := CheckArgv(argv, attr.Env); err != nil {
		return 0, err
	}
	ops := attr.FdOps()
	if len(ops) > MaxFdOps {
		return 0, E2BIG
	}
	// The pipe ends this process made are non-blocking (see Pipe). A
	// child must get them blocking: hosts keep the mode on the open file
	// description (wasmer does), which the child shares, and a program
	// that did not ask for O_NONBLOCK reads EAGAIN ("Try again"). The
	// shell keeps working on such an end afterwards, blocking only that
	// goroutine's syscall, which is safe as long as the peer is another
	// process.
	tracked.Lock()
	for _, fd := range attr.Files {
		if fd >= 0 && tracked.fds[fd] {
			setBlocking(fd)
		}
	}
	tracked.Unlock()
	return rawSpawn(name, argv, attr.Env, attr.InheritEnv, ops, attr.SearchPath, attr.Path)
}

// Join calls proc_join for pid with flags. A StatusNothing status with
// JoinNonBlocking means the child is still running.
func Join(pid Pid, flags JoinFlags) (Status, error) {
	_, s, err := rawJoin(pid, false, flags)
	return s, err
}

// JoinAny waits (or, non-blocking, polls) for any child.
func JoinAny(flags JoinFlags) (Pid, Status, error) {
	return rawJoin(0, true, flags)
}

// Wait waits for pid to exit without blocking the Go scheduler: it polls
// proc_join with JoinNonBlocking and sleeps between polls (50µs doubling
// to 5ms), so other goroutines — the ends of a pipeline that live in the
// shell itself — keep running. A blocking proc_join would freeze the
// whole wasip1 runtime, which is single-threaded.
//
// Cancelling ctx stops the wait, not the child: WASIX signals are not
// part of the supported subset.
func Wait(ctx context.Context, pid Pid) (Status, error) {
	d := 50 * time.Microsecond
	for {
		s, err := Join(pid, JoinNonBlocking)
		if err != nil {
			return Status{}, err
		}
		if s.Kind == StatusExited || s.Kind == StatusSignaled {
			return s, nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return Status{}, ctx.Err()
		case <-t.C:
		}
		d = min(d*2, 5*time.Millisecond)
	}
}

// Getpid returns the id of the calling process.
func Getpid() (Pid, error) { return rawGetpid() }

// Getppid returns the id of the parent of the calling process.
func Getppid() (Pid, error) {
	me, err := rawGetpid()
	if err != nil {
		return 0, err
	}
	return rawGetppid(me)
}

// TtyGet returns the terminal state of the calling process.
func TtyGet() (Tty, error) { return rawTtyGet() }

// TtySet updates the terminal state.
func TtySet(t Tty) error { return rawTtySet(t) }

// tracked holds the fds this process created that a child must not
// inherit (pipe ends, files opened for redirections).
var tracked struct {
	sync.Mutex
	fds map[int]bool
}

// Track records fd as one that children must not inherit.
func Track(fd int) {
	tracked.Lock()
	defer tracked.Unlock()
	if tracked.fds == nil {
		tracked.fds = map[int]bool{}
	}
	tracked.fds[fd] = true
}

// Untrack forgets fd (call it when the fd is closed).
func Untrack(fd int) {
	tracked.Lock()
	defer tracked.Unlock()
	delete(tracked.fds, fd)
}

// Inheritable returns the tracked fds that are still open, sorted: the
// ones to close in a child. Closed fds are forgotten.
func Inheritable() []int {
	tracked.Lock()
	defer tracked.Unlock()
	var out []int
	for fd := range tracked.fds {
		if fdAlive(fd) {
			out = append(out, fd)
		} else {
			delete(tracked.fds, fd)
		}
	}
	slices.Sort(out)
	return out
}

// Pipe creates a pipe with fd_pipe. Both ends are put in non-blocking
// mode, so that Go's wasip1 netpoller (poll_oneoff) schedules goroutines
// around them instead of blocking the whole runtime, and both are
// tracked so that children do not inherit them by accident.
func Pipe() (r, w *os.File, err error) {
	rfd, wfd, err := rawPipe()
	if err != nil {
		return nil, nil, &os.SyscallError{Syscall: "fd_pipe", Err: err}
	}
	for _, fd := range []int{rfd, wfd} {
		if err := setNonblock(fd); err != nil {
			closeFd(rfd)
			closeFd(wfd)
			return nil, nil, &os.SyscallError{Syscall: "fd_fdstat_set_flags", Err: err}
		}
		Track(fd)
	}
	return os.NewFile(uintptr(rfd), "|"+strconv.Itoa(rfd)), os.NewFile(uintptr(wfd), "|"+strconv.Itoa(wfd)), nil
}

func closeFd(fd int) {
	if f := os.NewFile(uintptr(fd), ""); f != nil {
		f.Close()
	}
}
