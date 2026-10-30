// Package exec runs programs in a WASIX container the way os/exec runs
// them on an operating system: a Cmd, Start, Wait, Run, Output, pipes.
// Underneath it is the wasix package (proc_spawn3, proc_join, fd_pipe):
// the child is another WebAssembly module in the same container, found on
// PATH as <dir>/<name> or <dir>/<name>.wasm.
//
// Differences from os/exec: there is no CommandContext and no Process.Kill
// (signals are not in the WASIX subset the hosts implement); a nil Stdin,
// Stdout or Stderr is a pipe that is closed or drained, since a container
// need not have /dev/null; ExtraFiles and SysProcAttr do not exist.
//
// Off GOOS=wasip1, Start fails with an error that matches
// errors.ErrUnsupported.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/go-wasmcontainers/shell/wasix"
)

// System is what a Cmd needs from the host: WASIX itself (the default),
// or a fake in tests, or a wrapper that traces.
type System interface {
	Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error)
	Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error)
	// Pipe returns a pipe whose ends a child can inherit.
	Pipe() (r, w *os.File, err error)
	// Inheritable lists the fds a child must not inherit.
	Inheritable() []int
}

// WASIX is the System of the wasix package.
type WASIX struct{}

func (WASIX) Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error) {
	return wasix.Spawn(name, argv, attr)
}
func (WASIX) Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error) {
	return wasix.Wait(ctx, pid)
}
func (WASIX) Pipe() (*os.File, *os.File, error) { return wasix.Pipe() }
func (WASIX) Inheritable() []int                { return wasix.Inheritable() }

// DefaultSystem is the System of a Cmd whose System is nil.
var DefaultSystem System = WASIX{}

func (c *Cmd) sys() System {
	if c.System != nil {
		return c.System
	}
	return DefaultSystem
}

// Cmd is a program being prepared or run.
type Cmd struct {
	// Path is the program; Command resolves a name without a slash on
	// PATH.
	Path string
	// Args holds the command line, Args[0] included.
	Args []string
	// Env is the environment; nil means os.Environ(). The last value of
	// a duplicate key wins. When Dir is set, PWD=Dir is added: Go's
	// wasip1 port takes its working directory from $PWD.
	Env []string
	// Dir is the working directory; empty means the caller's.
	Dir string
	// Stdin, Stdout and Stderr as in os/exec: an *os.File is handed to
	// the child; any other reader or writer goes through a pipe and a
	// goroutine; nil is a closed (stdin) or drained (stdout, stderr)
	// pipe.
	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// Process is set by Start; ProcessState by Wait.
	Process      *Process
	ProcessState *ProcessState

	// Err is the LookPath error of Command, returned by Start.
	Err error

	// System starts and waits for the child; nil is DefaultSystem.
	System System

	closeAfterStart []io.Closer // child ends of pipes
	closeAfterWait  []io.Closer // parent ends
	copies          []func() error
	errs            chan error
	started         bool
	waited          bool
}

// Process is a started child.
type Process struct {
	Pid int
}

// ProcessState is how a child ended.
type ProcessState struct {
	pid    int
	status wasix.Status
}

// Pid is the child's process id.
func (p *ProcessState) Pid() int { return p.pid }

// ExitCode is the exit code, or 128+n for a signal n.
func (p *ProcessState) ExitCode() int { return p.status.ExitCode() }

// Exited reports whether the child exited (rather than being signalled).
func (p *ProcessState) Exited() bool { return p.status.Kind == wasix.StatusExited }

// Success reports whether the child exited with status 0.
func (p *ProcessState) Success() bool { return p.Exited() && p.status.Code == 0 }

// Sys returns the wasix.Status.
func (p *ProcessState) Sys() any { return p.status }

func (p *ProcessState) String() string {
	if p.status.Kind == wasix.StatusSignaled {
		return fmt.Sprintf("signal: %d", p.status.Signal)
	}
	return fmt.Sprintf("exit status %d", p.status.Code)
}

