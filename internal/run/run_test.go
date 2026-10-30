//go:build unix

package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-wasmcontainers/shell/wasix"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// mapFS is an FS over fstest.MapFS with absolute paths.
type mapFS fstest.MapFS

func (m mapFS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(fstest.MapFS(m), strings.TrimPrefix(path.Clean(name), "/"))
}
func (m mapFS) Head(name string, n int) ([]byte, error) {
	b, err := fs.ReadFile(fstest.MapFS(m), strings.TrimPrefix(path.Clean(name), "/"))
	if len(b) > n {
		b = b[:n]
	}
	return b, err
}

var wasm = &fstest.MapFile{Data: []byte("\x00asm\x01\x00\x00\x00"), Mode: 0o755}

func testFS() mapFS {
	return mapFS{
		"bin/sh":         wasm,
		"bin/cat":        wasm,
		"bin/echo":       wasm,
		"bin/fail":       wasm,
		"bin/env":        wasm,
		"bin/argv0":      wasm,
		"bin/tool.wasm":  wasm,
		"usr/bin/only":   wasm,
		"bin/dir/x":      wasm,
		"bin/hello":      {Data: []byte("#!/bin/sh -e\necho hello\n")},
		"bin/plain":      {Data: []byte("echo plain\n")},
		"bin/blob":       {Data: []byte("ab\x00cd\n")},
		"bin/badinterp":  {Data: []byte("#!/nope\n")},
		"bin/emptybang":  {Data: []byte("#!\n")},
		"home/u/rel.sh":  {Data: []byte("echo rel\n")},
		"home/u/sub/bin": wasm,
	}
}

