// ultra is the ultrastack companion CLI.
//
//	ultra explain DI0101          # the mini-lesson behind any diagnostic code
//	ultra codes                   # list every code
//	ultra diff old.graph new.graph  # semantic diff of two GraphSummary files
//
// Graph files come from the app itself: write app.GraphSummary() to a file
// (a one-line test or ops endpoint) and commit it; `ultra diff` then makes
// architecture drift reviewable — and exits non-zero on change for CI.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/bronystylecrazy/ultrastack/di/diag"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errW io.Writer) int {
	if len(args) == 0 {
		usage(errW)
		return 2
	}
	switch args[0] {
	case "explain":
		if len(args) != 2 {
			fmt.Fprintln(errW, "usage: ultra explain DI0001")
			return 2
		}
		lesson, ok := diag.Lesson(diag.Code(args[1]))
		if !ok {
			fmt.Fprintf(errW, "unknown code %q — run `ultra codes` for the registry\n", args[1])
			return 1
		}
		fmt.Fprintln(out, lesson)
		return 0

	case "codes":
		fmt.Fprint(out, codesText())
		return 0

	case "new":
		return cmdNew(args[1:], out, errW)

	case "vet":
		return cmdVet(args[1:], out, errW)

	case "fleet":
		return cmdFleet(args[1:], out, errW)

	case "mcp":
		return cmdMCP(os.Stdin, out, errW)

	case "skill":
		return cmdSkill(args[1:], out, errW)

	case "diff":
		if len(args) != 3 {
			fmt.Fprintln(errW, "usage: ultra diff old.graph new.graph")
			return 2
		}
		oldB, err := os.ReadFile(args[1])
		if err != nil {
			fmt.Fprintln(errW, err)
			return 1
		}
		newB, err := os.ReadFile(args[2])
		if err != nil {
			fmt.Fprintln(errW, err)
			return 1
		}
		report, changed := diffGraphs(string(oldB), string(newB))
		fmt.Fprint(out, report)
		if changed {
			return 1 // CI-friendly: drift fails the step
		}
		return 0

	case "help", "-h", "--help":
		usage(out)
		return 0
	}
	fmt.Fprintf(errW, "unknown command %q\n", args[0])
	usage(errW)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ultra — the ultrastack companion

  ultra new <name> [--module m]   scaffold a product on the paved road:
                                  main.go + internal/app + config.toml +
                                  Taskfile. --db --web --auth are on by
                                  default; --bare strips to the core
  ultra new feature <name>        one file at internal/app/<name>/<name>.go
                                  exporting Use() — then one line in app.go
  ultra vet [packages] [-fix]     static wiring checks (DI0001-DI0106 graph family, UV0001)
                                  before anything runs; -fix applies the
                                  machine-safe edits the report marks [fixable]
                                  (the rest stay advisory — they need a decision)
  ultra fleet status|vet [dir]    the whole fleet: versions, graph
                                  fingerprints + drift (--save baselines),
                                  analyzer findings across every product
  ultra fleet bump [dir] --to vX.Y.Z   move products behind vX.Y.Z onto it on a
                                  branch (build + wiring test verify; a failure
                                  reverts); --push/--pr publish, --full full tests
  ultra fleet profiles [dir]      group products by capability set (the preset
                                  modules they wire) — descriptive discovery
  ultra explain <code>            mini-lesson for a diagnostic (e.g. DI0101)
  ultra codes                     list every diagnostic code
  ultra diff <old> <new>          semantic diff of two GraphSummary files
                                  (exit 1 when the graphs differ)
  ultra mcp                       Model Context Protocol server over stdio:
                                  the graph intelligence as agent tools
                                  (claude mcp add ultrastack -- ultra mcp)
  ultra skill gen                 regenerate references/ (errors.md + go-doc
                                  and compiled-example marker blocks); run
                                  from the repo root — the drift test gates it
`)
}
