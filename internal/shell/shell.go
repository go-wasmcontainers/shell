// Package shell is the sh command: mvdan.cc/sh's POSIX interpreter wired
// to WASIX process spawning (internal/run).
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/go-wasmcontainers/engine/lineedit"
	"github.com/go-wasmcontainers/shell/internal/complete"
	"github.com/go-wasmcontainers/shell/internal/run"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Config is what a shell needs from its surroundings.
type Config struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Env            []string
	Dir            string
	FS             run.FS
	Spawner        run.Spawner
	// IsTTY reports whether stdin is a terminal (prompts are printed).
	IsTTY bool
	// Open, if set, replaces the interpreter's open handler (used to
	// track the fds of redirections so children do not inherit them).
	Open interp.OpenHandlerFunc
	// Trace, if set, receives a line per program started.
	Trace io.Writer
	// Terminal, if set and stdin is a terminal, gives the interactive
	// shell its line editor (arrow keys, history) instead of the
	// terminal's own line mode.
	Terminal Terminal
	// DirFS, if set, gives the line editor Tab completion (builtins,
	// functions and programs on PATH for a command, file names for the
	// other words); it is how the completion reads directories.
	DirFS complete.FS
}

// Terminal is the control of stdin's terminal the line editor needs.
type Terminal interface {
	// Raw turns echo and line mode off; restore turns them back on.
	Raw() (restore func(), err error)
	// Cols is the terminal's width (0 if unknown).
	Cols() int
}

// Options are the sh invocation options.
type Options struct {
	Command     bool     // -c: the first operand is the script
	Set         []string // -e, -x, -u, ... passed to `set`
	Interactive bool     // -i
	ReadStdin   bool     // -s
}

// Shell runs scripts.
type Shell struct {
	cfg Config
	run *run.Runner
}

// New returns a shell for cfg.
func New(cfg Config) *Shell {
	s := &Shell{cfg: cfg}
	s.run = &run.Runner{FS: cfg.FS, Spawner: cfg.Spawner, Script: s.script, Trace: cfg.Trace}
	return s
}

func (s *Shell) options(stdin io.Reader, stdout, stderr io.Writer, env expand.Environ, dir string, params []string) []interp.RunnerOption {
	opts := []interp.RunnerOption{
		interp.StdIO(stdin, stdout, stderr),
		interp.Env(env),
		interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return s.run.ExecHandler() }),
	}
	if dir != "" {
		opts = append(opts, interp.Dir(dir))
	}
	if s.cfg.Open != nil {
		opts = append(opts, interp.OpenHandler(s.cfg.Open))
	}
	if params != nil {
		opts = append(opts, interp.Params(append([]string{"--"}, params...)...))
	}
	return opts
}

// script runs a text file without #! as a shell script in a fresh
// interpreter (POSIX XCU 2.9.1.6, ENOEXEC), with the caller's exported
// environment and stdio, sharing this shell's handlers.
func (s *Shell) script(ctx context.Context, hc interp.HandlerContext, path string, args []string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "sh: %s: %v\n", args[0], err)
		return interp.ExitStatus(126)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(string(src)), args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "sh:", err)
		return interp.ExitStatus(2)
	}
	r, err := interp.New(s.options(hc.Stdin, hc.Stdout, hc.Stderr,
		expand.ListEnviron(run.Env(hc.Env)...), hc.Dir, args[1:])...)
	if err != nil {
		return err
	}
	return r.Run(ctx, f)
}

// Status converts a Run error into an exit status, printing anything
// that is not a plain exit status.
func (s *Shell) Status(err error) int {
	if err == nil {
		return 0
	}
	var es interp.ExitStatus
	if errors.As(err, &es) {
		return int(es)
	}
	fmt.Fprintln(s.cfg.Stderr, "sh:", err)
	return 1
}

// Main runs sh with options o and operands args (POSIX sh synopsis):
//
//	sh [-eux...] [-c command_string [command_name [argument...]]]
//	sh [-eux...] [file [argument...]]
//	sh [-eux...] -s [argument...]
func (s *Shell) Main(ctx context.Context, o Options, args []string) int {
	env := expand.ListEnviron(s.cfg.Env...)
	var (
		src    io.Reader
		name   = "sh"
		params []string
	)
	switch {
	case o.Command:
		if len(args) == 0 {
			fmt.Fprintln(s.cfg.Stderr, "sh: -c: option requires an argument")
			return 2
		}
		src = strings.NewReader(args[0])
		if len(args) > 1 {
			name = args[1]
			params = args[2:]
		} else {
			params = []string{}
		}
	case len(args) > 0 && !o.ReadStdin:
		b, err := os.ReadFile(run.Abs(s.cfg.Dir, args[0]))
		if err != nil {
			fmt.Fprintf(s.cfg.Stderr, "sh: %s: %v\n", args[0], err)
			return 127
		}
		src = strings.NewReader(string(b))
		name = args[0]
		params = args[1:]
	default:
		params = args
		return s.interactive(ctx, o, env, params)
	}
	file, err := syntax.NewParser().Parse(src, name)
	if err != nil {
		fmt.Fprintln(s.cfg.Stderr, "sh:", err)
		return 2
	}
	r, err := s.newRunner(o, env, params)
	if err != nil {
		fmt.Fprintln(s.cfg.Stderr, "sh:", err)
		return 2
	}
	return s.Status(r.Run(ctx, file))
}

