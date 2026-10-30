package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"

	"github.com/go-wasmcontainers/shell/wasix"
	"github.com/go-wasmcontainers/shell/wasix/exec"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// Spawner starts and waits for processes: wasix/exec's System.
type Spawner = exec.System

// WASIX is the [Spawner] backed by the wasix package.
type WASIX = exec.WASIX

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

// spawnWait runs one program through wasix/exec: the shell's streams that
// are files go to the child as they are, the others through pipes.
func (r *Runner) spawnWait(ctx context.Context, hc interp.HandlerContext, name string, argv []string) error {
	sys := r.Spawner
	if r.Trace != nil {
		sys = tracer{sys, r.Trace}
	}
	c := &exec.Cmd{
		Path: name, Args: argv,
		// exec adds PWD=Dir last: Go's wasip1 port takes its working
		// directory from $PWD, so this is what makes `cd` reach a Go
		// child; other children get the FdOpChdir as well.
		Env: Env(hc.Env), Dir: hc.Dir,
		Stdin:  reader(hc.Stdin),
		Stdout: writer(hc.Stdout), Stderr: writer(hc.Stderr),
		System: sys,
	}
	if err := c.Start(); err != nil {
		return spawnError(hc, argv[0], err)
	}
	err := c.WaitContext(ctx)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		return interp.ExitStatus(uint8(ee.ExitCode()))
	case c.ProcessState != nil && c.ProcessState.Success():
		return nil // the child succeeded; a copy to a closed reader is not its failure
	case ctx.Err() != nil:
		return ctx.Err()
	}
	fmt.Fprintf(hc.Stderr, "sh: %s: wait: %v\n", argv[0], errors.Unwrap(err))
	return interp.ExitStatus(126)
}

// reader turns the interpreter's stdin into exec's: a nil file is nil.
func reader(s io.Reader) io.Reader {
	if f, ok := s.(*os.File); ok && f == nil {
		return nil
	}
	return s
}

// writer turns an output into exec's: files as they are, nil as nil, and
// other writers locked, since two members of a pipeline may share one (a
// command substitution's buffer).
func writer(w io.Writer) io.Writer {
	switch v := w.(type) {
	case nil:
		return nil
	case *os.File:
		if v == nil {
			return nil
		}
		return v
	}
	return lockedWriter{w}
}

// drainMu serialises the copies into writers that are not files.
var drainMu sync.Mutex

type lockedWriter struct{ w io.Writer }

func (l lockedWriter) Write(p []byte) (int, error) {
	drainMu.Lock()
	defer drainMu.Unlock()
	return l.w.Write(p)
}

// tracer writes a line per spawn (SH_TRACE_SPAWN=1).
type tracer struct {
	Spawner
	w io.Writer
}

func (t tracer) Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error) {
	fmt.Fprintf(t.w, "sh: spawn %s %q files=%v close=%v ops=%+v\n", name, argv, attr.Files, attr.Close, attr.FdOps())
	pid, err := t.Spawner.Spawn(name, argv, attr)
	fmt.Fprintf(t.w, "sh: spawned pid=%d err=%v\n", pid, err)
	return pid, err
}

func spawnError(hc interp.HandlerContext, name string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(hc.Stderr, "sh: %s: not found\n", name)
		return interp.ExitStatus(127)
	case errors.Is(err, errors.ErrUnsupported):
		fmt.Fprintf(hc.Stderr, "sh: %s: cannot start programs: the host does not implement the WASIX process calls (%v)\n", name, errors.Unwrap(err))
		return interp.ExitStatus(126)
	case errors.Is(err, wasix.E2BIG):
		fmt.Fprintf(hc.Stderr, "sh: %s: argument list too long\n", name)
		return interp.ExitStatus(126)
	case errors.Is(err, wasix.EAGAIN), errors.Is(err, wasix.ENOMEM):
		fmt.Fprintf(hc.Stderr, "sh: %s: cannot start: process limit reached (%v)\n", name, errors.Unwrap(err))
		return interp.ExitStatus(126)
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		fmt.Fprintf(hc.Stderr, "sh: %s: %v\n", name, en)
		return interp.ExitStatus(126)
	}
	if u := errors.Unwrap(err); u != nil {
		err = u
	}
	fmt.Fprintf(hc.Stderr, "sh: %s: %v\n", name, err)
	return interp.ExitStatus(126)
}
