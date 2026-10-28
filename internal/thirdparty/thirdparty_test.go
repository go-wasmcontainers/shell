// Package thirdparty checks that the patched copy of mvdan.cc/sh under
// third_party is the version go.mod says it is.
package thirdparty

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

func TestPatchedCopyMatchesGoMod(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mf, err := modfile.Parse("go.mod", b, nil)
	if err != nil {
		t.Fatal(err)
	}
	var required string
	for _, r := range mf.Require {
		if r.Mod.Path == "mvdan.cc/sh/v3" {
			required = r.Mod.Version
		}
	}
	replaced := false
	for _, r := range mf.Replace {
		if r.Old.Path == "mvdan.cc/sh/v3" && r.New.Path == "./third_party/mvdan-sh" {
			replaced = true
		}
	}
	if required == "" || !replaced {
		t.Fatalf("go.mod: require %q, replaced by third_party: %v", required, replaced)
	}
	p, err := os.ReadFile("../../third_party/mvdan-sh/PATCHED")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(p), "\n")
	if first != "upstream: mvdan.cc/sh/v3 "+required+" (BSD-3-Clause, see LICENSE in this directory)" {
		t.Errorf("third_party/mvdan-sh/PATCHED says %q but go.mod requires %s: re-copy and re-patch", first, required)
	}
	src, err := os.ReadFile("../../third_party/mvdan-sh/interp/stdin_os.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "func SetPipeFunc(") {
		t.Error("the SetPipeFunc patch is missing from interp/stdin_os.go")
	}
}
