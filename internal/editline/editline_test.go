package editline

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode"
)

func ed(in string) (*Editor, *bytes.Buffer) {
	var out bytes.Buffer
	return &Editor{In: strings.NewReader(in), Out: &out}, &out
}

func TestReadLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"echo hi\r", "echo hi"},
		{"echo hi\n", "echo hi"},
		{"\x1b[Dexit 4\r", "exit 4"},                 // an arrow on an empty line
		{"xit\x1b[D\x1b[D\x1b[De\r", "exit"},         // Left, insert
		{"xit\x1bOD\x1bOD\x1bODe\r", "exit"},         // SS3 Left
		{"xit\x02\x02\x02e\r", "exit"},               // ^B
		{"ab\x1b[D\x1b[D\x1b[Cx\x06\x06y\r", "axby"}, // Right, ^F, past the end
		{"\xf0\x9f\x98\x80!\r", "\U0001F600!"},       // 4-byte UTF-8
		{"exi\x1b[H\x1b[Ft\r", "exit"},               // Home, End
		{"xit\x01e\x05!\r", "exit!"},                 // ^A, ^E
		{"ab\x1b[1~x\x1b[4~y\r", "xaby"},             // CSI 1~ / 4~
		{"ab\x1b[7~x\x1b[8~y\r", "xaby"},             // CSI 7~ / 8~
		{"abc\x7f\x08d\r", "ad"},                     // DEL, ^H
		{"abc\x01\x1b[3~\r", "bc"},                   // Delete
		{"abc\x01\x04\r", "bc"},                      // ^D on a line deletes
		{"abc\x02\x02\x0b\r", "a"},                   // ^K
		{"abc\x02\x15\r", "c"},                       // ^U
		{"ls -l /tmp\x17\x17x\r", "ls x"},            // ^W twice
		{"a\x1b[1;5C\x1b[99Zb\r", "ab"},              // unknown CSI: ignored
		{"a\x1bxb\r", "ab"},                          // Alt-x: ignored
		{"a\x07\x00b\r", "ab"},                       // other controls: ignored
		{"caf\xc3\xa9 \xe2\x82\xac\x1b[D\x7f\r", "café€"},
		{"a\xff\xc3b\r", "a"}, // invalid UTF-8 dropped
		{"\x1b[" + strings.Repeat("1", 100) + "Dab\r", "ab"},
		{"\x0cls\r", "ls"}, // ^L
		{"\x1b[2\x01x\r", "x"},
		{"\x1b[\x7fx\r", "x"},
		{"\x1b\x7fx\r", "x"},
	} {
		e, _ := ed(c.in)
		got, err := e.ReadLine("$ ")
		if err != nil || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
		}
	}
}

