// Command tbox is a tiny multicall test program for the end-to-end
// tests: argv[0] picks the tool. It is built for GOOS=wasip1.
package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	// wasmer 7.5.0 answers fd_fdstat_get for fds 0-2 with hard-coded
	// flags (lib/wasix/src/fs/mod.rs, WasiFs::fdstat), so after a dup2 of
	// a pipe onto fd 0 Go's runtime cannot see the NONBLOCK it set in
	// syscall's init, treats stdin as blocking, and surfaces EAGAIN
	// ("Try again") instead of parking on the poller. Put stdin back in
	// blocking mode so that the tests exercise the shell, not that bug.
	// The engine's host answers fdstat from the fd table.
	syscall.SetNonblock(0, false)
	name := strings.TrimSuffix(path.Base(os.Args[0]), ".wasm")
	args := os.Args[1:]
	switch name {
	case "cat":
		if len(args) == 0 {
			args = []string{"-"}
		}
		for _, a := range args {
			var r io.Reader = os.Stdin
			if a != "-" {
				f, err := os.Open(a)
				if err != nil {
					fmt.Fprintln(os.Stderr, "cat:", err)
					os.Exit(1)
				}
				defer f.Close()
				r = f
			}
			if _, err := io.Copy(os.Stdout, r); err != nil {
				fmt.Fprintln(os.Stderr, "cat:", err)
				os.Exit(1)
			}
		}
	case "echo":
		fmt.Println(strings.Join(args, " "))
	case "argv":
		for _, a := range args {
			fmt.Printf("[%s]", a)
		}
		fmt.Println()
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
	case "env":
		e := os.Environ()
		sort.Strings(e)
		for _, kv := range e {
			fmt.Println(kv)
		}
	case "fail":
		n := 1
		if len(args) > 0 {
			n, _ = strconv.Atoi(args[0])
		}
		os.Exit(n)
	case "wc":
		b, _ := io.ReadAll(os.Stdin)
		fmt.Println(strings.Count(string(b), "\n"))
	default:
		fmt.Fprintf(os.Stderr, "tbox: unknown tool %q\n", name)
		os.Exit(2)
	}
}
