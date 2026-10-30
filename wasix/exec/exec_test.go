//go:build unix

package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-wasmcontainers/shell/wasix"
)

// fakeSystem runs "children" as goroutines over real native pipes: the
// fds a child gets are dup'ed at Spawn, as a host does, since the parent
// closes its copies right after.
type fakeSystem struct {
	mu                         sync.Mutex
	next                       wasix.Pid
	done                       map[wasix.Pid]chan wasix.Status
	attrs                      []wasix.Attr
	spawnErr, waitErr, pipeErr error
	pipes                      int
	failAt                     int // Pipe fails on this call (1-based), 0 never
}

func newFake() *fakeSystem { return &fakeSystem{done: map[wasix.Pid]chan wasix.Status{}} }

func (f *fakeSystem) Pipe() (*os.File, *os.File, error) {
	f.pipes++
	if f.pipeErr != nil && f.pipes == f.failAt {
		return nil, nil, f.pipeErr
	}
	return os.Pipe()
}

func (f *fakeSystem) Inheritable() []int { return []int{99} }

func dupFile(fd int) *os.File {
	if fd < 0 {
		return nil
	}
	n, err := syscall.Dup(fd)
	if err != nil {
		panic(err)
	}
	return os.NewFile(uintptr(n), "child")
}

func (f *fakeSystem) Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error) {
	if f.spawnErr != nil {
		return 0, f.spawnErr
	}
	f.mu.Lock()
	f.next++
	pid := f.next
	ch := make(chan wasix.Status, 1)
	f.done[pid] = ch
	f.attrs = append(f.attrs, *attr)
	f.mu.Unlock()
	in, out, errf := dupFile(attr.Files[0]), dupFile(attr.Files[1]), dupFile(attr.Files[2])
	go func() {
		code := child(argv, attr.Env, in, out, errf)
		for _, x := range []*os.File{in, out, errf} {
			if x != nil {
				x.Close()
			}
		}
		ch <- wasix.Status{Kind: wasix.StatusExited, Code: uint16(code)}
	}()
	return pid, nil
}

func (f *fakeSystem) Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error) {
	if f.waitErr != nil {
		return wasix.Status{}, f.waitErr
	}
	f.mu.Lock()
	ch := f.done[pid]
	f.mu.Unlock()
	return <-ch, nil
}

// child is the fake program table.
func child(argv, env []string, in, out, errf *os.File) int {
	switch filepath.Base(argv[0]) {
	case "echo":
		fmt.Fprintln(out, strings.Join(argv[1:], " "))
	case "cat":
		io.Copy(out, in)
	case "both":
		fmt.Fprint(out, "out;")
		fmt.Fprint(errf, "err;")
	case "env":
		for _, kv := range env {
			fmt.Fprintln(out, kv)
		}
	case "fail":
		n, _ := strconv.Atoi(argv[1])
		fmt.Fprint(errf, "failing")
		return n
	case "head1": // reads one byte and exits: its stdin writer sees EPIPE
		b := make([]byte, 1)
		in.Read(b)
		out.Write(b)
	}
	return 0
}

func withFake(t *testing.T) *fakeSystem {
	t.Helper()
	f := newFake()
	old := DefaultSystem
	DefaultSystem = f
	t.Cleanup(func() { DefaultSystem = old })
	return f
}

func cmd(name string, args ...string) *Cmd {
	return &Cmd{Path: "/bin/" + name, Args: append([]string{name}, args...)}
}

func TestRunAndOutput(t *testing.T) {
	withFake(t)
	out, err := cmd("echo", "hello", "world").Output()
	if err != nil || string(out) != "hello world\n" {
		t.Fatalf("%q %v", out, err)
	}
	c := cmd("cat")
	c.Stdin = strings.NewReader("piped")
	if out, err := c.Output(); err != nil || string(out) != "piped" {
		t.Errorf("stdin: %q %v", out, err)
	}
	if out, err := cmd("both").CombinedOutput(); err != nil || (string(out) != "out;err;" && string(out) != "err;out;") {
		t.Errorf("combined: %q %v", out, err)
	}
	// nil stdin is closed, nil stdout is drained.
	if err := cmd("cat").Run(); err != nil {
		t.Errorf("nil stdio: %v", err)
	}
}

