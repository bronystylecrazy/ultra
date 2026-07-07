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
		for _, c := range diag.AllCodes {
			lesson, _ := diag.Lesson(c)
			fmt.Fprintf(out, "%s  %s\n", c, firstLine(lesson))
		}
		return 0

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

  ultra explain <code>            mini-lesson for a diagnostic (e.g. DI0101)
  ultra codes                     list every diagnostic code
  ultra diff <old> <new>          semantic diff of two GraphSummary files
                                  (exit 1 when the graphs differ)
`)
}
