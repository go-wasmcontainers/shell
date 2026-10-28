//go:build wasip1

package wasix

// These tests run on a WASIX host (CI: wasmer, through Go's
// go_wasip1_wasm_exec with GOWASIRUNTIME=wasmer). The test binary
// re-spawns itself as the child, like os/exec's helper-process tests.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	switch os.Getenv("WASIX_TEST_CHILD") {
	case "":
		os.Exit(m.Run())
	case "exit":
		n, _ := strconv.Atoi(os.Getenv("WASIX_TEST_CODE"))
		os.Exit(n)
	case "echo":
		syscall_SetBlocking0()
		io.WriteString(os.Stdout, strings.Join(os.Args[1:], ","))
		os.Exit(0)
	case "cat":
		syscall_SetBlocking0()
		io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	}
	os.Exit(99)
}

// syscall_SetBlocking0 works around wasmer's hard-coded fdstat for fds
// 0-2 (see internal/e2e/testdata/tbox).
func syscall_SetBlocking0() { setBlocking(0) }

func self(t *testing.T) string {
	p := os.Args[0]
	if _, err := os.Stat(p); err != nil {
		t.Skipf("cannot see the test binary %s: %v", p, err)
	}
	return p
}

func childEnv(kind string, extra ...string) []string {
	return append([]string{"WASIX_TEST_CHILD=" + kind}, extra...)
}

func TestGetpid(t *testing.T) {
	pid, err := Getpid()
	if err != nil || pid == 0 {
		t.Fatalf("Getpid = %d, %v", pid, err)
	}
	if _, err := Getppid(); err != nil {
		t.Logf("Getppid: %v (a top-level process may have no parent)", err)
	}
}

func TestSpawnExitStatus(t *testing.T) {
	p := self(t)
	// The exit code travels in a __wasi_errno_t. wasmer 7.5.0 converts it
	// through its Errno enum, so codes above 80 come back as 79
	// (__WASI_ERRNO_UNKNOWN); 78 is the largest code that survives there.
	// The engine passes the raw u16 and its own tests check 255.
	for _, code := range []int{0, 3, 78} {
		pid, err := Spawn(p, []string{p}, &Attr{Env: childEnv("exit", "WASIX_TEST_CODE="+strconv.Itoa(code)), Files: []int{0, 1, 2}})
		if err != nil {
			t.Fatal(err)
		}
		st, err := Wait(context.Background(), pid)
		if err != nil || st.Kind != StatusExited || st.ExitCode() != code {
			t.Errorf("exit %d: %+v %v", code, st, err)
		}
	}
}

func TestSpawnNotFound(t *testing.T) {
	_, err := Spawn("/no/such/program", []string{"x"}, nil)
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ENOEXEC) {
		t.Errorf("err = %v", err)
	}
}

func TestPipeToChild(t *testing.T) {
	p := self(t)
	inR, inW, err := Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := Pipe()
	if err != nil {
		t.Fatal(err)
	}
	in, _ := fileFdT(inR)
	out, _ := fileFdT(outW)
	pid, err := Spawn(p, []string{p}, &Attr{Env: childEnv("cat"), Files: []int{in, out, 2}, Close: Inheritable()})
	if err != nil {
		t.Fatal(err)
	}
	inR.Close()
	outW.Close()
	go func() {
		io.WriteString(inW, "through a pipe")
		inW.Close()
	}()
	b, err := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if st, err := Wait(ctx, pid); err != nil || st.ExitCode() != 0 {
		t.Fatalf("wait: %+v %v", st, err)
	}
	if string(b) != "through a pipe" {
		t.Errorf("got %q", b)
	}
	outR.Close()
	for _, fd := range Inheritable() {
		if fd == in || fd == out {
			t.Errorf("closed fd %d still tracked", fd)
		}
	}
}

// Two goroutines of this process on the two ends of one pipe, with more
// data than any pipe buffer: the non-blocking ends and Go's poller must
// keep both running in the single-threaded wasip1 runtime.
func TestPipeBetweenGoroutines(t *testing.T) {
	r, w, err := Pipe()
	if err != nil {
		t.Fatal(err)
	}
	const lines = 20000
	done := make(chan int)
	readerFirst := os.Getenv("WASIX_HOST_POLLS_PIPES") != ""
	reader := func() {
		b, _ := io.ReadAll(r)
		done <- strings.Count(string(b), "\n")
	}
	if readerFirst {
		// Let the reader park on the poller with the pipe still empty.
		// wasmer 7.5.0 never returns from poll_oneoff then, not even for
		// the clock subscription, and the whole module freezes; so this
		// order only runs on hosts that say they poll pipes
		// (WASIX_HOST_POLLS_PIPES=1, the engine's tests).
		go reader()
		time.Sleep(50 * time.Millisecond)
	}
	go func() {
		for i := 0; i < lines; i++ {
			if _, err := io.WriteString(w, "line "+strconv.Itoa(i)+"\n"); err != nil {
				t.Error(err)
				break
			}
		}
		w.Close()
	}()
	if !readerFirst {
		go reader()
	}
	select {
	case n := <-done:
		if n != lines {
			t.Errorf("read %d lines", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("pipe between goroutines stalled")
	}
}

func TestArgvEmptyAndNewline(t *testing.T) {
	p := self(t)
	r, w, err := Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := fileFdT(w)
	pid, err := Spawn(p, []string{p, "", "a\nb", "c"}, &Attr{Env: childEnv("echo"), Files: []int{0, out, 2}, Close: Inheritable()})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	b, _ := io.ReadAll(r)
	Wait(context.Background(), pid)
	if string(b) != ",a\nb,c" {
		t.Errorf("argv seen by child: %q", b)
	}
}

func fileFdT(f *os.File) (int, bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return -1, false
	}
	fd := -1
	rc.Control(func(u uintptr) { fd = int(u) })
	return fd, fd >= 0
}

func TestTtyHost(t *testing.T) {
	tt, err := TtyGet()
	if err != nil {
		t.Skipf("tty_get: %v", err)
	}
	if err := TtySet(tt); err != nil {
		t.Errorf("tty_set: %v", err)
	}
}
