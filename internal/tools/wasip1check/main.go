// Command wasip1check builds Go packages for GOOS=wasip1 GOARCH=wasm one
// at a time and reports, for each, PASS or FAIL with the first error line.
// It is how docs/USERLAND.md measured which candidate tools build.
//
//	go run ./internal/tools/wasip1check -C DIR PKG...
//
// Each package is built alone (go build stops reporting a package's
// dependents after a failure, so one build of many hides results).
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// Result is one package's outcome.
type Result struct {
	Pkg  string
	OK   bool
	Why  string // first error line when not OK
	Size int64  // binary bytes when OK
}

// builder runs `go build` for one package; replaced in tests.
type builder func(dir, pkg, out string) ([]byte, error)

func goBuild(dir, pkg, out string) ([]byte, error) {
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOWORK=off")
	return cmd.CombinedOutput()
}

// firstError is the first line of compiler output that says something:
// not a "# package" header, not empty.
func firstError(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "go: downloading ") {
			continue
		}
		return l
	}
	return "build failed without output"
}

func check(build builder, dir, tmp string, pkgs []string) []Result {
	var rs []Result
	for i, p := range pkgs {
		out := fmt.Sprintf("%s/bin%d.wasm", tmp, i)
		msg, err := build(dir, p, out)
		r := Result{Pkg: p, OK: err == nil}
		if err != nil {
			r.Why = firstError(msg)
		} else if st, err := os.Stat(out); err == nil {
			r.Size = st.Size()
			os.Remove(out)
		}
		rs = append(rs, r)
	}
	return rs
}

func report(w io.Writer, rs []Result) {
	for _, r := range rs {
		if r.OK {
			fmt.Fprintf(w, "PASS\t%s\t%.1f MB\n", r.Pkg, float64(r.Size)/1e6)
		} else {
			fmt.Fprintf(w, "FAIL\t%s\t%s\n", r.Pkg, r.Why)
		}
	}
}

// RootCmd is the command line.
func RootCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:          "wasip1check [-C DIR] PKG...",
		Short:        "Build each package for GOOS=wasip1 GOARCH=wasm and report PASS or FAIL",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, pkgs []string) error {
			tmp, err := os.MkdirTemp("", "wasip1check")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			report(cmd.OutOrStdout(), check(goBuild, dir, tmp, pkgs))
			return nil
		},
	}
	cmd.Flags().StringVarP(&dir, "dir", "C", ".", "module directory to build in")
	return cmd
}

func main() {
	if err := RootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