// ExitError is the error of a child that did not succeed.
type ExitError struct {
	*ProcessState
	// Stderr holds the child's standard error when Output collected it.
	Stderr []byte
}

func (e *ExitError) Error() string { return e.ProcessState.String() }

// Command returns a Cmd for name and its arguments. A name without a
// slash is looked up on PATH; a failure is kept in Err.
func Command(name string, arg ...string) *Cmd {
	c := &Cmd{Path: name, Args: append([]string{name}, arg...)}
	if !strings.Contains(name, "/") {
		p, err := LookPath(name)
		if err != nil {
			c.Err = err
		} else {
			c.Path = p
		}
	}
	return c
}

// String is the command line, for messages.
func (c *Cmd) String() string { return strings.Join(append([]string{c.Path}, c.Args[1:]...), " ") }

// Run starts the command and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// Output runs the command and returns its standard output; a failure's
// ExitError carries the standard error when Stderr was nil.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	captured := c.Stderr == nil
	if captured {
		c.Stderr = &errb
	}
	err := c.Run()
	var ee *ExitError
	if captured && errors.As(err, &ee) {
		ee.Stderr = errb.Bytes()
	}
	return out.Bytes(), err
}

// CombinedOutput runs the command and returns its standard output and
// standard error together.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil || c.Stderr != nil {
		return nil, errors.New("exec: Stdout or Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.Bytes(), err
}

// StdinPipe returns a pipe to the command's standard input; close it to
// signal end of input.
func (c *Cmd) StdinPipe() (io.WriteCloser, error) {
	if c.Stdin != nil {
		return nil, errors.New("exec: Stdin already set")
	}
	if c.started {
		return nil, errors.New("exec: StdinPipe after process started")
	}
	r, w, err := c.sys().Pipe()
	if err != nil {
		return nil, err
	}
	c.Stdin = r
	c.closeAfterStart = append(c.closeAfterStart, r)
	c.closeAfterWait = append(c.closeAfterWait, w)
	return w, nil
}

// StdoutPipe returns a pipe from the command's standard output. Wait
// closes it after the command exits: read everything before Wait.
func (c *Cmd) StdoutPipe() (io.ReadCloser, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	return c.outPipe(&c.Stdout)
}

// StderrPipe is StdoutPipe for standard error.
func (c *Cmd) StderrPipe() (io.ReadCloser, error) {
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	return c.outPipe(&c.Stderr)
}

func (c *Cmd) outPipe(dst *io.Writer) (io.ReadCloser, error) {
	if c.started {
		return nil, errors.New("exec: pipe after process started")
	}
	r, w, err := c.sys().Pipe()
	if err != nil {
		return nil, err
	}
	*dst = w
	c.closeAfterStart = append(c.closeAfterStart, w)
	c.closeAfterWait = append(c.closeAfterWait, r)
	return r, nil
}

// fileFd returns the descriptor of f without switching it to blocking
// mode (os.File.Fd would, and Go's wasip1 runtime needs it non-blocking).
func fileFd(f *os.File) (int, bool) {
	fd := -1
	if rc, err := f.SyscallConn(); err == nil {
		rc.Control(func(u uintptr) { fd = int(u) }) // fails (fd stays -1) once closed
	}
	return fd, fd >= 0
}