func TestLookPath(t *testing.T) {
	f := testFS()
	cases := []struct{ dir, path, name, want string }{
		{"/", "/bin:/usr/bin", "cat", "/bin/cat"},
		{"/", "/bin:/usr/bin", "tool", "/bin/tool.wasm"},
		{"/", "/bin:/usr/bin", "tool.wasm", "/bin/tool.wasm"},
		{"/", "/bin:/usr/bin", "only", "/usr/bin/only"},
		{"/", "/bin", "dir", ""}, // a directory never matches
		{"/home/u", "/bin", "./rel.sh", "/home/u/rel.sh"},
		{"/home/u", "/bin", "sub/bin", "/home/u/sub/bin"},
		{"/home/u", ":/bin", "rel.sh", "/home/u/rel.sh"}, // empty element = cwd
		{"/", "/bin", "nosuch", ""},
		{"/", "/bin", "", ""},
		{"/", "/bin", "/bin/../bin/cat", "/bin/cat"},
	}
	for _, c := range cases {
		got, err := LookPath(f, c.dir, c.path, c.name)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("LookPath(%q, %q, %q) = %q, %v; want %q", c.dir, c.path, c.name, got, err, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	f := testFS()
	cases := []struct {
		name      string
		kind      Kind
		interp, a string
	}{
		{"/bin/cat", KindWasm, "", ""},
		{"/bin/hello", KindShebang, "/bin/sh", "-e"},
		{"/bin/plain", KindScript, "", ""},
		{"/bin/blob", KindBinary, "", ""},
		{"/bin/emptybang", KindScript, "", ""},
	}
	for _, c := range cases {
		k, i, a, err := Classify(f, c.name)
		if err != nil || k != c.kind || i != c.interp || a != c.a {
			t.Errorf("Classify(%s) = %v %q %q %v", c.name, k, i, a, err)
		}
	}
	if _, _, _, err := Classify(f, "/nope"); err == nil {
		t.Error("missing file classified")
	}
}

func TestOSFS(t *testing.T) {
	d := t.TempDir()
	p := d + "/f"
	os.WriteFile(p, []byte("0123456789"), 0o644)
	b, err := OSFS{}.Head(p, 4)
	if err != nil || string(b) != "0123" {
		t.Errorf("Head = %q %v", b, err)
	}
	b, err = OSFS{}.Head(p, 100)
	if err != nil || string(b) != "0123456789" {
		t.Errorf("Head short = %q %v", b, err)
	}
	if _, err := (OSFS{}).Head(d+"/none", 4); err == nil {
		t.Error("missing file")
	}
	if fi, err := (OSFS{}).Stat(p); err != nil || fi.Size() != 10 {
		t.Error("Stat")
	}
}

func TestEnv(t *testing.T) {
	env := expand.ListEnviron("A=1", "B=2", "A=3")
	got := Env(env)
	if strings.Join(got, ",") != "A=3,B=2" {
		t.Errorf("Env = %v", got)
	}
}

// fakeSpawner runs Go functions as "programs", on duplicates of the fds
// the shell hands over, like a WASIX host would.
type fakeSpawner struct {
	mu       sync.Mutex
	next     wasix.Pid
	procs    map[wasix.Pid]chan int
	spawnErr error
	lastAttr *wasix.Attr
}

type prog func(argv, env []string, stdin io.Reader, stdout, stderr io.Writer) int

var progs = map[string]prog{
	"cat": func(_, _ []string, in io.Reader, out, _ io.Writer) int { io.Copy(out, in); return 0 },
	"echo": func(argv, _ []string, _ io.Reader, out, _ io.Writer) int {
		fmt.Fprintln(out, strings.Join(argv[1:], " "))
		return 0
	},
	"fail": func(argv, _ []string, _ io.Reader, _, errw io.Writer) int {
		fmt.Fprintln(errw, "failing")
		var n int
		fmt.Sscan(argv[1], &n)
		return n
	},
	"env": func(_, env []string, _ io.Reader, out, _ io.Writer) int {
		for _, kv := range env {
			fmt.Fprintln(out, kv)
		}
		return 0
	},
	"argv0": func(argv, _ []string, _ io.Reader, out, _ io.Writer) int {
		fmt.Fprintln(out, strings.Join(argv, "|"))
		return 0
	},
	"sh": func(argv, _ []string, _ io.Reader, out, _ io.Writer) int {
		fmt.Fprintln(out, "sh:"+strings.Join(argv, "|"))
		return 0
	},
}

func (f *fakeSpawner) Spawn(name string, argv []string, attr *wasix.Attr) (wasix.Pid, error) {
	if f.spawnErr != nil {
		return 0, f.spawnErr
	}
	f.mu.Lock()
	f.lastAttr = attr
	f.next++
	pid := f.next
	if f.procs == nil {
		f.procs = map[wasix.Pid]chan int{}
	}
	done := make(chan int, 1)
	f.procs[pid] = done
	f.mu.Unlock()
	p, ok := progs[path.Base(name)]
	if !ok {
		return 0, wasix.ENOENT
	}
	files := make([]*os.File, 3)
	for i := range files {
		if attr.Files[i] < 0 {
			continue
		}
		fd, err := syscall.Dup(attr.Files[i])
		if err != nil {
			return 0, err
		}
		files[i] = os.NewFile(uintptr(fd), "")
	}
	go func() {
		var in io.Reader = strings.NewReader("")
		if files[0] != nil {
			in = files[0]
		}
		code := p(argv, attr.Env, in, files[1], files[2])
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
		done <- code
	}()
	return pid, nil
}

func (f *fakeSpawner) Wait(ctx context.Context, pid wasix.Pid) (wasix.Status, error) {
	f.mu.Lock()
	done := f.procs[pid]
	f.mu.Unlock()
	select {
	case c := <-done:
		return wasix.Status{Kind: wasix.StatusExited, Code: uint16(c)}, nil
	case <-ctx.Done():
		return wasix.Status{}, ctx.Err()
	}
}
func (f *fakeSpawner) Pipe() (*os.File, *os.File, error) { return os.Pipe() }
func (f *fakeSpawner) Inheritable() []int                { return nil }

// syncBuf is a bytes.Buffer safe for the concurrent writers a pipeline
// has (in the real shell stdout and stderr are files).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func runScript(t *testing.T, sp *fakeSpawner, src string) (string, string, error) {
	t.Helper()
	r := &Runner{FS: testFS(), Spawner: sp}
	r.Script = func(ctx context.Context, hc interp.HandlerContext, p string, args []string) error {
		fmt.Fprintln(hc.Stdout, "script", p, len(args))
		return nil
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "t")
	if err != nil {
		t.Fatal(err)
	}
	var out, errb syncBuf
	ir, err := interp.New(
		interp.StdIO(nil, &out, &errb),
		interp.Env(expand.ListEnviron("PATH=/bin:/usr/bin", "HOME=/home/u")),
		interp.Dir("/"),
		interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return r.ExecHandler() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = ir.Run(ctx, file)
	return out.String(), errb.String(), err
}

func TestExecHandler(t *testing.T) {
	cases := []struct {
		src, out string
		status   int
	}{
		{"echo a | cat", "a\n", 0},
		{"/bin/echo x | cat | cat", "x\n", 0},
		{"fail 3", "", 3},
		{"fail 3; echo $?", "3\n", 0},
		{"nosuch; echo $?", "127\n", 0},
		{"blob; echo $?", "126\n", 0},
		{"badinterp; echo $?", "126\n", 0},
		{"hello a", "sh:/bin/sh|-e|/bin/hello|a\n", 0},
		{"plain a b", "script /bin/plain 3\n", 0},
		{"x=$(/bin/echo sub); echo \"[$x]\"", "[sub]\n", 0},
		{"argv0 1 2", "argv0|1|2\n", 0},
		{"tool; echo $?", "127\n", 0}, // tool.wasm exists but no such fake program: ENOENT from the host
		{"fail 2 2>&1 | cat", "failing\n", 0},
		{"UNEXPORTED=1; export K=v; env", "", 0},
	}
	for _, c := range cases {
		out, stderr, err := runScript(t, &fakeSpawner{}, c.src)
		var es interp.ExitStatus
		status := 0
		if errors.As(err, &es) {
			status = int(es)
		} else if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		if strings.HasPrefix(c.src, "UNEXPORTED") {
			// Order of the environment is not part of the contract.
			if !strings.Contains(out, "K=v\n") || !strings.Contains(out, "PWD=/\n") || strings.Contains(out, "UNEXPORTED") {
				t.Errorf("env: %q", out)
			}
			continue
		}
		if out != c.out || status != c.status {
			t.Errorf("%q: got %q status %d, want %q status %d (stderr %q)", c.src, out, status, c.out, c.status, stderr)
		}
	}
}

func TestSpawnErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		msg    string
	}{
		{wasix.ENOENT, 127, "not found"},
		{wasix.ENOSYS, 126, "does not implement"},
		{wasix.E2BIG, 126, "too long"},
		{wasix.EAGAIN, 126, "process limit"},
		{wasix.ENOMEM, 126, "process limit"},
		{wasix.EINVAL, 126, "invalid argument"},
		{syscall.EIO, 126, "input/output"},
		{errors.New("odd"), 126, "odd"},
	} {
		_, stderr, err := runScript(t, &fakeSpawner{spawnErr: c.err}, "cat")
		var es interp.ExitStatus
		if !errors.As(err, &es) || int(es) != c.status || !strings.Contains(stderr, c.msg) {
			t.Errorf("%v: status %v, stderr %q", c.err, err, stderr)
		}
	}
}

