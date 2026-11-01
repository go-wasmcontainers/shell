package complete

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/lineedit"
	"mvdan.cc/sh/v3/interp"
)

type ent struct {
	name string
	mode fs.FileMode
}

func (e ent) Name() string               { return e.name }
func (e ent) IsDir() bool                { return e.mode.IsDir() }
func (e ent) Type() fs.FileMode          { return e.mode.Type() }
func (e ent) Info() (fs.FileInfo, error) { return nil, errors.New("unused") }

type info struct{ mode fs.FileMode }

func (i info) Name() string       { return "" }
func (i info) Size() int64        { return 0 }
func (i info) Mode() fs.FileMode  { return i.mode }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.mode.IsDir() }
func (i info) Sys() any           { return nil }

type dirFile struct {
	ents []fs.DirEntry
	err  error // returned with the last chunk
	read *int  // counts entries handed out
}

func (d *dirFile) Read([]byte) (int, error)   { return 0, io.EOF }
func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Stat() (fs.FileInfo, error) { return info{fs.ModeDir}, nil }
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if len(d.ents) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.ents))
	out := d.ents[:n]
	d.ents = d.ents[n:]
	*d.read += n
	if len(d.ents) == 0 {
		return out, d.err
	}
	return out, nil
}

// fake is a file system of directories (by cleaned absolute path) and
// symbolic link targets.
type fake struct {
	dirs  map[string][]fs.DirEntry
	links map[string]fs.FileMode // path -> mode of the target
	err   error                  // returned with the last chunk
	read  int
}

func (f *fake) OpenDir(name string) (fs.ReadDirFile, error) {
	e, ok := f.dirs[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &dirFile{ents: slices.Clone(e), err: f.err, read: &f.read}, nil
}

func (f *fake) Stat(name string) (fs.FileInfo, error) {
	m, ok := f.links[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return info{m}, nil
}

func files(names ...string) []fs.DirEntry {
	var out []fs.DirEntry
	for _, n := range names {
		out = append(out, ent{n, 0})
	}
	return out
}

func newFake() *fake {
	return &fake{
		dirs: map[string][]fs.DirEntry{
			"/bin": append(files("ls", "echo.wasm", "cat.wasm", "cat", ".hidden", ".wasm"),
				ent{"sub", fs.ModeDir}, ent{"lnk", fs.ModeSymlink}, ent{"ldir", fs.ModeSymlink}, ent{"dangling", fs.ModeSymlink}),
			"/usr/bin": files("ecomm", "ls"),
			"/home": append(files("a.txt", "my file", ".dot", ".dotfile", "b\x01c"),
				ent{"docs", fs.ModeDir}, ent{"linkdir", fs.ModeSymlink}, ent{"linkfile", fs.ModeSymlink}, ent{"broken", fs.ModeSymlink}),
			"/home/docs": files("x1", "x2", "y"),
			"/home/one":  files("only"),
			"/home/d1":   {ent{"sub", fs.ModeDir}},
			"/":          {ent{"etc", fs.ModeDir}},
		},
		links: map[string]fs.FileMode{
			"/bin/lnk": 0, "/bin/ldir": fs.ModeDir,
			"/home/linkdir": fs.ModeDir, "/home/linkfile": 0,
		},
	}
}

func completer(f *fake) *Completer {
	return &Completer{
		FS:    f,
		Dir:   func() string { return "/home" },
		Path:  func() string { return "/bin:/nope:/usr/bin:" },
		Funcs: func() []string { return []string{"myfunc", "echo"} },
	}
}

func values(cs []lineedit.Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Value)
	}
	return out
}