// lockedWriter serialises the copies into one writer (Stdout == Stderr).
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Start starts the command without waiting for it.
func (c *Cmd) Start() error {
	if c.started {
		return errors.New("exec: already started")
	}
	c.started = true
	if c.Err != nil {
		c.closeAll(c.closeAfterStart)
		c.closeAll(c.closeAfterWait)
		return c.Err
	}
	if len(c.Args) == 0 {
		c.Args = []string{c.Path}
	}
	stdout, stderr := c.Stdout, c.Stderr
	if stdout != nil && stdout == stderr {
		lw := lockedWriter{new(sync.Mutex), stdout}
		stdout, stderr = lw, lw
	}
	files := make([]int, 3)
	for i, s := range []any{c.Stdin, stdout, stderr} {
		fd, err := c.childFd(i, s)
		if err != nil {
			c.closeAll(c.closeAfterStart)
			c.closeAll(c.closeAfterWait)
			return err
		}
		files[i] = fd
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	env = dedupEnv(env)
	if c.Dir != "" {
		env = setEnv(env, "PWD", c.Dir)
	}
	pid, err := c.sys().Spawn(c.Path, c.Args, &wasix.Attr{Env: env, Dir: c.Dir, Files: files, Close: c.sys().Inheritable()})
	// The child holds its own copies of the pipe ends now; the parent's
	// must go so that readers see EOF when the child exits.
	c.closeAll(c.closeAfterStart)
	if err != nil {
		c.closeAll(c.closeAfterWait)
		if errors.Is(err, wasix.ENOENT) {
			return &Error{c.Path, err}
		}
		return fmt.Errorf("exec: %s: %w", c.Path, err)
	}
	c.Process = &Process{Pid: int(pid)}
	c.errs = make(chan error, len(c.copies))
	for _, f := range c.copies {
		go func() { c.errs <- f() }()
	}
	return nil
}

// childFd arranges stream i (0 stdin, 1 stdout, 2 stderr) and returns the
// parent fd the child gets.
func (c *Cmd) childFd(i int, s any) (int, error) {
	if f, ok := s.(*os.File); ok && f != nil {
		if fd, ok := fileFd(f); ok {
			return fd, nil
		}
	}
	r, w, err := c.sys().Pipe()
	if err != nil {
		return -1, err
	}
	if i == 0 {
		c.closeAfterStart = append(c.closeAfterStart, r)
		fd, _ := fileFd(r)
		if s == nil || isNilFile(s) {
			w.Close() // end of input at once
			return fd, nil
		}
		src := s.(io.Reader)
		c.copies = append(c.copies, func() error {
			_, err := io.Copy(w, src)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if errors.Is(err, os.ErrClosed) || errors.Is(err, wasix.EPIPE) || errors.Is(err, syscall.EPIPE) {
				err = nil // the child stopped reading
			}
			return err
		})
		return fd, nil
	}
	c.closeAfterStart = append(c.closeAfterStart, w)
	fd, _ := fileFd(w)
	dst := io.Discard
	if s != nil && !isNilFile(s) {
		dst = s.(io.Writer)
	}
	c.copies = append(c.copies, func() error {
		_, err := io.Copy(dst, r)
		r.Close()
		return err
	})
	return fd, nil
}

func isNilFile(s any) bool {
	f, ok := s.(*os.File)
	return ok && f == nil
}

func (c *Cmd) closeAll(cs []io.Closer) {
	for _, x := range cs {
		x.Close()
	}
}

// Wait waits for the command to exit and for its copies to finish.
func (c *Cmd) Wait() error { return c.WaitContext(context.Background()) }

// WaitContext is Wait that gives up when ctx is done. The child is not
// stopped (WASIX has no signals here) and its copies are left to end
// with it.
func (c *Cmd) WaitContext(ctx context.Context) error {
	if c.Process == nil {
		return errors.New("exec: not started")
	}
	if c.waited {
		return errors.New("exec: Wait was already called")
	}
	c.waited = true
	st, err := c.sys().Wait(ctx, wasix.Pid(c.Process.Pid))
	if err != nil {
		c.closeAll(c.closeAfterWait)
		return fmt.Errorf("exec: wait %s: %w", c.Path, err)
	}
	var copyErr error
	for range c.copies {
		if e := <-c.errs; e != nil && copyErr == nil {
			copyErr = e
		}
	}
	c.closeAll(c.closeAfterWait)
	c.ProcessState = &ProcessState{pid: c.Process.Pid, status: st}
	if !c.ProcessState.Success() {
		return &ExitError{ProcessState: c.ProcessState}
	}
	return copyErr
}

// dedupEnv keeps the last value of each key, in first-seen order.
func dedupEnv(env []string) []string {
	idx := map[string]int{}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	return out
}

// setEnv sets k=v as the last entry, dropping an earlier k.
func setEnv(env []string, k, v string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return append(out, k+"="+v)
}
