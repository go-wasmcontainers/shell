//go:build !wasip1

package main

import (
	"context"
	"io"
	"os"

	"mvdan.cc/sh/v3/interp"
)

// openFile is the interpreter's default outside wasip1 (where sh does
// not start programs anyway).
func openFile(ctx context.Context, name string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	return interp.DefaultOpenHandler()(ctx, name, flag, perm)
}
