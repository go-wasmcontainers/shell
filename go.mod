module github.com/go-wasmcontainers/shell

go 1.27.3

require (
	github.com/spf13/cobra v1.10.2
	golang.org/x/mod v0.42.0
	mvdan.cc/sh/v3 v3.14.1
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
)

// mvdan.cc/sh/v3 v3.14.1 with one patch (interp.SetPipeFunc) so that the
// interpreter can create pipes on GOOS=wasip1, where os.Pipe is ENOSYS.
// See third_party/mvdan-sh/PATCHED and docs/REFERENCES.md.
replace mvdan.cc/sh/v3 => ./third_party/mvdan-sh
