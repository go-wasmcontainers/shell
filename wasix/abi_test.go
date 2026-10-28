package wasix

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
)

func TestFdOpLayout(t *testing.T) {
	op := FdOp{Cmd: FdOpOpen, Fd: 7, SrcFd: 9, Path: "/etc/x", DirFlags: 1, OFlags: 0x9,
		RightsBase: 0x1122334455667788, RightsInheriting: 0x99, FdFlags: 4, FdFlagsExt: 1}
	b := make([]byte, FdOpSize)
	PutFdOp(b, op, 0xabcd)
	// Offsets from api_wasix.h's _Static_asserts.
	want := map[int]byte{0: 2, 4: 7, 8: 9, 12: 0xcd, 13: 0xab, 16: 6, 20: 1, 24: 9, 32: 0x88, 39: 0x11, 40: 0x99, 48: 4, 50: 1}
	for off, v := range want {
		if b[off] != v {
			t.Errorf("byte %d = %#x, want %#x", off, b[off], v)
		}
	}
	r, err := DecodeFdOp(b)
	if err != nil {
		t.Fatal(err)
	}
	op.Path = ""
	if r.FdOp != op || r.PathPtr != 0xabcd || r.PathLen != 6 {
		t.Errorf("round trip: %+v", r)
	}
	if _, err := DecodeFdOp(b[:10]); err != EINVAL {
		t.Errorf("short: %v", err)
	}
	b[0] = 9
	if _, err := DecodeFdOp(b); err != EINVAL {
		t.Errorf("bad cmd: %v", err)
	}
}

func TestStatus(t *testing.T) {
	for _, s := range []Status{
		{Kind: StatusNothing}, {Kind: StatusExited, Code: 3},
		{Kind: StatusSignaled, Code: 0, Signal: 9}, {Kind: StatusStopped, Signal: 19},
	} {
		b := make([]byte, JoinStatusSize)
		PutStatus(b, s)
		got, err := DecodeStatus(b)
		if err != nil || got != s {
			t.Errorf("%+v -> %+v, %v", s, got, err)
		}
	}
	if (Status{Kind: StatusExited, Code: 3}).ExitCode() != 3 ||
		(Status{Kind: StatusSignaled, Signal: 9}).ExitCode() != 137 ||
		(Status{Kind: StatusNothing}).ExitCode() != -1 {
		t.Error("ExitCode")
	}
	if _, err := DecodeStatus([]byte{7, 0, 0, 0, 0, 0}); err != EINVAL {
		t.Error("bad tag accepted")
	}
	if _, err := DecodeStatus(nil); err != EINVAL {
		t.Error("short accepted")
	}
}

func TestTty(t *testing.T) {
	tt := Tty{Cols: 80, Rows: 24, Width: 640, Height: 480, StdinTTY: true, StderrTTY: true, LineBuffered: true}
	b := make([]byte, TtySize)
	PutTty(b, tt)
	if b[16] != 1 || b[17] != 0 || b[18] != 1 || b[20] != 1 {
		t.Errorf("bools: %v", b[16:22])
	}
	got, err := DecodeTty(b)
	if err != nil || got != tt {
		t.Errorf("%+v %v", got, err)
	}
	if _, err := DecodeTty(b[:3]); err != EINVAL {
		t.Error("short accepted")
	}
}

func TestErrno(t *testing.T) {
	if !errors.Is(ENOENT, fs.ErrNotExist) || !errors.Is(EACCES, fs.ErrPermission) ||
		!errors.Is(ENOTCAPABLE, fs.ErrPermission) || !errors.Is(ENOSYS, errors.ErrUnsupported) ||
		errors.Is(EINVAL, fs.ErrNotExist) {
		t.Error("Is mapping")
	}
	if ENOEXEC.Error() != "exec format error" || !strings.Contains(Errno(999).Error(), "999") {
		t.Error("messages")
	}
	if errnoErr(0) != nil || errnoErr(44) != ENOENT {
		t.Error("errnoErr")
	}
}

func TestCheckArgv(t *testing.T) {
	if CheckArgv(nil, nil) != EINVAL {
		t.Error("empty argv")
	}
	if CheckArgv([]string{"a\x00b"}, nil) != EINVAL {
		t.Error("NUL")
	}
	if CheckArgv([]string{"a"}, make([]string, MaxEnv+1)) != E2BIG {
		t.Error("env count")
	}
	if CheckArgv([]string{strings.Repeat("x", MaxArgBytes)}, nil) != E2BIG {
		t.Error("size")
	}
	if CheckArgv([]string{"", "a b", "c\nd"}, []string{"K=v"}) != nil {
		t.Error("valid argv refused")
	}
}

