// Package complete is the Tab completion of the interactive shell, after
// bash's defaults (bash(1) "Programmable Completion" and "READLINE", the
// default, non-programmable completions):
//
//   - the first word of a command (and the word after | || & && ; or a
//     reserved word such as then) completes to the shell's builtins, its
//     functions, its reserved words and the programs on PATH;
//   - any other word, and a first word that contains a slash, completes to
//     file names relative to the working directory: a directory gets a
//     trailing "/" (and no space after a unique match), names that begin
//     with a dot are offered only when the typed prefix begins with one, and
//     the line editor quotes what needs it (a name with a space becomes
//     "my\ file").
//
// Not done, on purpose: ~ and $VAR expansion, alias names, and the
// programmable completion of bash (complete, compgen).
//
// Reading is bounded: at most [Completer.MaxEntries] directory entries are
// scanned per directory (in chunks, so a huge directory is never read at
// once), at most [MaxDirs] PATH directories are visited and at most
// [Completer.MaxCandidates] candidates are returned; a name longer than
// [MaxName] bytes is skipped.
package complete

import (
	"context"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/go-wasmcontainers/engine/lineedit"
)

// The bounds of one completion.
const (
	// DefaultMaxEntries is the directory entries scanned per directory.
	DefaultMaxEntries = 4096
	// DefaultMaxCandidates is the candidates returned.
	DefaultMaxCandidates = 1000
	// MaxDirs is the PATH directories visited.
	MaxDirs = 32
	// MaxName is the longest candidate name, in bytes.
	MaxName = 255
	// chunk is how many entries one read of a directory asks for.
	chunk = 256
)

// WasmExt is the suffix of program files in the pkgx bottle layout
// (bin/<name>.wasm, started as <name>), as run.WasmExt.
const WasmExt = ".wasm"

// FS is the file system the completion reads.
type FS interface {
	// OpenDir opens a directory for reading its entries a chunk at a time.
	OpenDir(name string) (fs.ReadDirFile, error)
	// Stat follows symbolic links.
	Stat(name string) (fs.FileInfo, error)
}

// OSFS is the [FS] of the running process.
type OSFS struct{}

// OpenDir opens name with os.Open.
func (OSFS) OpenDir(name string) (fs.ReadDirFile, error) { return os.Open(name) }