func TestFdPlan(t *testing.T) {
	sp := &fakeSpawner{}
	dir := t.TempDir()
	if _, _, err := runScript(t, sp, "cd "+dir+"; echo x | cat"); err != nil {
		t.Fatal(err)
	}
	a := sp.lastAttr
	if a.Dir != dir || len(a.Files) != 3 || a.Files[0] < 3 {
		t.Errorf("attr: %+v", a)
	}
	if a.Env[len(a.Env)-1] != "PWD="+dir {
		t.Errorf("PWD last: %v", a.Env)
	}
}

func TestTrace(t *testing.T) {
	var tr bytes.Buffer
	r := &Runner{FS: testFS(), Spawner: &fakeSpawner{}, Trace: &tr}
	file, _ := syntax.NewParser().Parse(strings.NewReader("echo hi"), "t")
	ir, _ := interp.New(interp.StdIO(nil, io.Discard, io.Discard),
		interp.Env(expand.ListEnviron("PATH=/bin")),
		interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return r.ExecHandler() }))
	ir.Run(context.Background(), file)
	ir.Run(context.Background(), &syntax.File{Stmts: []*syntax.Stmt{}})
	file, _ = syntax.NewParser().Parse(strings.NewReader("/bin/echo hi"), "t")
	ir.Run(context.Background(), file)
	if !strings.Contains(tr.String(), "spawn /bin/echo") || !strings.Contains(tr.String(), "spawned pid=") {
		t.Errorf("trace: %q", tr.String())
	}
}

func TestNoScriptHandler(t *testing.T) {
	r := &Runner{FS: testFS(), Spawner: &fakeSpawner{}}
	var errb bytes.Buffer
	file, _ := syntax.NewParser().Parse(strings.NewReader("plain"), "t")
	ir, _ := interp.New(interp.StdIO(nil, io.Discard, &errb),
		interp.Env(expand.ListEnviron("PATH=/bin")),
		interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return r.ExecHandler() }))
	err := ir.Run(context.Background(), file)
	var es interp.ExitStatus
	if !errors.As(err, &es) || es != 126 {
		t.Errorf("err = %v, %q", err, errb.String())
	}
}

func TestWASIXSpawnerStubs(t *testing.T) {
	var w WASIX
	if _, err := w.Spawn("x", []string{"x"}, &wasix.Attr{}); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Spawn: %v", err)
	}
	if _, err := w.Wait(context.Background(), 1); err == nil {
		t.Error("Wait")
	}
	if _, _, err := w.Pipe(); err == nil {
		t.Error("Pipe")
	}
	if len(w.Inheritable()) != 0 {
		t.Error("Inheritable")
	}
}