func TestExitError(t *testing.T) {
	withFake(t)
	_, err := cmd("fail", "3").Output()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || string(ee.Stderr) != "failing" || ee.Success() || !ee.Exited() || ee.Error() != "exit status 3" {
		t.Fatalf("%#v", err)
	}
	if ee.Pid() == 0 || ee.Sys().(wasix.Status).Code != 3 {
		t.Errorf("state %+v", ee.ProcessState)
	}
	s := &ProcessState{status: wasix.Status{Kind: wasix.StatusSignaled, Signal: 9, Code: 0}}
	if s.String() != "signal: 9" || s.Exited() {
		t.Errorf("signalled: %s", s)
	}
	// Stderr set by the caller is not captured into the ExitError.
	var errb bytes.Buffer
	c := cmd("fail", "1")
	c.Stderr = &errb
	if _, err := c.Output(); !errors.As(err, &ee) || ee.Stderr != nil || errb.String() != "failing" {
		t.Errorf("caller's stderr: %v %q", err, errb.String())
	}
}

func TestEnvAndDir(t *testing.T) {
	f := withFake(t)
	c := cmd("env")
	c.Env = []string{"A=1", "B=2", "A=3"}
	c.Dir = "/work"
	out, err := c.Output()
	if err != nil || string(out) != "A=3\nB=2\nPWD=/work\n" {
		t.Errorf("%q %v", out, err)
	}
	if a := f.attrs[len(f.attrs)-1]; a.Dir != "/work" || !slices.Equal(a.Close, []int{99}) {
		t.Errorf("attr %+v", a)
	}
	c = cmd("env")
	c.Env = []string{"PWD=/old"}
	c.Dir = "/new"
	if out, _ := c.Output(); string(out) != "PWD=/new\n" {
		t.Errorf("PWD replaced: %q", out)
	}
	t.Setenv("FROM_PARENT", "yes")
	if out, _ := cmd("env").Output(); !strings.Contains(string(out), "FROM_PARENT=yes") {
		t.Error("nil Env is not the parent's environment")
	}
}

func TestPipes(t *testing.T) {
	withFake(t)
	c := cmd("cat")
	w, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "through pipes")
	w.Close()
	b, _ := io.ReadAll(r)
	if err := c.Wait(); err != nil || string(b) != "through pipes" {
		t.Errorf("%q %v", b, err)
	}
	c = cmd("both")
	er, _ := c.StderrPipe()
	c.Start()
	b, _ = io.ReadAll(er)
	if c.Wait(); string(b) != "err;" {
		t.Errorf("stderr pipe %q", b)
	}
}

