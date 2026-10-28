//go:build wasip1

package main

import (
	"context"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/go-wasmcontainers/shell/wasix"
	"mvdan.cc/sh/v3/interp"
)

// setupPipes makes the interpreter create its pipes with WASIX fd_pipe:
// os.Pipe is ENOSYS on wasip1.
func setupPipes() { interp.SetPipeFunc(wasix.Pipe) }

// openFile opens a redirection target in blocking mode.
//
// os.OpenFile on wasip1 puts every file it opens in non-blocking mode and
// on the netpoller. Regular files never need it, and wasmer 7.5.0 answers
// non-blocking writes to a file with EAGAIN once its buffer is busy and
// is slow to report it writable again: a loop writing 2000 lines into a
// redirected file did not finish in a minute, against 0.2s for 200.
// syscall.Open plus os.NewFile keeps the descriptor blocking.
func openFile(ctx context.Context, name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	if name != "" && !path.IsAbs(name) {
		name = path.Join(interp.HandlerCtx(ctx).Dir, name)
	}
	fd, err := syscall.Open(name, flag, uint32(perm.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	if fd > 2 {
		wasix.Track(fd)
	}
	return os.NewFile(uintptr(fd), name), nil
}
