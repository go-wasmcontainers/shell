// Package editline is the interactive shell's line editor: it reads the
// keys of a terminal in raw mode (no echo, no line buffering) and edits
// one line, the way ksh's and bash's emacs modes do, so that the arrow
// keys move the cursor instead of reaching the command as escape bytes.
//
// It is a small, dependency-free model of the engine's cli/lineedit
// (github.com/go-wasmcontainers/engine/cli/lineedit) without its
// completion, which needs the engine's CLI; the two should share one core
// once that one is split from the CLI.
//
// Keys: printable text; Enter (CR or LF); Backspace (DEL, ^H); Delete
// (CSI 3~, ^D on a non-empty line); Left/Right (CSI D/C, SS3 D/C, ^B/^F);
// Home/End (CSI H/F, CSI 1~/4~/7~/8~, ^A/^E); Up/Down and ^P/^N
// (history); ^U, ^K, ^W (kill); ^L (clear screen); ^C (abandon the line);
// ^D on an empty line (end of input). Any other key or escape sequence is
// ignored, never inserted.
package editline

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInterrupt is returned by ReadLine for ^C.
var ErrInterrupt = errors.New("interrupt")

// MaxLine bounds the line being edited, in runes.
const MaxLine = 4096

// maxSeq bounds an escape sequence's parameter bytes.
const maxSeq = 16

// DefaultHistory is the history size when Editor.HistorySize is 0.
const DefaultHistory = 500

// Editor edits lines read from In (one byte at a time, so nothing past
// the line is consumed) and draws them on Out.
type Editor struct {
	In  io.Reader
	Out io.Writer
	// Cols returns the terminal width (0 or less: 80).
	Cols func() int
	// HistorySize bounds the history (0: DefaultHistory).
	HistorySize int

	hist []string
}

// History returns the history, oldest first.
func (e *Editor) History() []string { return append([]string(nil), e.hist...) }

// state is the line being edited.
type state struct {
	e      *Editor
	prompt string // the prompt's last line (the part redrawn)
	line   []rune
	pos    int // cursor, in runes
	hi     int // history index; len(hist) is the line being typed
	saved  []rune
	off    int // first rune shown when the line is wider than the screen
	b      [1]byte
}