func TestMisuse(t *testing.T) {
	f := withFake(t)
	c := cmd("echo")
	c.Stdout = io.Discard
	if _, err := c.Output(); err == nil {
		t.Error("Output with Stdout set")
	}
	if _, err := c.CombinedOutput(); err == nil {
		t.Error("CombinedOutput with Stdout set")
	}
	if _, err := c.StdoutPipe(); err == nil {
		t.Error("StdoutPipe with Stdout set")
	}
	c.Stderr = io.Discard
	if _, err := c.StderrPipe(); err == nil {
		t.Error("StderrPipe with Stderr set")
	}
	c.Stdin = strings.NewReader("")
	if _, err := c.StdinPipe(); err == nil {
		t.Error("StdinPipe with Stdin set")
	}
	if err := cmd("echo").Wait(); err == nil {
		t.Error("Wait before Start")
	}
	c = cmd("echo")
	c.Run()
	if err := c.Start(); err == nil {
		t.Error("second Start")
	}
	if err := c.Wait(); err == nil {
		t.Error("second Wait")
	}
	if _, err := c.StdinPipe(); err == nil {
		t.Error("StdinPipe after Start")
	}
	if _, err := c.StdoutPipe(); err == nil {
		t.Error("StdoutPipe after Start")
	}
	// Args defaults to Path.
	c = &Cmd{Path: "/bin/echo"}
	if out, _ := c.Output(); string(out) != "\n" || c.Args[0] != "/bin/echo" {
		t.Errorf("default Args: %q %q", out, c.Args)
	}
	// Spawn and wait errors.
	f.spawnErr = wasix.ENOENT
	var pe *Error
	if err := cmd("x").Run(); !errors.As(err, &pe) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("spawn ENOENT: %v", err)
	}
	f.spawnErr = wasix.ENOEXEC
	if err := cmd("x").Run(); err == nil || !strings.Contains(err.Error(), "exec: /bin/x") {
		t.Errorf("spawn ENOEXEC: %v", err)
	}
	f.spawnErr = nil
	f.waitErr = wasix.ECHILD
	if err := cmd("echo").Run(); err == nil || !strings.Contains(err.Error(), "wait") {
		t.Errorf("wait error: %v", err)
	}
	f.waitErr = nil
	// Pipe failures: for a stdin copy, a stdout copy, StdinPipe, StdoutPipe.
	for at := 1; at <= 3; at++ {
		f.pipes, f.failAt, f.pipeErr = 0, at, errors.New("EMFILE")
		if err := cmd("echo").Run(); err == nil || !strings.Contains(err.Error(), "EMFILE") {
			t.Errorf("pipe %d: %v", at, err)
		}
	}
	f.pipes, f.failAt = 0, 1
	if _, err := cmd("cat").StdinPipe(); err == nil {
		t.Error("StdinPipe pipe error")
	}
	f.pipes, f.failAt = 0, 1
	if _, err := cmd("cat").StdoutPipe(); err == nil {
		t.Error("StdoutPipe pipe error")
	}
	f.pipeErr = nil
}

// A Cmd's own System is used instead of DefaultSystem.
func TestCmdSystem(t *testing.T) {
	f := newFake()
	c := cmd("echo", "own")
	c.System = f
	if out, err := c.Output(); err != nil || string(out) != "own\n" || len(f.attrs) != 1 {
		t.Errorf("%q %v %d", out, err, len(f.attrs))
	}
}

// blockingWait is a System whose children never end.
type blockingWait struct{ *fakeSystem }

func (blockingWait) Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error) {
	<-ctx.Done()
	return wasix.Status{}, ctx.Err()
}

func TestWaitContext(t *testing.T) {
	c := cmd("echo")
	c.System = blockingWait{newFake()}
	r, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.WaitContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v", err)
	}
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Error("the StdoutPipe was left open")
	}
}

// A child that stops reading: the stdin copy's EPIPE is not an error.
func TestStdinNotRead(t *testing.T) {
	withFake(t)
	c := cmd("head1")
	c.Stdin = strings.NewReader(strings.Repeat("x", 1<<20))
	if out, err := c.Output(); err != nil || string(out) != "x" {
		t.Errorf("%q %v", out, err)
	}
}

// A failing writer is reported by Wait.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCopyError(t *testing.T) {
	withFake(t)
	c := cmd("echo", "x")
	c.Stdout = failWriter{}
	if err := c.Run(); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("%v", err)
	}
}