func TestComplete(t *testing.T) {
	for _, c := range []struct {
		name    string
		words   []string
		current string
		want    []string
		noSpace bool
	}{
		{"builtin and program", nil, "ec", []string{"echo", "ecomm"}, false},
		{"unique program wasm", nil, "cat", []string{"cat"}, false},
		{"function", nil, "myf", []string{"myfunc"}, false},
		{"reserved word", nil, "whi", []string{"while"}, false},
		{"symlink to file is a program", nil, "ln", []string{"lnk"}, false},
		{"symlink to dir is not", nil, "ld", nil, false},
		{"dangling link is not", nil, "dang", nil, false},
		{"directory is not a program", nil, "sub", nil, false},
		{"hidden program needs a dot", nil, ".h", []string{".hidden"}, false},
		{"after pipe", []string{"ls", "|"}, "cat", []string{"cat"}, false},
		{"after then", []string{"if", "true", ";", "then"}, "ec", []string{"echo", "ecomm"}, false},
		{"program with a slash is a file", nil, "./a", []string{"./a.txt"}, false},
		{"file", []string{"ls"}, "a", []string{"a.txt"}, false},
		{"file with space", []string{"ls"}, "my", []string{"my file"}, false},
		{"directory", []string{"ls"}, "doc", []string{"docs/"}, true},
		{"directories are 2 names", []string{"ls"}, "l", []string{"linkdir/", "linkfile"}, false},
		{"into a directory", []string{"ls"}, "docs/x", []string{"docs/x1", "docs/x2"}, false},
		{"absolute", []string{"ls"}, "/e", []string{"/etc/"}, true},
		{"parent", []string{"ls"}, "../et", []string{"../etc/"}, true},
		{"empty prefix hides dotfiles", []string{"ls"}, "docs/", []string{"docs/x1", "docs/x2", "docs/y"}, false},
		{"empty prefix in the working directory", []string{"ls"}, "", []string{"a.txt", "b\x01c", "broken", "docs/", "linkdir/", "linkfile", "my file"}, false},
		{"dot prefix shows them", []string{"ls"}, ".d", []string{".dot", ".dotfile"}, false},
		{"unreadable directory", []string{"ls"}, "nope/", nil, false},
		{"control character dropped by the editor, not here", []string{"ls"}, "b", []string{"b\x01c", "broken"}, false},
		{"unique file takes a space", []string{"ls"}, "a.t", []string{"a.txt"}, false},
		{"broken link is a file", []string{"ls"}, "bro", []string{"broken"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, noSpace := completer(newFake()).Complete(context.Background(), c.words, c.current)
			if v := values(got); !slices.Equal(v, c.want) || noSpace != c.noSpace {
				t.Errorf("%q after %q: %q noSpace=%v, want %q noSpace=%v", c.current, c.words, v, noSpace, c.want, c.noSpace)
			}
		})
	}
}

func TestEmptyPrefixCommands(t *testing.T) {
	got, _ := completer(newFake()).Complete(context.Background(), nil, "")
	v := values(got)
	for _, w := range []string{"cd", "echo", "ls", "cat", "myfunc", "if", "[", ":"} {
		if !slices.Contains(v, w) {
			t.Errorf("%q missing from %q", w, v)
		}
	}
	for _, w := range []string{".hidden", ".wasm", ""} {
		if slices.Contains(v, w) {
			t.Errorf("%q offered", w)
		}
	}
	if !slices.IsSorted(v) || len(slices.Compact(slices.Clone(v))) != len(v) {
		t.Error("candidates are not sorted and unique")
	}
}

func TestNilFuncsAndEmptyPathElement(t *testing.T) {
	f := newFake()
	f.dirs["/home"] = files("zz")
	c := &Completer{FS: f, Dir: func() string { return "/home" }, Path: func() string { return ":" }}
	got, _ := c.Complete(context.Background(), nil, "z")
	if v := values(got); !slices.Equal(v, []string{"zz"}) {
		t.Errorf("empty PATH element is the working directory: %q", v)
	}
}

func TestBounds(t *testing.T) {
	// A huge directory: only MaxEntries are read, in chunks.
	f := &fake{dirs: map[string][]fs.DirEntry{"/home": nil}}
	for i := range 10000 {
		f.dirs["/home"] = append(f.dirs["/home"], ent{"f" + strconv.Itoa(i), 0})
	}
	c := &Completer{FS: f, Dir: func() string { return "/home" }, Path: func() string { return "" }, MaxEntries: 1000, MaxCandidates: 5000}
	got, _ := c.Complete(context.Background(), []string{"ls"}, "f")
	if f.read != 1000 || len(got) != 1000 {
		t.Errorf("read %d entries, %d candidates; want 1000 and 1000", f.read, len(got))
	}

	// MaxCandidates stops the scan.
	f.read = 0
	c.MaxEntries, c.MaxCandidates = 0, 300
	got, _ = c.Complete(context.Background(), []string{"ls"}, "f")
	if f.read != 512 || len(got) != 300 {
		t.Errorf("read %d entries, %d candidates; want 512 and 300", f.read, len(got))
	}

	// Defaults.
	f.read = 0
	c.MaxCandidates = 0
	if got, _ = c.Complete(context.Background(), []string{"ls"}, "f"); f.read != 1024 || len(got) != DefaultMaxCandidates {
		t.Errorf("default: read %d, %d candidates", f.read, len(got))
	}

	// A name that is too long is skipped.
	long := make([]byte, MaxName+1)
	for i := range long {
		long[i] = 'a'
	}
	f.dirs["/home"] = files(string(long), "ok", "")
	got, _ = c.Complete(context.Background(), []string{"ls"}, "")
	if v := values(got); !slices.Equal(v, []string{"ok"}) {
		t.Errorf("long and empty names: %q", v)
	}

	// MaxDirs PATH directories at most.
	f = &fake{dirs: map[string][]fs.DirEntry{}}
	p := ""
	for i := range MaxDirs + 5 {
		d := "/d" + strconv.Itoa(i)
		f.dirs[d] = files("zq" + strconv.Itoa(i))
		p += d + ":"
	}
	c = &Completer{FS: f, Dir: func() string { return "/" }, Path: func() string { return p }}
	got, _ = c.Complete(context.Background(), nil, "zq")
	if len(got) != MaxDirs {
		t.Errorf("%d PATH directories visited, want %d", len(got), MaxDirs)
	}

	// Enough candidates from the builtins and earlier directories stop the PATH walk.
	f = &fake{dirs: map[string][]fs.DirEntry{"/a": files("q1", "q2", "q3"), "/b": files("q4")}}
	c = &Completer{FS: f, Dir: func() string { return "/" }, Path: func() string { return "/a:/b" }, MaxCandidates: 2}
	got, _ = c.Complete(context.Background(), nil, "q")
	if v := values(got); !slices.Equal(v, []string{"q1", "q2"}) {
		t.Errorf("candidate cap: %q", v)
	}
}

func TestContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _ := completer(newFake()).Complete(ctx, []string{"ls"}, "a")
	if len(got) != 0 {
		t.Errorf("cancelled: %q", values(got))
	}
	got, _ = completer(newFake()).Complete(ctx, nil, "ec")
	if v := values(got); !slices.Equal(v, []string{"echo"}) {
		t.Errorf("cancelled, only the builtins: %q", v)
	}
}

func TestReadError(t *testing.T) {
	f := newFake()
	f.err = errors.New("EIO")
	got, _ := completer(f).Complete(context.Background(), []string{"ls"}, "a")
	if v := values(got); !slices.Equal(v, []string{"a.txt"}) {
		t.Errorf("entries read before an error are kept: %q", v)
	}
}

// TestInEditor drives the completer through the line editor: the quoting
// and the trailing slash or space are the editor's, on this completer's
// candidates.
func TestInEditor(t *testing.T) {
	for _, c := range []struct{ typed, want string }{
		{"cat my\t", `cat my\ file `},
		{"cat 'my\t", `cat 'my file' `},
		{"ls doc\t", "ls docs/"},
		{"ls docs/x\t", "ls docs/x"},
		{"ls /e\t", "ls /etc/"},
		{"ecom\t", "ecomm "},
		{"ec\t", "ec"},
	} {
		ed := lineedit.New(lineedit.Options{Complete: completer(newFake()).Complete})
		var st lineedit.State
		for _, k := range lineedit.ParseKeys(c.typed) {
			ed.Key(context.Background(), k)
			st = ed.State()
		}
		if st.Line != c.want {
			t.Errorf("%q: line %q, want %q", c.typed, st.Line, c.want)
		}
	}
}

func TestBuiltinsMatchTheInterpreter(t *testing.T) {
	for _, n := range Builtins {
		if !interp.IsBuiltin(n) {
			t.Errorf("%q is not a builtin of the interpreter", n)
		}
	}
	// And the other way, over the names a shell documents.
	known := 0
	for _, n := range []string{"alias", "bg", "cd", "command", "false", "fc", "fg", "getopts", "hash", "jobs", "kill", "newgrp", "pwd", "read", "true", "umask", "unalias", "wait",
		"break", ":", "continue", ".", "eval", "exec", "exit", "export", "readonly", "return", "set", "shift", "times", "trap", "unset", "source", "bind", "builtin", "caller", "compgen",
		"complete", "compopt", "declare", "typeset", "dirs", "disown", "enable", "history", "help", "let", "local", "logout", "mapfile", "readarray", "popd", "pushd", "shopt",
		"suspend", "type", "ulimit", "echo", "printf", "test", "["} {
		if interp.IsBuiltin(n) {
			known++
		}
	}
	if known != len(Builtins) || len(slices.Compact(slices.Sorted(slices.Values(Builtins)))) != len(Builtins) {
		t.Errorf("Builtins has %d names (unique: no?), the interpreter %d", len(Builtins), known)
	}
}

func TestOSFS(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file1"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(dir, "lnk")); err != nil {
		t.Skip("no symbolic links:", err)
	}
	c := &Completer{FS: OSFS{}, Dir: func() string { return dir }, Path: func() string { return dir }}
	got, _ := c.Complete(context.Background(), []string{"ls"}, "")
	if v := values(got); !slices.Equal(v, []string{"file1", "lnk/", "sub/"}) {
		t.Errorf("files: %q", v)
	}
	got, _ = c.Complete(context.Background(), nil, "file")
	if v := values(got); !slices.Equal(v, []string{"file1"}) {
		t.Errorf("programs: %q", v)
	}
	if _, err := (OSFS{}).OpenDir(filepath.Join(dir, "none")); err == nil {
		t.Error("OpenDir of a missing directory succeeded")
	}
	if _, err := (OSFS{}).Stat(filepath.Join(dir, "none")); err == nil {
		t.Error("Stat of a missing file succeeded")
	}
}
