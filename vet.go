package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
//
// Flags (-fix, --format) pass through after vetFlags normalizes them; the
// package patterns default to ./... so `ultra vet -fix` means the whole
// module, exactly as `ultra vet` does. Exit codes are the analyzer's: 1 with
// findings, 0 clean, 2 on a load error — every format, unchanged.
func runVet(dir string, args []string, out, errW io.Writer) int {
	return runAnalyzer(dir, withPatterns(vetFlags(args)), out, errW)
}

// runAnalyzer executes the analyzer with argv exactly as given — no flag
// translation, no default patterns. runVet is the front door; this is the raw
// one, for callers that must speak the analyzer's own protocol (the mcp tool's
// `-json` fallback to the flat go/analysis driver on an older binary).
func runAnalyzer(dir string, args []string, out, errW io.Writer) int {
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

// vetFlags normalizes `ultra vet`'s output flags into the analyzer's own
// spelling, so one marshaling implementation serves the CLI, CI and the mcp
// `vet` tool:
//
//	--json                 → -format=json    the structured finding document
//	--format github        → -format=github  GitHub Actions annotations
//
// --json cannot pass through verbatim: on the ANALYZER's command line -json is
// reserved for the go/analysis flat driver, which `go vet -vettool` and gopls
// speak. Folding `--format X` into one token also keeps withPatterns from
// mistaking the value for a package pattern.
//
// Nothing else is touched — -fix, patterns and any future analyzer flag reach
// it unchanged. No format flag at all means the analyzer decides: the
// rustc-style report normally, GitHub annotations when GITHUB_ACTIONS=true, so
// `ultra vet ./...` in a workflow annotates the pull request with zero config.
func vetFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			out = append(out, "-format=json")
		case (a == "--format" || a == "-format") && i+1 < len(args):
			out = append(out, "-format="+args[i+1])
			i++
		default:
			out = append(out, a)
		}
	}
	return out
}

// withPatterns appends the default ./... when args carry only flags — so a
// bare `ultra vet -fix` analyzes the module rather than handing the analyzer
// a flag and no packages.
func withPatterns(args []string) []string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return args
		}
	}
	return append(append([]string{}, args...), "./...")
}