// Stat is os.Stat.
func (OSFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// Reserved are the reserved words of the shell language, offered at the
// start of a command.
var Reserved = []string{
	"!", "{", "}", "case", "do", "done", "elif", "else", "esac", "fi", "for",
	"function", "if", "in", "then", "time", "until", "while",
}

// Builtins are the builtins of the interpreter (mvdan.cc/sh interp's
// IsBuiltin); a test keeps the list true to it.
var Builtins = []string{
	"alias", "bg", "cd", "command", "false", "fc", "fg", "getopts", "hash",
	"jobs", "kill", "newgrp", "pwd", "read", "true", "umask", "unalias", "wait",
	"break", ":", "continue", ".", "eval", "exec", "exit", "export",
	"readonly", "return", "set", "shift", "times", "trap", "unset",
	"source", "bind", "builtin", "caller", "compgen", "complete", "compopt",
	"declare", "typeset", "dirs", "disown", "enable", "history", "help", "let",
	"local", "logout", "mapfile", "readarray", "popd", "pushd", "shopt",
	"suspend", "type", "ulimit", "echo", "printf", "test", "[",
}

// afterWord are the words after which a command starts again.
var afterWord = []string{"|", "||", "&", "&&", ";", ";;", "(", "{", "!", "then", "do", "else", "elif", "if", "while", "until", "time"}

// Completer completes the word being typed. The zero value of the bounds
// means their defaults.
type Completer struct {
	FS FS
	// Dir is the working directory and Path the PATH, read at each Tab.
	Dir, Path func() string
	// Funcs lists the names of the functions defined (nil: none).
	Funcs func() []string

	MaxEntries    int
	MaxCandidates int
}

// Complete is a [lineedit.Completer].
func (c *Completer) Complete(ctx context.Context, words []string, current string) ([]lineedit.Candidate, bool) {
	var names []string
	dirs := false
	if c.commandPosition(words) && !strings.Contains(current, "/") {
		names = c.commands(ctx, current)
	} else {
		names, dirs = c.files(ctx, current)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	cands := make([]lineedit.Candidate, len(names))
	for i, n := range names {
		cands[i] = lineedit.Candidate{Value: n}
	}
	// A unique directory is completed up to its slash and no further.
	return cands, dirs && len(cands) == 1
}

func (c *Completer) commandPosition(words []string) bool {
	return len(words) == 0 || slices.Contains(afterWord, words[len(words)-1])
}

func (c *Completer) maxEntries() int {
	if c.MaxEntries > 0 {
		return c.MaxEntries
	}
	return DefaultMaxEntries
}

func (c *Completer) maxCandidates() int {
	if c.MaxCandidates > 0 {
		return c.MaxCandidates
	}
	return DefaultMaxCandidates
}

// commands lists the builtins, reserved words, functions and programs on
// PATH that begin with prefix.
func (c *Completer) commands(ctx context.Context, prefix string) []string {
	var out []string
	add := func(n string) {
		if n != "" && strings.HasPrefix(n, prefix) && (prefix != "" || n[0] != '.') {
			out = append(out, n)
		}
	}
	for _, n := range Builtins {
		add(n)
	}
	for _, n := range Reserved {
		add(n)
	}
	if c.Funcs != nil {
		for _, n := range c.Funcs() {
			add(n)
		}
	}
	dir := c.Dir()
	seen := 0
	for _, d := range strings.Split(c.Path(), ":") {
		if seen == MaxDirs || len(out) >= c.maxCandidates() || ctx.Err() != nil {
			break
		}
		seen++
		if d == "" {
			d = "."
		}
		d = abs(dir, d)
		c.scan(ctx, d, func(e fs.DirEntry) {
			if c.regular(d, e) {
				add(strings.TrimSuffix(e.Name(), WasmExt))
			}
		}, prefix)
	}
	return out
}

// regular reports whether e, in directory d, is a regular file, following
// a symbolic link.
func (c *Completer) regular(d string, e fs.DirEntry) bool {
	if e.Type().IsRegular() {
		return true
	}
	if e.Type()&fs.ModeSymlink == 0 {
		return false
	}
	fi, err := c.FS.Stat(path.Join(d, e.Name()))
	return err == nil && fi.Mode().IsRegular()
}

// files lists the names in the directory of current that begin with its
// last element; dirs reports that a directory is among them.
func (c *Completer) files(ctx context.Context, current string) (names []string, dirs bool) {
	head, base := "", current
	if i := strings.LastIndexByte(current, '/'); i >= 0 {
		head, base = current[:i+1], current[i+1:]
	}
	dir := abs(c.Dir(), head) // head "" is the working directory
	c.scan(ctx, dir, func(e fs.DirEntry) {
		n := e.Name()
		if base == "" && strings.HasPrefix(n, ".") {
			return
		}
		isDir := e.IsDir()
		if !isDir && e.Type()&fs.ModeSymlink != 0 {
			fi, err := c.FS.Stat(path.Join(dir, n))
			isDir = err == nil && fi.IsDir()
		}
		if isDir {
			n += "/"
			dirs = true
		}
		names = append(names, head+n)
	}, base)
	return names, dirs
}

// scan calls fn for the entries of dir whose name begins with prefix,
// reading at most MaxEntries entries in chunks and stopping at
// MaxCandidates calls or when ctx ends.
func (c *Completer) scan(ctx context.Context, dir string, fn func(fs.DirEntry), prefix string) {
	d, err := c.FS.OpenDir(dir)
	if err != nil {
		return
	}
	defer d.Close()
	left, found := c.maxEntries(), 0
	for left > 0 && found < c.maxCandidates() && ctx.Err() == nil {
		ents, err := d.ReadDir(min(chunk, left))
		left -= len(ents)
		for _, e := range ents {
			n := e.Name()
			if found >= c.maxCandidates() {
				return
			}
			if n == "" || len(n) > MaxName || !strings.HasPrefix(n, prefix) {
				continue
			}
			found++
			fn(e)
		}
		if err != nil || len(ents) == 0 {
			return
		}
	}
}

func abs(dir, name string) string {
	if !path.IsAbs(name) {
		name = path.Join(dir, name)
	}
	return path.Clean(name)
}
