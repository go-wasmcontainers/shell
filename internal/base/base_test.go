package base

import (
	"strings"
	"testing"
)

var mod = []byte("\x00asm\x01\x00\x00\x00")

func TestEntries(t *testing.T) {
	es, err := Entries(mod, mod, []string{"cat", "ls", "cat"}, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]string{}
	files := map[string]bool{}
	for _, e := range es {
		if e.Link != "" {
			links[e.Path] = e.Link
		} else if !e.Dir {
			files[e.Path] = true
		}
	}
	if len(links) != 2 || links["bin/cat"] != Coreutils || links["bin/ls"] != Coreutils {
		t.Errorf("links: %v", links)
	}
	for _, f := range []string{"bin/sh", "bin/" + Coreutils, "etc/passwd", "etc/group", "etc/os-release"} {
		if !files[f] {
			t.Errorf("missing %s", f)
		}
	}
	for _, bad := range []string{"sh", Coreutils, "a/b", "", "..", "x\x00"} {
		if _, err := Entries(mod, mod, []string{bad}, "v"); err == nil {
			t.Errorf("tool %q accepted", bad)
		}
	}
	if _, err := Entries([]byte("#!/bin/sh"), mod, nil, "v"); err == nil {
		t.Error("non-wasm sh accepted")
	}
}

func TestExec(t *testing.T) {
	e := Exec("0.1.0")
	if strings.Join(e.Shell, " ") != "/bin/sh -c" || e.Env[0] != "PATH=/bin" || e.Cmd[0] != "/bin/sh" ||
		e.Labels["org.opencontainers.image.version"] != "0.1.0" {
		t.Errorf("%+v", e)
	}
}
