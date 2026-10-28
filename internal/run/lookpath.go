// Package run is the part of the shell that runs external programs: it
// finds them on PATH, decides how to start them (WebAssembly module,
// #! script or plain shell script) and hands them to a [Spawner].
package run

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// FS is the filesystem view the lookups use; [OSFS] in the real shell.
type FS interface {
	Stat(name string) (fs.FileInfo, error)
	// Head returns up to n bytes from the start of the file.
	Head(name string, n int) ([]byte, error)
}

// OSFS is the [FS] of the running process.
type OSFS struct{}

// Stat follows symbolic links, like a kernel resolving a program path.
func (OSFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// Head reads up to n bytes of name.
func (OSFS) Head(name string, n int) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	m, err := io.ReadFull(f, b)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return b[:m], err
}

// WasmExt is the suffix of program files in the pkgx bottle layout
// (bin/<name>.wasm, provided as bin/<name>).
const WasmExt = ".wasm"

// ErrNotFound is returned when no program of that name is on PATH.
var ErrNotFound = errors.New("not found")

// Abs makes name absolute against dir and cleans it.
func Abs(dir, name string) string {
	if !path.IsAbs(name) {
		name = path.Join(dir, name)
	}
	return path.Clean(name)
}

// LookPath resolves a command name the way POSIX sh does (XCU 2.9.1.4):
// a name with a slash is used as is (relative to dir); otherwise each
// PATH element is tried in order, an empty element meaning dir. For
// each candidate <d>/<name> is tried, then <d>/<name>.wasm. Only
// regular files (after following symbolic links) match.
//
// The paths are resolved by the WASI host inside the container's root
// filesystem: a guest cannot name a host path.
func LookPath(fsys FS, dir, pathEnv, name string) (string, error) {
	if name == "" {
		return "", ErrNotFound
	}
	try := func(p string) (string, bool) {
		for _, c := range []string{p, p + WasmExt} {
			if strings.HasSuffix(p, WasmExt) && c != p {
				break
			}
			fi, err := fsys.Stat(c)
			if err == nil && fi.Mode().IsRegular() {
				return c, true
			}
		}
		return "", false
	}
	if strings.Contains(name, "/") {
		if p, ok := try(Abs(dir, name)); ok {
			return p, nil
		}
		return "", ErrNotFound
	}
	for _, d := range strings.Split(pathEnv, ":") {
		if d == "" || d == "." {
			d = dir
		}
		if p, ok := try(Abs(dir, path.Join(d, name))); ok {
			return p, nil
		}
	}
	return "", ErrNotFound
}

// Kind is how a program file is started.
type Kind int

// The kinds of program files.
const (
	// KindWasm is a WebAssembly module (starts with \0asm).
	KindWasm Kind = iota
	// KindShebang is a #! script, run by the interpreter it names.
	KindShebang
	// KindScript is any other text file: POSIX says a shell runs it
	// as a shell script (XCU 2.9.1.6, ENOEXEC).
	KindScript
	// KindBinary is a file that is neither: refused with status 126.
	KindBinary
)

// MaxShebang bounds the #! line that is read.
const MaxShebang = 256

// Classify reads the start of the file and says how to run it; for a
// #! script it also returns the interpreter and its optional argument.
func Classify(fsys FS, name string) (Kind, string, string, error) {
	head, err := fsys.Head(name, MaxShebang)
	if err != nil {
		return 0, "", "", err
	}
	if bytes.HasPrefix(head, []byte("\x00asm")) {
		return KindWasm, "", "", nil
	}
	if bytes.HasPrefix(head, []byte("#!")) {
		line, _, _ := bytes.Cut(head[2:], []byte("\n"))
		line = bytes.TrimRight(line, "\r")
		f := strings.Fields(string(line))
		if len(f) == 0 {
			return KindScript, "", "", nil
		}
		// Like Linux, everything after the interpreter is one argument.
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(line)), f[0]))
		return KindShebang, f[0], arg, nil
	}
	line, _, _ := bytes.Cut(head, []byte("\n"))
	if bytes.IndexByte(line, 0) >= 0 {
		return KindBinary, "", "", nil
	}
	return KindScript, "", "", nil
}