func TestFdOps(t *testing.T) {
	cases := []struct {
		name string
		attr Attr
		want []FdOp
	}{
		{"identity", Attr{Files: []int{0, 1, 2}}, nil},
		{"chdir and dup", Attr{Dir: "/tmp", Files: []int{5, 1, 1}, Close: []int{5, 6}}, []FdOp{
			{Cmd: FdOpChdir, Path: "/tmp"},
			{Cmd: FdOpDup2, SrcFd: 5, Fd: 0},
			{Cmd: FdOpDup2, SrcFd: 1, Fd: 2},
			{Cmd: FdOpClose, Fd: 5},
			{Cmd: FdOpClose, Fd: 6},
		}},
		{"close stdin", Attr{Files: []int{-1, 1, 2}}, []FdOp{{Cmd: FdOpClose, Fd: 0}}},
		// fd 0 is overwritten before fd 1 reads it: it goes via a temp.
		{"swap", Attr{Files: []int{1, 0, 2}}, []FdOp{
			{Cmd: FdOpDup2, SrcFd: 0, Fd: 3},
			{Cmd: FdOpDup2, SrcFd: 1, Fd: 0},
			{Cmd: FdOpDup2, SrcFd: 3, Fd: 1},
			{Cmd: FdOpClose, Fd: 3},
		}},
		{"close list skips targets and duplicates", Attr{Files: []int{0, 1, 2}, Close: []int{1, 4, 4}}, []FdOp{
			{Cmd: FdOpClose, Fd: 4},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.attr.FdOps()
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// simulate applies ops to a table child[fd] = parent description.
func simulate(ops []FdOp, parent map[int]int) map[int]int {
	t := map[int]int{}
	for k, v := range parent {
		t[k] = v
	}
	for _, op := range ops {
		switch op.Cmd {
		case FdOpDup2:
			if v, ok := t[int(op.SrcFd)]; ok {
				t[int(op.Fd)] = v
			}
		case FdOpClose:
			delete(t, int(op.Fd))
		}
	}
	return t
}

func FuzzFdOps(f *testing.F) {
	f.Add([]byte{1, 0, 2}, []byte{5})
	f.Add([]byte{2, 0, 1}, []byte{})
	f.Fuzz(func(t *testing.T, files, closes []byte) {
		if len(files) > 8 || len(closes) > 8 {
			return
		}
		a := Attr{}
		parent := map[int]int{}
		for i := 0; i < 16; i++ {
			parent[i] = 100 + i
		}
		for _, b := range files {
			a.Files = append(a.Files, int(b%12)-1)
		}
		for _, b := range closes {
			a.Close = append(a.Close, int(b%12))
		}
		got := simulate(a.FdOps(), parent)
		for i, s := range a.Files {
			v, ok := got[i]
			if s < 0 {
				if ok {
					t.Fatalf("fd %d should be closed: %+v", i, a)
				}
				continue
			}
			if !ok || v != parent[s] {
				t.Fatalf("fd %d = %d, want %d (%+v, ops %+v)", i, v, parent[s], a, a.FdOps())
			}
		}
		for _, c := range a.Close {
			if c >= len(a.Files) {
				if _, ok := got[c]; ok {
					t.Fatalf("fd %d not closed", c)
				}
			}
		}
		// Temps must not leak.
		for fd := range got {
			if _, ok := parent[fd]; !ok {
				t.Fatalf("temp fd %d leaked", fd)
			}
		}
	})
}

func FuzzDecode(f *testing.F) {
	b := make([]byte, FdOpSize)
	PutFdOp(b, FdOp{Cmd: FdOpDup2, Fd: 1, SrcFd: 4}, 0)
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := DecodeFdOp(b); err == nil {
			out := make([]byte, FdOpSize)
			PutFdOp(out, r.FdOp, r.PathPtr)
			r2, err := DecodeFdOp(out)
			if err != nil || r2.FdOp != r.FdOp {
				t.Fatalf("fd op round trip: %+v %+v", r, r2)
			}
		}
		if s, err := DecodeStatus(b); err == nil {
			out := make([]byte, JoinStatusSize)
			PutStatus(out, s)
			if s2, _ := DecodeStatus(out); s2 != s {
				t.Fatalf("status round trip: %+v %+v", s, s2)
			}
		}
		if tt, err := DecodeTty(b); err == nil {
			out := make([]byte, TtySize)
			PutTty(out, tt)
			if t2, _ := DecodeTty(out); t2 != tt {
				t.Fatalf("tty round trip")
			}
		}
	})
}

func TestStubs(t *testing.T) {
	if _, err := Spawn("x", nil, nil); err != EINVAL {
		t.Errorf("empty argv: %v", err)
	}
	if _, err := Spawn("x", []string{"x"}, &Attr{Close: make([]int, MaxFdOps+1)}); err == nil {
		t.Error("too many ops accepted")
	}
}