func (s *state) readByte() (byte, error) {
	for {
		n, err := s.e.In.Read(s.b[:])
		if n == 1 {
			return s.b[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// ReadLine shows prompt and returns the line typed, without its newline.
// It returns io.EOF for ^D on an empty line (or the end of In with
// nothing typed), ErrInterrupt for ^C.
func (e *Editor) ReadLine(prompt string) (string, error) {
	head, tail := "", prompt
	if i := strings.LastIndexByte(prompt, '\n'); i >= 0 {
		head, tail = prompt[:i+1], prompt[i+1:]
	}
	s := &state{e: e, prompt: tail, hi: len(e.hist)}
	io.WriteString(e.Out, head)
	s.draw()
	for {
		c, err := s.readByte()
		if err != nil {
			if len(s.line) > 0 && errors.Is(err, io.EOF) {
				return s.submit(), nil
			}
			io.WriteString(e.Out, "\r\n")
			return "", err
		}
		switch c {
		case '\r', '\n':
			return s.submit(), nil
		case 0x03: // ^C
			s.end()
			io.WriteString(e.Out, "^C\r\n")
			return "", ErrInterrupt
		case 0x04: // ^D
			if len(s.line) == 0 {
				io.WriteString(e.Out, "\r\n")
				return "", io.EOF
			}
			s.del()
		case 0x7f, 0x08:
			if s.pos > 0 {
				s.pos--
				s.del()
			}
		case 0x01:
			s.pos = 0
		case 0x05:
			s.pos = len(s.line)
		case 0x02:
			s.left()
		case 0x06:
			s.right()
		case 0x0b: // ^K
			s.line = s.line[:s.pos]
		case 0x15: // ^U
			s.line = append(s.line[:0], s.line[s.pos:]...)
			s.pos = 0
		case 0x17: // ^W
			s.killWord()
		case 0x0c: // ^L
			io.WriteString(e.Out, "\x1b[H\x1b[2J")
		case 0x10:
			s.history(-1)
		case 0x0e:
			s.history(1)
		case 0x1b:
			if err := s.escape(); err != nil {
				return "", err
			}
		default:
			if c < 0x20 {
				break // another control key: ignored
			}
			r := rune(c)
			if c >= utf8.RuneSelf {
				var err error
				if r, err = s.rune(c); err != nil {
					return "", err
				}
				if r == utf8.RuneError || !unicode.IsPrint(r) {
					break
				}
			}
			s.insert(r)
		}
		s.draw()
	}
}

// rune reads the rest of a UTF-8 sequence that starts with c.
func (s *state) rune(c byte) (rune, error) {
	n := 0
	switch {
	case c&0xe0 == 0xc0:
		n = 1
	case c&0xf0 == 0xe0:
		n = 2
	case c&0xf8 == 0xf0:
		n = 3
	default:
		return utf8.RuneError, nil
	}
	b := []byte{c}
	for range n {
		d, err := s.readByte()
		if err != nil {
			return 0, err
		}
		b = append(b, d)
	}
	r, _ := utf8.DecodeRune(b)
	return r, nil
}

// escape reads the rest of an escape sequence (ESC already read) and
// applies the keys it knows.
func (s *state) escape() error {
	c, err := s.readByte()
	if err != nil {
		return err
	}
	switch c {
	case 'O': // SS3: one final byte
		f, err := s.readByte()
		if err != nil {
			return err
		}
		s.key(f, "")
	case '[': // CSI: parameter bytes, then a final byte 0x40..0x7e
		var params []byte
		for {
			f, err := s.readByte()
			if err != nil {
				return err
			}
			if f >= 0x40 && f <= 0x7e {
				if len(params) <= maxSeq {
					s.key(f, string(params))
				}
				return nil
			}
			if f < 0x20 || f > 0x3f {
				return nil // not a CSI sequence after all: dropped
			}
			if len(params) <= maxSeq { // a longer one is consumed, not kept
				params = append(params, f)
			}
		}
	}
	return nil // ESC + another key (Alt-key): ignored
}

func (s *state) key(final byte, params string) {
	switch {
	case final == 'A':
		s.history(-1)
	case final == 'B':
		s.history(1)
	case final == 'C':
		s.right()
	case final == 'D':
		s.left()
	case final == 'H', final == '~' && (params == "1" || params == "7"):
		s.pos = 0
	case final == 'F', final == '~' && (params == "4" || params == "8"):
		s.pos = len(s.line)
	case final == '~' && params == "3":
		s.del()
	}
}

func (s *state) insert(r rune) {
	if len(s.line) >= MaxLine {
		return
	}
	s.line = append(s.line, 0)
	copy(s.line[s.pos+1:], s.line[s.pos:])
	s.line[s.pos] = r
	s.pos++
}

func (s *state) del() {
	if s.pos < len(s.line) {
		s.line = append(s.line[:s.pos], s.line[s.pos+1:]...)
	}
}

func (s *state) left() {
	if s.pos > 0 {
		s.pos--
	}
}

func (s *state) right() {
	if s.pos < len(s.line) {
		s.pos++
	}
}

func (s *state) killWord() {
	i := s.pos
	for i > 0 && s.line[i-1] == ' ' {
		i--
	}
	for i > 0 && s.line[i-1] != ' ' {
		i--
	}
	s.line = append(s.line[:i], s.line[s.pos:]...)
	s.pos = i
}

// history moves d entries through the history (-1 older, 1 newer),
// keeping the line being typed to come back to.
func (s *state) history(d int) {
	h := s.e.hist
	n := s.hi + d
	if n < 0 || n > len(h) {
		return
	}
	if s.hi == len(h) {
		s.saved = append(s.saved[:0], s.line...)
	}
	s.hi = n
	if n == len(h) {
		s.line = append(s.line[:0], s.saved...)
	} else {
		s.line = []rune(h[n])
	}
	s.pos = len(s.line)
}

func (s *state) submit() string {
	s.end()
	io.WriteString(s.e.Out, "\r\n")
	line := string(s.line)
	if strings.TrimSpace(line) != "" {
		h := s.e.hist
		if len(h) == 0 || h[len(h)-1] != line {
			h = append(h, line)
		}
		max := s.e.HistorySize
		if max <= 0 {
			max = DefaultHistory
		}
		if len(h) > max {
			h = append([]string(nil), h[len(h)-max:]...)
		}
		s.e.hist = h
	}
	return line
}

// end moves the cursor to the end of the line before leaving it.
func (s *state) end() {
	s.pos = len(s.line)
	s.draw()
}

func (s *state) cols() int {
	if s.e.Cols != nil {
		if c := s.e.Cols(); c > 0 {
			return c
		}
	}
	return 80
}

// draw redraws the prompt and the line on one screen row: a line wider
// than the screen scrolls sideways (one column per rune is assumed).
func (s *state) draw() {
	p := []rune(s.prompt)
	w := s.cols() - len(p) - 1
	if w < 1 {
		p, w = nil, s.cols()-1
	}
	if s.pos < s.off {
		s.off = s.pos
	}
	if s.pos > s.off+w {
		s.off = s.pos - w
	}
	end := min(s.off+w, len(s.line))
	var b strings.Builder
	b.WriteString("\r")
	b.WriteString(string(p))
	b.WriteString(string(s.line[s.off:end]))
	b.WriteString("\x1b[K")
	if back := end - s.pos; back > 0 {
		fmt.Fprintf(&b, "\x1b[%dD", back)
	}
	io.WriteString(s.e.Out, b.String())
}
