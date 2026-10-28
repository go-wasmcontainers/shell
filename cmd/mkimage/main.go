// Command mkimage builds the shell base image (wasip1/wasm): /bin/sh, the
// coreutils multicall module and its links, a minimal /etc. It writes an
// OCI image layout, optionally the unpacked root filesystem, and can push
// the image to a registry.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wasmdesk/coreutils/multicall"

	"github.com/go-wasmcontainers/shell/internal/base"
	"github.com/go-wasmcontainers/shell/internal/oci"
)

var osExit = os.Exit

// RootCmd is the mkimage command line.
func RootCmd() *cobra.Command {
	var (
		shPath, cuPath, version, layoutDir, rootfsDir, created, push, username string
		tags                                                                   []string
		passwordStdin                                                          bool
	)
	cmd := &cobra.Command{
		Use:           "mkimage --sh sh.wasm --coreutils coreutils.wasm --version V [--layout DIR] [--rootfs DIR] [--push REGISTRY/REPO --tag T...]",
		Short:         "Build the shell base image for WASI (wasip1) containers",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if shPath == "" || cuPath == "" || version == "" {
				return errors.New("--sh, --coreutils and --version are required")
			}
			if layoutDir == "" && rootfsDir == "" && push == "" {
				return errors.New("nothing to do: give --layout, --rootfs or --push")
			}
			mtime, err := time.Parse(time.RFC3339, created)
			if err != nil {
				return fmt.Errorf("--created: %w", err)
			}
			sh, err := os.ReadFile(shPath)
			if err != nil {
				return err
			}
			cu, err := os.ReadFile(cuPath)
			if err != nil {
				return err
			}
			entries, err := base.Entries(sh, cu, multicall.Names(), version)
			if err != nil {
				return err
			}
			if rootfsDir != "" {
				if err := writeRootfs(rootfsDir, entries); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "rootfs:", rootfsDir)
			}
			layer, diffID, err := oci.Layer(entries, mtime)
			if err != nil {
				return err
			}
			img, err := oci.Build(oci.Spec{Layer: layer, DiffID: diffID, Exec: base.Exec(version), Created: mtime,
				Annotations: map[string]string{
					"org.opencontainers.image.source":  "https://github.com/go-wasmcontainers/shell",
					"org.opencontainers.image.version": version,
				}})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "manifest: %s (layer %d bytes, %d entries)\n", img.Desc.Digest, len(layer), len(entries))
			if layoutDir != "" {
				if err := oci.WriteLayout(layoutDir, img, version); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "layout:", layoutDir)
			}
			if push == "" {
				return nil
			}
			host, repo, ok := strings.Cut(push, "/")
			if !ok || !strings.Contains(host, ".") && !strings.Contains(host, ":") {
				return fmt.Errorf("--push %q must be REGISTRY/REPOSITORY", push)
			}
			var password string
			if passwordStdin {
				// One line, the way `docker login --password-stdin` reads it.
				line, err := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 4096)).ReadString('\n')
				if err != nil && err != io.EOF {
					return err
				}
				password = strings.TrimRight(line, "\r\n")
			}
			scheme := "https://"
			if strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.0.0.1:") {
				scheme = "http://"
			}
			d, err := oci.Push(cmd.Context(), oci.PushOptions{Base: scheme + host, Repo: repo, Tags: tags,
				Username: username, Password: password}, img)
			if err != nil {
				return err
			}
			for _, t := range tags {
				fmt.Fprintf(cmd.OutOrStdout(), "pushed %s:%s@%s\n", push, t, d)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&shPath, "sh", "", "the sh module (GOOS=wasip1)")
	f.StringVar(&cuPath, "coreutils", "", "the coreutils multicall module (GOOS=wasip1)")
	f.StringVar(&version, "version", "", "the image version (also its ref name in the layout)")
	f.StringVar(&layoutDir, "layout", "", "write an OCI image layout here")
	f.StringVar(&rootfsDir, "rootfs", "", "write the unpacked root filesystem here (for tests)")
	f.StringVar(&created, "created", "1970-01-01T00:00:00Z", "creation time and every file's mtime (RFC 3339), for reproducible builds")
	f.StringVar(&push, "push", "", "push to REGISTRY/REPOSITORY (e.g. ghcr.io/go-wasmcontainers/shell)")
	f.StringArrayVarP(&tags, "tag", "t", nil, "tag to push (repeatable)")
	f.StringVarP(&username, "username", "u", "", "registry user name")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the registry password or token from standard input")
	return cmd
}

// writeRootfs unpacks entries under dir (which must not exist yet).
func writeRootfs(dir string, es []oci.Entry) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	for _, e := range es {
		p := filepath.Join(dir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		switch {
		case e.Dir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case e.Link != "":
			if err := os.Symlink(e.Link, p); err != nil {
				return err
			}
		default:
			if err := os.WriteFile(p, e.Data, os.FileMode(e.Mode).Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

func main() {
	if err := RootCmd().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "mkimage:", err)
		osExit(1)
	}
}
