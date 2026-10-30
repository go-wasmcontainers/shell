package exec

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
)

// WasmExt is the suffix of program files in the pkgx bottle layout
// (bin/<name>.wasm, provided as bin/<name>).
const WasmExt = ".wasm"

var getwd = os.Getwd // a seam for tests

// ErrNotFound is the error when no program of that name is on PATH.
var ErrNotFound = errors.New("executable file not found in $PATH")

// Error is returned by LookPath when it fails to classify a file as a
// program.
type Error struct {
	Name string
	Err  error
}

func (e *Error) Error() string { return "exec: " + e.Name + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// LookPath searches the directories of $PATH for file, as POSIX sh does
// (XCU 2.9.1.4), with the WASIX rule of this module: for each directory d,
// d/file then d/file.wasm. A name with a slash is used as is (relative
// names against the working directory). An empty PATH element is the
// working directory. Only regular files (after following symbolic links)
// match.
func LookPath(file string) (string, error) {
	dir, err := getwd()
	if err != nil {
		dir = "/"
	}
	p, err := Find(os.Stat, dir, os.Getenv("PATH"), file)
	if err != nil {
		return "", &Error{file, err}
	}
	return p, nil
}

// Find is LookPath's rule with its inputs explicit: stat (os.Stat, or a
// container filesystem's), the working directory, the PATH value and the
// name. It returns a cleaned absolute path.
func Find(stat func(string) (fs.FileInfo, error), dir, pathEnv, name string) (string, error) {
	if name == "" {
		return "", ErrNotFound
	}
	try := func(p string) (string, bool) {
		for _, c := range []string{p, p + WasmExt} {
			if strings.HasSuffix(p, WasmExt) && c != p {
				break
			}
			if fi, err := stat(c); err == nil && fi.Mode().IsRegular() {
				return c, true
			}
		}
		return "", false
	}
	if strings.Contains(name, "/") {
		if p, ok := try(abs(dir, name)); ok {
			return p, nil
		}
		return "", ErrNotFound
	}
	for _, d := range strings.Split(pathEnv, ":") {
		if d == "" || d == "." {
			d = dir
		}
		if p, ok := try(abs(dir, path.Join(d, name))); ok {
			return p, nil
		}
	}
	return "", ErrNotFound
}

func abs(dir, name string) string {
	if !path.IsAbs(name) {
		name = path.Join(dir, name)
	}
	return path.Clean(name)
}
