// Command execprobe exercises wasix/exec from inside a container: each
// check prints one line, "ok <name>" or "FAIL <name>: why". Built for
// GOOS=wasip1.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-wasmcontainers/shell/wasix/exec"
)

func check(name string, err error) {
	if err != nil {
		fmt.Printf("FAIL %s: %v\n", name, err)
		return
	}
	fmt.Printf("ok %s\n", name)
}

func main() {
	// LookPath: PATH, and the <name>.wasm rule.
	check("lookpath", func() error {
		p, err := exec.LookPath("cat")
		if err != nil || !strings.HasPrefix(p, "/bin/cat") {
			return fmt.Errorf("%q %v", p, err)
		}
		_, err = exec.LookPath("no-such-tool")
		if !errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("missing: %v", err)
		}
		return nil
	}())
	// Output with stdin from a reader.
	check("output", func() error {
		c := exec.Command("tr", "a-z", "A-Z")
		c.Stdin = strings.NewReader("hello\n")
		out, err := c.Output()
		if err != nil || string(out) != "HELLO\n" {
			return fmt.Errorf("%q %v", out, err)
		}
		return nil
	}())
	// The exit status and the captured stderr of a failure.
	check("exiterror", func() error {
		_, err := exec.Command("ls", "/no/such/file").Output()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() == 0 || !strings.Contains(string(ee.Stderr), "no/such/file") {
			return fmt.Errorf("%v", err)
		}
		return nil
	}())
	// Pipes both ways, and a large stream.
	check("pipes", func() error {
		c := exec.Command("wc", "-c")
		w, err := c.StdinPipe()
		if err != nil {
			return err
		}
		r, err := c.StdoutPipe()
		if err != nil {
			return err
		}
		if err := c.Start(); err != nil {
			return err
		}
		go func() {
			io.WriteString(w, strings.Repeat("x", 1<<20))
			w.Close()
		}()
		b, _ := io.ReadAll(r)
		if err := c.Wait(); err != nil || strings.TrimSpace(string(b)) != "1048576" {
			return fmt.Errorf("%q %v", b, err)
		}
		return nil
	}())
	// Dir and Env reach the child.
	check("dir-env", func() error {
		c := exec.Command("sh", "-c", "pwd; echo $PROBE")
		c.Dir = "/etc"
		c.Env = append(os.Environ(), "PROBE=yes")
		out, err := c.Output()
		if err != nil || string(out) != "/etc\nyes\n" {
			return fmt.Errorf("%q %v", out, err)
		}
		return nil
	}())
	// Stdout and Stderr into one writer.
	check("combined", func() error {
		out, err := exec.Command("sh", "-c", "echo out; echo err >&2").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "out\n") || !strings.Contains(string(out), "err\n") {
			return fmt.Errorf("%q %v", out, err)
		}
		return nil
	}())
	// Our own stdout as the child's: handed over as a file.
	check("file-stdout", func() error {
		c := exec.Command("echo", "direct")
		c.Stdout = os.Stdout
		return c.Run()
	}())
}
