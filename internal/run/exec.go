package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/go-wasmcontainers/shell/wasix"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// Spawner starts and waits for processes; [WASIX] in the real shell.
type Spawner interface {
	Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error)
	Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error)
	// Pipe returns a pipe whose ends a child can inherit.
	Pipe() (r, w *os.File, err error)
	// Inheritable lists the fds a child must not inherit.
	Inheritable() []int
}

// WASIX is the [Spawner] backed by the wasix package.
type WASIX struct{}

func (WASIX) Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error) {
	return wasix.Spawn(name, argv, attr)
}
func (WASIX) Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error) {
	return wasix.Wait(ctx, pid)
}
func (WASIX) Pipe() (*os.File, *os.File, error) { return wasix.Pipe() }
func (WASIX) Inheritable() []int                { return wasix.Inheritable() }

// Runner holds what the exec handler needs.
type Runner struct {
	FS      FS
	Spawner Spawner
	// Script runs a shell script in-process (for KindScript files);
	// args[0] is the script path.
	Script func(ctx context.Context, hc interp.HandlerContext, path string, args []string) error
	// Trace, if set, receives one line per spawn (SH_TRACE_SPAWN=1).
	Trace io.Writer
}

// Env is the environment a child gets: the exported string variables,
// last assignment wins, in the order they were first seen.
func Env(env expand.Environ) []string {
	var order []string
	vals := map[string]string{}
	for name, vr := range env.Each {
		if !vr.IsSet() {
			delete(vals, name)
			continue
		}
		if vr.Exported && vr.Kind == expand.String {
			if _, ok := vals[name]; !ok {
				order = append(order, name)
			}
			vals[name] = vr.String()
		}
	}
	out := make([]string, 0, len(order))
	for _, n := range order {
		if v, ok := vals[n]; ok {
			out = append(out, n+"="+v)
		}
	}
	return out
}

// withPWD sets PWD=dir: Go's wasip1 port takes its working directory
// from $PWD, not from the host, so this is what makes `cd` reach a Go
// child. Non-Go children get the FdOpChdir as well.
func withPWD(env []string, dir string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PWD=") {
			out = append(out, kv)
		}
	}
	return append(out, "PWD="+dir)
}

// fileFd returns the descriptor of f without switching it to blocking
// mode (os.File.Fd would).
func fileFd(f *os.File) (int, bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return -1, false
	}
	fd := -1
	if rc.Control(func(u uintptr) { fd = int(u) }) != nil {
		return -1, false
	}
	return fd, fd >= 0
}

// ExecHandler returns the interp exec handler: it resolves args[0],
// starts it with proc_spawn3 and waits for it with proc_join.
func (r *Runner) ExecHandler() interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		p, err := LookPath(r.FS, hc.Dir, hc.Env.Get("PATH").String(), args[0])
		if err != nil {
			fmt.Fprintf(hc.Stderr, "sh: %s: not found\n", args[0])
			return interp.ExitStatus(127)
		}
		kind, interpName, interpArg, err := Classify(r.FS, p)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "sh: %s: %v\n", args[0], err)
			return interp.ExitStatus(126)
		}
		name, argv := p, args
		switch kind {
		case KindBinary:
			fmt.Fprintf(hc.Stderr, "sh: %s: cannot execute binary file\n", args[0])
			return interp.ExitStatus(126)
		case KindScript:
			if r.Script == nil {
				fmt.Fprintf(hc.Stderr, "sh: %s: exec format error\n", args[0])
				return interp.ExitStatus(126)
			}
			return r.Script(ctx, hc, p, args)
		case KindShebang:
			ip, err := LookPath(r.FS, hc.Dir, "", interpName)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "sh: %s: %s: bad interpreter\n", args[0], interpName)
				return interp.ExitStatus(126)
			}
			argv = []string{interpName}
			if interpArg != "" {
				argv = append(argv, interpArg)
			}
			argv = append(argv, p)
			argv = append(argv, args[1:]...)
			name = ip
		}
		return r.spawnWait(ctx, hc, name, argv)
	}
}