func TestCommandAndLookPath(t *testing.T) {
	withFake(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tool.wasm"), []byte("\x00asm"), 0o755)
	os.WriteFile(filepath.Join(dir, "plain"), []byte("#!"), 0o755)
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)
	t.Setenv("PATH", "/nonexistent:"+dir)
	c := Command("tool", "a")
	if c.Err != nil || c.Path != filepath.Join(dir, "tool.wasm") || !slices.Equal(c.Args, []string{"tool", "a"}) {
		t.Errorf("tool: %+v", c)
	}
	if c.String() != c.Path+" a" {
		t.Errorf("String %q", c.String())
	}
	if p, err := LookPath("plain"); err != nil || p != filepath.Join(dir, "plain") {
		t.Errorf("plain: %q %v", p, err)
	}
	c = Command("nosuch")
	if !errors.Is(c.Err, ErrNotFound) || c.Run() != c.Err || !strings.Contains(c.Err.Error(), "exec: nosuch") {
		t.Errorf("nosuch: %v", c.Err)
	}
	if _, err := LookPath("adir"); err == nil {
		t.Error("a directory matched")
	}
	if c := Command("./rel"); c.Err != nil || c.Path != "./rel" {
		t.Errorf("a name with a slash is not looked up: %+v", c)
	}
	// A failed Start closes the pipes a caller asked for.
	c = Command("nosuch")
	r, _ := c.StdoutPipe()
	c.Start()
	if _, err := r.Read(make([]byte, 1)); err == nil {
		t.Error("pipe left open")
	}
}

func TestFind(t *testing.T) {
	files := map[string]fs.FileMode{"/bin/a.wasm": 0, "/bin/b": 0, "/usr/b.wasm": 0, "/w/rel.wasm": 0, "/w/x.wasm": 0, "/d": fs.ModeDir}
	stat := func(p string) (fs.FileInfo, error) {
		if m, ok := files[p]; ok {
			return fakeInfo{m}, nil
		}
		return nil, fs.ErrNotExist
	}
	for _, c := range []struct{ path, name, want string }{
		{"/bin:/usr", "a", "/bin/a.wasm"},
		{"/bin:/usr", "b", "/bin/b"},
		{"/usr:/bin", "b", "/usr/b.wasm"},
		{"/bin", "a.wasm", "/bin/a.wasm"},
		{":/bin", "rel", "/w/rel.wasm"},
		{"/bin:.", "x", "/w/x.wasm"},
		{"", "./rel", "/w/rel.wasm"},
		{"", "/bin/../bin/a", "/bin/a.wasm"},
	} {
		if got, err := Find(stat, "/w", c.path, c.name); err != nil || got != c.want {
			t.Errorf("Find(%q, %q) = %q %v, want %q", c.path, c.name, got, err, c.want)
		}
	}
	for _, name := range []string{"", "nosuch", "/d", "./nosuch"} {
		if _, err := Find(stat, "/w", "/bin:/", name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if _, err := Find(stat, "/w", "/bin", "a.wasm.wasm"); err == nil {
		t.Error("a .wasm name got a second .wasm")
	}
}

type fakeInfo struct{ m fs.FileMode }

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.m }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.m.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// The real system, off wasip1: Start says the host cannot.
func TestUnsupportedNatively(t *testing.T) {
	c := cmd("echo")
	c.Stdout = io.Discard
	if err := c.Run(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("%v", err)
	}
	if _, err := LookPath("definitely-not-a-program-xyz"); err == nil {
		t.Error("LookPath found a ghost")
	}
}

// LookPath without a working directory resolves against /.
func TestLookPathNoCwd(t *testing.T) {
	defer func(g func() (string, error)) { getwd = g }(getwd)
	getwd = func() (string, error) { return "", errors.New("ENOENT") }
	t.Setenv("PATH", "")
	if _, err := LookPath("x"); err == nil {
		t.Error("found x")
	}
}

// A closed *os.File as stdin: its descriptor cannot be had, so it goes
// through a pipe, whose copy ends at once.
func TestClosedFileStdin(t *testing.T) {
	withFake(t)
	r, w, _ := os.Pipe()
	w.Close()
	r.Close()
	c := cmd("cat")
	c.Stdin = r
	if out, err := c.Output(); err != nil || len(out) != 0 {
		t.Errorf("%q %v", out, err)
	}
}

// The real system's calls, off wasip1: they fail as unsupported.
func TestWasixSystem(t *testing.T) {
	var s WASIX
	if _, err := s.Spawn("/bin/x", []string{"x"}, &wasix.Attr{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Spawn: %v", err)
	}
	if _, err := s.Wait(context.Background(), 1); err == nil {
		t.Error("Wait succeeded")
	}
	s.Inheritable()
}