func (s *Shell) newRunner(o Options, env expand.Environ, params []string) (*interp.Runner, error) {
	opts := s.options(s.cfg.Stdin, s.cfg.Stdout, s.cfg.Stderr, env, s.cfg.Dir, params)
	if len(o.Set) > 0 {
		opts = append(opts, interp.Params(o.Set...))
	}
	if o.Interactive {
		opts = append(opts, interp.Interactive(true))
	}
	return interp.New(opts...)
}

type byteReader struct{ r io.Reader }

func (b byteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return b.r.Read(p)
}

// lineSource feeds the parser the lines of the line editor, one line at
// a time, the terminal in raw mode only while one is being typed: the
// commands it runs see the terminal as they expect it, in line mode. If
// the terminal cannot be switched, it falls back to the terminal's line
// mode (the prompt printed, the bytes read as they come).
type lineSource struct {
	ed     *lineedit.Reader
	ctx    context.Context
	term   Terminal
	plain  io.Reader
	stderr io.Writer
	prompt string
	buf    []byte
}

func (l *lineSource) Read(p []byte) (int, error) {
	if len(l.buf) == 0 {
		if l.ed == nil {
			return l.plain.Read(p)
		}
		restore, err := l.term.Raw()
		if err != nil {
			l.ed = nil
			fmt.Fprint(l.stderr, l.prompt)
			return l.plain.Read(p)
		}
		line, err := l.ed.ReadLine(l.ctx, l.prompt)
		restore()
		if err != nil {
			return 0, err
		}
		l.buf = []byte(line + "\n")
	}
	n := copy(p, l.buf)
	l.buf = l.buf[n:]
	return n, nil
}

// completer is the Tab completion of the interactive shell r: builtins,
// functions and programs on PATH for a command, file names otherwise, read
// from the live state of r (its PATH, its working directory, its functions)
// at each Tab.
func (s *Shell) completer(r *interp.Runner) *complete.Completer {
	return &complete.Completer{
		FS:   s.cfg.DirFS,
		Dir:  func() string { return r.Dir },
		Path: func() string { return r.Vars["PATH"].String() },
		Funcs: func() []string {
			return slices.Collect(maps.Keys(r.Funcs))
		},
	}
}

// interactive reads commands from stdin, one complete command at a time,
// printing $PS1 (default "$ ") when stdin is a terminal.
func (s *Shell) interactive(ctx context.Context, o Options, env expand.Environ, params []string) int {
	r, err := s.newRunner(o, env, params)
	if err != nil {
		fmt.Fprintln(s.cfg.Stderr, "sh:", err)
		return 2
	}
	// One byte at a time, like other shells on unseekable input: what
	// the parser has not consumed stays for the commands (`read`, a
	// child's stdin) instead of sitting in a read-ahead buffer.
	var in io.Reader = byteReader{s.cfg.Stdin}
	var src *lineSource
	if s.cfg.IsTTY && s.cfg.Terminal != nil {
		rd := &lineedit.Reader{In: s.cfg.Stdin, Out: s.cfg.Stderr, Cols: s.cfg.Terminal.Cols}
		if s.cfg.DirFS != nil {
			rd.Complete = s.completer(r).Complete
		}
		src = &lineSource{
			ed:    rd,
			ctx:   ctx,
			term:  s.cfg.Terminal,
			plain: in, stderr: s.cfg.Stderr,
		}
		in = src
	}
	prompt := func(more bool) {
		if !s.cfg.IsTTY && !o.Interactive {
			return
		}
		ps := "$ "
		if more {
			ps = "> "
		}
		if v := r.Vars["PS1"]; !more && v.IsSet() {
			ps = v.String()
		}
		if src != nil && src.ed != nil {
			src.prompt = ps // the editor shows it
			return
		}
		fmt.Fprint(s.cfg.Stderr, ps)
	}
	status := 0
	prompt(false)
	for {
		// A syntax error ends a parse; an interactive shell reports it
		// and starts a new one on the rest of the input.
		parser := syntax.NewParser()
		failed := false
		for stmts, err := range parser.InteractiveSeq(in) {
			if errors.Is(err, lineedit.ErrInterrupt) {
				status = 130 // ^C abandons the command being typed
				failed = true
				break
			}
			if err != nil {
				fmt.Fprintln(s.cfg.Stderr, "sh:", err)
				status = 2
				failed = true
				break
			}
			if parser.Incomplete() {
				prompt(true)
				continue
			}
			for _, st := range stmts {
				err := r.Run(ctx, st)
				status = s.Status(err)
				if r.Exited() {
					return status
				}
			}
			prompt(false)
		}
		if !failed || !(s.cfg.IsTTY || o.Interactive) {
			return status
		}
		prompt(false)
	}
}