func (r *Runner) spawnWait(ctx context.Context, hc interp.HandlerContext, name string, argv []string) error {
	var (
		files    = make([]int, 3)
		closeLtr []io.Closer // parent copies of pipe ends handed to the child
		copies   sync.WaitGroup
		drains   []*os.File
	)
	defer func() {
		for _, c := range closeLtr {
			c.Close()
		}
	}()
	streams := []any{hc.Stdin, hc.Stdout, hc.Stderr}
	for i, s := range streams {
		if f, ok := s.(*os.File); ok && f != nil {
			if fd, ok := fileFd(f); ok {
				files[i] = fd
				continue
			}
		}
		if s == nil || isNilReader(s) {
			files[i] = -1
			continue
		}
		pr, pw, err := r.Spawner.Pipe()
		if err != nil {
			return spawnError(hc, argv[0], err)
		}
		if i == 0 {
			fd, _ := fileFd(pr)
			files[i] = fd
			closeLtr = append(closeLtr, pr)
			src := s.(io.Reader)
			go func() {
				io.Copy(pw, src)
				pw.Close()
			}()
		} else {
			fd, _ := fileFd(pw)
			files[i] = fd
			closeLtr = append(closeLtr, pw)
			dst := lockedWriter{s.(io.Writer)}
			drains = append(drains, pr)
			copies.Go(func() {
				io.Copy(dst, pr)
			})
		}
	}
	attr := &wasix.Attr{
		Env:   withPWD(Env(hc.Env), hc.Dir),
		Dir:   hc.Dir,
		Files: files,
		Close: r.Spawner.Inheritable(),
	}
	if r.Trace != nil {
		fmt.Fprintf(r.Trace, "sh: spawn %s %q files=%v close=%v ops=%+v\n", name, argv, attr.Files, attr.Close, attr.FdOps())
	}
	pid, err := r.Spawner.Spawn(name, argv, attr)
	if r.Trace != nil {
		fmt.Fprintf(r.Trace, "sh: spawned pid=%d err=%v\n", pid, err)
	}
	// The child holds its own copies now; the parent's must go so that
	// readers see EOF when the child exits.
	for _, c := range closeLtr {
		c.Close()
	}
	closeLtr = nil
	if err != nil {
		for _, f := range drains {
			f.Close()
		}
		return spawnError(hc, argv[0], err)
	}
	st, err := r.Spawner.Wait(ctx, pid)
	copies.Wait()
	for _, f := range drains {
		f.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(hc.Stderr, "sh: %s: wait: %v\n", argv[0], err)
		return interp.ExitStatus(126)
	}
	if c := st.ExitCode(); c != 0 {
		return interp.ExitStatus(uint8(c))
	}
	return nil
}

// drainMu serialises the copies into writers that are not files (a
// command substitution's buffer, say): the stdout and stderr of one
// child, or two members of a pipeline, may share one.
var drainMu sync.Mutex

type lockedWriter struct{ w io.Writer }

func (l lockedWriter) Write(p []byte) (int, error) {
	drainMu.Lock()
	defer drainMu.Unlock()
	return l.w.Write(p)
}

func isNilReader(s any) bool {
	f, ok := s.(*os.File)
	return ok && f == nil
}

func spawnError(hc interp.HandlerContext, name string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(hc.Stderr, "sh: %s: not found\n", name)
		return interp.ExitStatus(127)
	case errors.Is(err, errors.ErrUnsupported):
		fmt.Fprintf(hc.Stderr, "sh: %s: cannot start programs: the host does not implement the WASIX process calls (%v)\n", name, err)
		return interp.ExitStatus(126)
	case errors.Is(err, wasix.E2BIG):
		fmt.Fprintf(hc.Stderr, "sh: %s: argument list too long\n", name)
		return interp.ExitStatus(126)
	case errors.Is(err, wasix.EAGAIN), errors.Is(err, wasix.ENOMEM):
		fmt.Fprintf(hc.Stderr, "sh: %s: cannot start: process limit reached (%v)\n", name, err)
		return interp.ExitStatus(126)
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		fmt.Fprintf(hc.Stderr, "sh: %s: %v\n", name, en)
		return interp.ExitStatus(126)
	}
	fmt.Fprintf(hc.Stderr, "sh: %s: %v\n", name, err)
	return interp.ExitStatus(126)
}
