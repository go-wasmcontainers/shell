package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeTerm records the mode switches of the line editor.
type fakeTerm struct {
	raw, restored int
	err           error
}

func (f *fakeTerm) Raw() (func(), error) {
	if f.err != nil {
		return nil, f.err
	}
	f.raw++
	return func() { f.restored++ }, nil
}

func (f *fakeTerm) Cols() int { return 40 }

// file returns a file that reads s, as the real stdin is (interp copies
// any other reader into a pipe of its own, which would read ahead).
func file(t *testing.T, s string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		w.WriteString(s)
		w.Close()
	}()
	t.Cleanup(func() { r.Close() })
	return r
}

func interactive(t *testing.T, term Terminal, stdin string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	sh := New(Config{
		Stdin: file(t, stdin), Stdout: &out, Stderr: &errb,
		Env: []string{"PATH=/bin"}, Dir: "/", IsTTY: true, Terminal: term,
	})
	code := sh.Main(context.Background(), Options{}, nil)
	return out.String(), errb.String(), code
}

// The terminal's keys go through the line editor: an arrow key moves
// the cursor instead of reaching the command (the dashboard's
// "sh: exit: not found").
func TestLineEditor(t *testing.T) {
	term := &fakeTerm{}
	out, errb, code := interactive(t, term,
		"\x1b[Decho hi\r"+ // an arrow before the command
			"cho b\x01e\x05y\r"+ // Home, End
			"echo dropped\x03"+ // ^C abandons the line
			"\x1b[A\x1b[A\r"+ // history: the line before last
			"if true\rthen echo more\rfi\r"+ // a continuation line
			"exit 3\r")
	if code != 3 || out != "hi\nby\nhi\nmore\n" {
		t.Errorf("out %q exit %d; stderr %q", out, code, errb)
	}
	if term.raw == 0 || term.raw != term.restored {
		t.Errorf("raw %d restored %d", term.raw, term.restored)
	}
	if !strings.Contains(errb, "$ echo hi") || !strings.Contains(errb, "> then echo more") || !strings.Contains(errb, "^C") {
		t.Errorf("prompts %q", errb)
	}
}

// ^D on an empty line ends the shell with the last command's status.
func TestLineEditorEOF(t *testing.T) {
	for _, c := range []struct {
		in   string
		code int
	}{
		{"\x04", 0},
		{"false\r\x04", 1},
		{"(exit 7)\r\x04", 7},
		{"false\rtrue\r\x04", 0},
		{"echo x\x03\x04", 130},
	} {
		if _, _, code := interactive(t, &fakeTerm{}, c.in); code != c.code {
			t.Errorf("%q: exit %d, want %d", c.in, code, c.code)
		}
	}
}

// A terminal that cannot be put in raw mode: the shell reads the
// terminal's own lines and prints its prompts itself.
func TestLineEditorFallback(t *testing.T) {
	term := &fakeTerm{err: errors.New("no tty_set")}
	out, errb, code := interactive(t, term, "echo a\necho b\nexit 2\n")
	if code != 2 || out != "a\nb\n" || errb != "$ $ $ " {
		t.Errorf("out %q stderr %q exit %d", out, errb, code)
	}
}

// Without a terminal, nothing changes: no prompts, no editor.
func TestNoTerminal(t *testing.T) {
	var out, errb bytes.Buffer
	sh := New(Config{Stdin: file(t, "echo \x1b[Dx\nexit 4\n"), Stdout: &out, Stderr: &errb, Dir: "/", Terminal: &fakeTerm{}})
	if code := sh.Main(context.Background(), Options{}, nil); code != 4 || out.String() != "\x1b[Dx\n" || errb.String() != "" {
		t.Errorf("out %q stderr %q exit %d", out.String(), errb.String(), code)
	}
}
