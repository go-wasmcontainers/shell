//go:build !wasip1

package main

import (
	"context"
	"io"
	"os"

	"mvdan.cc/sh/v3/interp"
)

// setupPipes keeps the interpreter's own pipes: os.Pipe on most systems,
// io.Pipe on js/wasm (where interp.SetPipeFunc does not exist).
func setupPipes() {}

// openFile is the interpreter's default outside wasip1 (where sh does
// not start programs anyway).
func openFile(ctx context.Context, name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	return interp.DefaultOpenHandler()(ctx, name, flag, perm)
}
