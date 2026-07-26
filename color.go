package main

import (
	"io"
	"os"
)

// Colour in `ultra`, in one place.
//
// The kernel already owns this decision for diagnostics — di/diag.WantColor —
// and the rule here is deliberately the SAME rule, mirrored rather than
// imported so cmd/ultra can also ask it of a writer that is not os.Stdout
// (every command in this binary takes its out/errW, which is a buffer under
// test). One product, one answer:
//
//  1. NO_COLOR set (any value)            → never (https://no-color.org)
//  2. CLICOLOR_FORCE or FORCE_COLOR set   → always (CI logs that keep ANSI)
//  3. otherwise                           → only when the writer is a terminal
//
// There is deliberately no --no-color flag: NO_COLOR is the standard, every
// other tool in the pipeline already honours it, and one more flag to
// remember is one more thing to get wrong in a CI file.

// ANSI sequences, mirroring di/diag/render.go's palette so a diagnostic
// printed by the app and a banner printed by ultra are the same green.
const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cDim    = "\x1b[2m"
)

// isTerminal is the ONE terminal probe in cmd/ultra. It is a var so a test can
// force the answer: everything downstream — colour, and whether a supervised
// child gets a pty — hangs off it, and neither is reachable from a test
// harness whose stdout is a pipe.
var isTerminal = func(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// palette is "should this writer get ANSI" made callable. The zero value is
// the plain one, so a struct that never sets it prints exactly what it printed
// before colour existed.
type palette bool

// colorFor decides for w, by the rules at the top of this file.
func colorFor(w io.Writer) palette {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" || os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	f, ok := w.(*os.File)
	return palette(ok && isTerminal(f))
}

func (p palette) wrap(seq, s string) string {
	if !p || s == "" {
		return s
	}
	return seq + s + cReset
}

func (p palette) red(s string) string    { return p.wrap(cRed, s) }
func (p palette) green(s string) string  { return p.wrap(cGreen, s) }
func (p palette) yellow(s string) string { return p.wrap(cYellow, s) }
func (p palette) dim(s string) string    { return p.wrap(cDim, s) }
func (p palette) bold(s string) string   { return p.wrap(cBold, s) }

// banner renders one status line. The GLYPH carries the colour and the text
// stays at the terminal's default foreground: a status line is read for its
// words, and a wall of green sentences is harder to scan than a green dot in
// front of a plain one. `·` is the exception — it is an aside, and the whole
// line dims, because a dimmed dot in front of bright text says nothing.
func (p palette) banner(glyph, text string) string {
	switch glyph {
	case "●", "✓":
		return p.green(glyph) + " " + text
	case "✗":
		return p.red(glyph) + " " + text
	case "↻":
		return p.yellow(glyph) + " " + text
	case "·":
		return p.dim(glyph + " " + text)
	}
	return glyph + " " + text
}
