module github.com/go-wasmcontainers/shell

go 1.27.3

require (
	github.com/go-wasmcontainers/engine v0.29.2
	github.com/go-wasmcontainers/shell/wasix v0.1.0
	github.com/spf13/cobra v1.10.2
	github.com/wasmdesk/coreutils v0.5.1-0.20261031200158-fe13df492e30
	golang.org/x/mod v0.42.0
	mvdan.cc/sh/v3 v3.14.1
)

require (
	github.com/go-regexp/engine v0.3.2 // indirect
	github.com/go-ruby-regexp/regexp v0.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/term v0.47.0 // indirect
)

// mvdan.cc/sh/v3 v3.14.1 with one patch (interp.SetPipeFunc) so that the
// interpreter can create pipes on GOOS=wasip1, where os.Pipe is ENOSYS.
// See third_party/mvdan-sh/PATCHED and docs/REFERENCES.md.
replace mvdan.cc/sh/v3 => ./third_party/mvdan-sh

replace github.com/go-wasmcontainers/shell/wasix => ./wasix
