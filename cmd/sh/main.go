// Command sh is a POSIX shell for WASI containers. It is mvdan.cc/sh's
// interpreter with external commands started through the WASIX process
// subset (proc_spawn3, proc_join, fd_pipe); build it with
// GOOS=wasip1 GOARCH=wasm.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/go-wasmcontainers/shell/internal/complete"
	"github.com/go-wasmcontainers/shell/internal/run"
	"github.com/go-wasmcontainers/shell/internal/shell"
	"github.com/spf13/cobra"
)

// version is set by the release build (-ldflags -X main.version=...).
var version = "devel"

var osExit = os.Exit

// RootCmd is the sh command line. Operands after the first non-option
// argument belong to the script, as POSIX requires.
func RootCmd() *cobra.Command {
	var (
		cmdFlag, stdinFlag, interFlag, showVersion  bool
		errexit, xtrace, nounset, noglob, allexport bool
	)
	cmd := &cobra.Command{
		Use:   "sh [-eux] [-c command_string [command_name [argument...]] | file [argument...] | -s [argument...]]",
		Short: "A POSIX shell for WASI containers",
		Long: "sh runs POSIX shell scripts inside a WASI container. External commands\n" +
			"are started through the WASIX process subset (proc_spawn3, proc_join,\n" +
			"fd_pipe) and looked up on PATH as <dir>/<name> or <dir>/<name>.wasm.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Fprintln(cmd.OutOrStdout(), "sh (go-wasmcontainers/shell)", version)
				return nil
			}
			var set []string
			for f, on := range map[string]bool{"-e": errexit, "-x": xtrace, "-u": nounset, "-f": noglob, "-a": allexport} {
				if on {
					set = append(set, f)
				}
			}
			dir, err := os.Getwd()
			if err != nil {
				dir = "/"
			}
			cfg := shell.Config{
				Stdin:   cmd.InOrStdin(),
				Stdout:  cmd.OutOrStdout(),
				Stderr:  cmd.ErrOrStderr(),
				Env:     os.Environ(),
				Dir:     dir,
				FS:      run.OSFS{},
				Spawner: run.WASIX{},
				// Redirection fds are tracked so that children do not
				// inherit them (WASIX children inherit every open fd).
				Open: openFile,
				// Tab completion reads directories through the process's FS.
				DirFS: complete.OSFS{},
			}
			if os.Getenv("SH_TRACE_SPAWN") != "" {
				cfg.Trace = cmd.ErrOrStderr()
			}
			if t, err := ttyGet(); err == nil {
				cfg.IsTTY = t.StdinTTY
				cfg.Terminal = wasixTerm{}
			}
			code := shell.New(cfg).Main(context.Background(), shell.Options{
				Command: cmdFlag, Set: set, Interactive: interFlag, ReadStdin: stdinFlag,
			}, args)
			if code != 0 {
				osExit(code)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.SetInterspersed(false)
	f.BoolVarP(&cmdFlag, "command", "c", false, "read commands from the first operand")
	f.BoolVarP(&stdinFlag, "stdin", "s", false, "read commands from standard input")
	f.BoolVarP(&interFlag, "interactive", "i", false, "interactive shell (prompts, aliases)")
	f.BoolVarP(&errexit, "errexit", "e", false, "exit when a command fails (set -e)")
	f.BoolVarP(&xtrace, "xtrace", "x", false, "trace commands (set -x)")
	f.BoolVarP(&nounset, "nounset", "u", false, "unset variables are an error (set -u)")
	f.BoolVarP(&noglob, "noglob", "f", false, "disable pathname expansion (set -f)")
	f.BoolVarP(&allexport, "allexport", "a", false, "export every assigned variable (set -a)")
	f.BoolVar(&showVersion, "version", false, "print the version and exit")
	return cmd
}

func main() {
	setupPipes()
	if err := RootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sh:", err)
		osExit(2)
	}
}
