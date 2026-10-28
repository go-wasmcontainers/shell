// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

//go:build !js

package interp

import (
	"io"
	"os"

	"golang.org/x/term"
)

// stdinFile is the runner's standard input. Outside of js/wasm it is
// always an [*os.File]: the only type that subprocesses can inherit,
// and one supporting cancellable reads via [os.File.SetReadDeadline].
type stdinFile = *os.File

// pipeFunc creates the pipes used for pipelines, here-documents and
// non-file standard input.
//
// PATCH (go-wasmcontainers/shell): upstream calls [os.Pipe] directly,
// which returns ENOSYS on GOOS=wasip1. [SetPipeFunc] lets a WASIX guest
// supply pipes made with fd_pipe instead.
var pipeFunc = os.Pipe

// SetPipeFunc replaces the function the interpreter uses to create pipes.
// A nil f restores [os.Pipe]. It must be called before any [Runner] runs.
//
// PATCH (go-wasmcontainers/shell): not in upstream mvdan.cc/sh.
func SetPipeFunc(f func() (r, w *os.File, err error)) {
	if f == nil {
		f = os.Pipe
	}
	pipeFunc = f
}

// newPipe returns an OS pipe; both ends are [*os.File] so they can
// be inherited by subprocesses.
func newPipe() (stdinFile, *os.File, error) {
	return pipeFunc()
}

// newStdinFile converts a reader into the runner's stdin. Readers
// other than [*os.File] require an [os.Pipe] and a copying goroutine,
// as an [os.File] is the only way to share a reader with subprocesses.
func newStdinFile(r io.Reader) (stdinFile, error) {
	switch r := r.(type) {
	case *os.File:
		return r, nil
	case nil:
		return nil, nil
	default:
		pr, pw, err := pipeFunc()
		if err != nil {
			return nil, err
		}
		go func() {
			io.Copy(pw, r)
			pw.Close()
		}()
		return pr, nil
	}
}

// stdinTerminal returns the file descriptor of the shell's stdin
// if it is a terminal.
// Note that we only call [os.File.Fd] on character devices,
// as it stops [os.File.SetReadDeadline] from working,
// which [Runner.readLine] needs to cancel blocking reads.
func stdinTerminal(stdin stdinFile) (int, bool) {
	if stdin == nil {
		return -1, false
	}
	fi, err := stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return -1, false
	}
	fd := int(stdin.Fd())
	return fd, term.IsTerminal(fd)
}
