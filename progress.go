package main

import (
	"fmt"
	"io"
	"os"
	"time"
)

// step announces a long-running phase BEFORE it starts and reports its
// duration when it ends — `ultra upgrade` runs tidy → build → a full test
// suite, minutes of silence on a real product without this. On a terminal
// the pending line is rewritten in place; piped output gets two plain
// lines. The verdict line at the end is unchanged — this is the heartbeat,
// not the summary.
type step struct {
	w     io.Writer
	label string
	tty   bool
	began time.Time
}

func beginStep(w io.Writer, label string) *step {
	f, isFile := w.(*os.File)
	s := &step{w: w, label: label, tty: isFile && isTerminal(f), began: time.Now()}
	if s.tty {
		fmt.Fprintf(w, "  %s %s …", colorFor(w).dim("→"), label)
	} else {
		fmt.Fprintf(w, "  → %s …\n", label)
	}
	return s
}

func (s *step) done(ok bool) {
	d := time.Since(s.began).Round(100 * time.Millisecond)
	glyph := colorFor(s.w).green("✓")
	if !ok {
		glyph = colorFor(s.w).red("✗")
	}
	if s.tty {
		fmt.Fprintf(s.w, "\r\033[K  %s %s (%s)\n", glyph, s.label, d)
	} else {
		fmt.Fprintf(s.w, "  %s %s (%s)\n", glyph, s.label, d)
	}
}
