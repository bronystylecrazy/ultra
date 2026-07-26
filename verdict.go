package main

import (
	"fmt"
	"io"
)

// NO SILENT SUCCESS.
//
// A command that finishes and prints nothing has told the reader two things at
// once — "it worked" and "it did nothing" — and they cannot tell which. Every
// command in this binary therefore ends with exactly one verdict line:
//
//	✓ vet — no findings
//	✗ vet — 3 findings (1 fixable)
//	✓ skill gen — references up to date
//
// It goes to STDERR, always. stdout is a machine's stream: `ultra vet --json`
// pipes into jq, `ultra codes` into grep, `ultra diff` into a review comment.
// Writing the verdict beside them would corrupt every one of those, and
// suppressing it in machine modes would mean the mode nobody watches is also
// the mode that says nothing when it fails. stderr is the stream that is
// already prose, so the verdict rides there and the contract on stdout stays
// byte-for-byte what it was.
//
// The glyph carries the colour, the words stay default — colour.go's rule.

// verdict prints the success line for a command.
func verdict(w io.Writer, cmd, detail string) {
	writeVerdict(w, "✓", cmd, detail)
}

// failVerdict prints the failure line. It is not an error message — the error
// was already reported in full; this is the one-line result a reader scanning
// a long log needs to find.
func failVerdict(w io.Writer, cmd, detail string) {
	writeVerdict(w, "✗", cmd, detail)
}

// verdictFor picks the glyph from an ok flag, for the many callers whose
// outcome is a boolean.
func verdictFor(w io.Writer, ok bool, cmd, detail string) {
	if ok {
		verdict(w, cmd, detail)
		return
	}
	failVerdict(w, cmd, detail)
}

func writeVerdict(w io.Writer, glyph, cmd, detail string) {
	text := cmd
	if detail != "" {
		text += " — " + detail
	}
	fmt.Fprintln(w, colorFor(w).banner(glyph, text))
}

// count renders "1 finding" / "3 findings" — verdicts count things, and
// "1 findings" reads like a bug in the tool.
func count(n int, noun string) string {
	return fmt.Sprintf("%d %s%s", n, noun, plural(n))
}
