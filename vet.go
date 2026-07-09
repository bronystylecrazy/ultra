package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// analyzerModule is where the real analyzer lives — its own module, so
// the kernel keeps zero dependencies; `ultra vet` is a thin delegator.
const analyzerModule = "github.com/bronystylecrazy/ultrastack/analyzer/cmd/ultravet"

// cmdVet runs the static analyzer over the current directory.
func cmdVet(args []string, out, errW io.Writer) int {
	return runVet("", args, out, errW)
}

// runVet delegates to the analyzer: an installed ultravet binary if
// present, else `go run <module>@latest` (which needs GOPRIVATE + git
// auth while the repo is private). dir, when set, is the working directory
// (the mcp `vet` tool analyzes a named product); empty runs in place.
func runVet(dir string, args []string, out, errW io.Writer) int {
	if len(args) == 0 {
		args = []string{"./..."}
	}

	run := func(name string, argv ...string) int {
		cmd := exec.Command(name, argv...)
		cmd.Dir = dir
		cmd.Stdout = out
		cmd.Stderr = errW
		cmd.Stdin = os.Stdin
		cmd.Env = append(os.Environ(), "GOPRIVATE=github.com/bronystylecrazy/*")
		if err := cmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			return -1 // could not start at all
		}
		return 0
	}

	if path, err := exec.LookPath("ultravet"); err == nil {
		return run(path, args...)
	}
	if code := run("go", append([]string{"run", analyzerModule + "@latest"}, args...)...); code >= 0 {
		return code
	}
	fmt.Fprintf(errW, `ultra vet: the analyzer is not installed and could not be fetched.

  GOPRIVATE=github.com/bronystylecrazy/* go install %s@latest

then re-run: ultra vet ./...
`, analyzerModule)
	return 1
}