func TestEndings(t *testing.T) {
	e, out := ed("\x04")
	if _, err := e.ReadLine("$ "); err != io.EOF {
		t.Errorf("^D on an empty line: %v", err)
	}
	if !strings.HasSuffix(out.String(), "\r\n") {
		t.Errorf("^D output %q", out.String())
	}
	e, out = ed("abc\x03")
	if _, err := e.ReadLine("$ "); err != ErrInterrupt || !strings.HasSuffix(out.String(), "abc\x1b[K^C\r\n") {
		t.Errorf("^C: %v %q", err, out.String())
	}
	e, _ = ed("partial")
	if l, err := e.ReadLine("$ "); err != nil || l != "partial" {
		t.Errorf("end of input after text: %q %v", l, err)
	}
	e, _ = ed("")
	if _, err := e.ReadLine("$ "); err != io.EOF {
		t.Errorf("end of input: %v", err)
	}
	for _, in := range []string{"\x1b", "\x1b[", "\x1bO", "\xe2\x82"} {
		e, _ = ed(in)
		if _, err := e.ReadLine("$ "); err != io.EOF {
			t.Errorf("%q cut short: %v", in, err)
		}
	}
	boom := errors.New("boom")
	e = &Editor{In: io.MultiReader(strings.NewReader("ab"), errReader{boom}), Out: io.Discard}
	if _, err := e.ReadLine("$ "); err != boom {
		t.Errorf("read error: %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// zeroReader returns (0, nil) once, as a non-blocking source may.
type zeroReader struct {
	r    io.Reader
	done bool
}

func (z *zeroReader) Read(p []byte) (int, error) {
	if !z.done {
		z.done = true
		return 0, nil
	}
	return z.r.Read(p)
}

func TestZeroRead(t *testing.T) {
	e := &Editor{In: &zeroReader{r: strings.NewReader("x\r")}, Out: io.Discard}
	if l, err := e.ReadLine(""); l != "x" || err != nil {
		t.Errorf("%q %v", l, err)
	}
}

func TestHistory(t *testing.T) {
	// history: one two | one two one | then bounded to 3: two one two.
	in := strings.NewReader("one\rtwo\rtwo\r  \r\x1b[A\x1b[A\r\x10\x10\x10\x10\x0e\r\x1b[B\x1b[A\x1b[A\x1b[A\x1b[A\x1b[A\x1b[B\x1b[B\x1b[B\x1b[Btyped\x1b[A\x1b[B\r")
	e := &Editor{In: in, Out: io.Discard, HistorySize: 3}
	var got []string
	for {
		l, err := e.ReadLine("$ ")
		if err != nil {
			break
		}
		got = append(got, l)
	}
	// ^P past the oldest entry stays there; Down past the newest comes
	// back to the line being typed.
	want := []string{"one", "two", "two", "  ", "one", "two", "typed"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines %q, want %q", got, want)
	}
	// "two" twice in a row is kept once; blank lines are not kept; bounded.
	if h := strings.Join(e.History(), "|"); h != "one|two|typed" {
		t.Errorf("history %q", h)
	}
	if e2, _ := ed(""); len(e2.History()) != 0 {
		t.Error("new editor has history")
	}
}

func TestHistoryDefault(t *testing.T) {
	var b strings.Builder
	for i := range DefaultHistory + 10 {
		b.WriteString(strings.Repeat("x", i%7+1) + string(rune('a'+i%26)) + "\r")
	}
	e, _ := ed(b.String())
	for {
		if _, err := e.ReadLine(""); err != nil {
			break
		}
	}
	if n := len(e.History()); n != DefaultHistory {
		t.Errorf("history %d", n)
	}
}

func TestDraw(t *testing.T) {
	e, out := ed("ab\x1b[D\r")
	e.ReadLine("x\n$ ")
	want := "x\n" + "\r$ \x1b[K" + "\r$ a\x1b[K" + "\r$ ab\x1b[K" + "\r$ ab\x1b[K\x1b[1D" + "\r$ ab\x1b[K" + "\r\n"
	if out.String() != want {
		t.Errorf("draw\n got %q\nwant %q", out.String(), want)
	}
	// A line wider than the screen scrolls sideways.
	e, out = ed("abcdefghij\x01\r")
	e.Cols = func() int { return 6 }
	e.ReadLine("$ ")
	s := out.String()
	// Width 6, prompt 2: three runes and the cursor's column.
	if !strings.Contains(s, "\r$ hij\x1b[K\r\n") || !strings.Contains(s, "\r$ abc\x1b[K\x1b[3D") {
		t.Errorf("scroll %q", s)
	}
	// A prompt as wide as the screen is dropped from the redraw.
	e, out = ed("ab\r")
	e.Cols = func() int { return 3 }
	e.ReadLine("long> ")
	if !strings.Contains(out.String(), "\rab\x1b[K") {
		t.Errorf("narrow %q", out.String())
	}
	e, out = ed("\r")
	e.Cols = func() int { return 0 }
	e.ReadLine("$ ")
	if out.String() != "\r$ \x1b[K\r$ \x1b[K\r\n" {
		t.Errorf("cols 0 %q", out.String())
	}
}

func TestMaxLine(t *testing.T) {
	e, _ := ed(strings.Repeat("y", MaxLine+10) + "\r")
	if l, _ := e.ReadLine(""); len(l) != MaxLine {
		t.Errorf("len %d", len(l))
	}
}

// FuzzReadLine: any input ends without a panic, and the line holds only
// printable text.
func FuzzReadLine(f *testing.F) {
	for _, s := range []string{"echo hi\r", "\x1b[A\x1b[3~x\r", "caf\xc3\xa9\x1b[D\x7f\r", "\x1b[1;5C\x1bOA\x03", "\x04"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		e := &Editor{In: strings.NewReader(in), Out: io.Discard, Cols: func() int { return 10 }}
		for range 64 {
			l, err := e.ReadLine("$ ")
			for _, r := range l {
				if !unicode.IsPrint(r) {
					t.Fatalf("non-printable %q in %q", r, l)
				}
			}
			if err == io.EOF {
				return
			}
		}
	})
}
