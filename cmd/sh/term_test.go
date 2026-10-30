package main

import (
	"errors"
	"testing"

	"github.com/go-wasmcontainers/shell/wasix"
)

func TestWasixTerm(t *testing.T) {
	defer func(g func() (wasix.Tty, error), s func(wasix.Tty) error) { ttyGet, ttySet = g, s }(ttyGet, ttySet)
	cur := wasix.Tty{Cols: 100, StdinTTY: true, Echo: true, LineBuffered: true}
	var set []wasix.Tty
	ttyGet = func() (wasix.Tty, error) { return cur, nil }
	ttySet = func(t wasix.Tty) error { set = append(set, t); return nil }
	restore, err := wasixTerm{}.Raw()
	if err != nil || len(set) != 1 || set[0].Echo || set[0].LineBuffered || set[0].Cols != 100 {
		t.Fatalf("raw: %v %+v", err, set)
	}
	restore()
	if len(set) != 2 || set[1] != cur {
		t.Fatalf("restore: %+v", set)
	}
	if c := (wasixTerm{}).Cols(); c != 100 {
		t.Errorf("cols %d", c)
	}
	boom := errors.New("boom")
	ttySet = func(wasix.Tty) error { return boom }
	if _, err := (wasixTerm{}).Raw(); err != boom {
		t.Errorf("set error: %v", err)
	}
	ttyGet = func() (wasix.Tty, error) { return wasix.Tty{}, boom }
	if _, err := (wasixTerm{}).Raw(); err != boom {
		t.Errorf("get error: %v", err)
	}
	if c := (wasixTerm{}).Cols(); c != 0 {
		t.Errorf("cols without a terminal %d", c)
	}
}
