package main

import "github.com/go-wasmcontainers/shell/wasix"

// wasixTerm is stdin's terminal, through WASIX tty_get/tty_set: the
// host's line discipline (the engine's, for exec -t) is put in raw mode
// while the line editor reads a line.
type wasixTerm struct{}

var (
	ttyGet = wasix.TtyGet
	ttySet = wasix.TtySet
)

func (wasixTerm) Raw() (func(), error) {
	t, err := ttyGet()
	if err != nil {
		return nil, err
	}
	raw := t
	raw.Echo, raw.LineBuffered = false, false
	if err := ttySet(raw); err != nil {
		return nil, err
	}
	return func() { ttySet(t) }, nil
}

func (wasixTerm) Cols() int {
	t, err := ttyGet()
	if err != nil {
		return 0
	}
	return int(t.Cols)
}
