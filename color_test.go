package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// setEnv sets (or, for an empty value, unsets) environment variables for the
// duration of one test and restores exactly what was there before. The colour
// rules are read FROM the environment, so a test that only ever sets can never
// assert the default — and the machine running it may already export NO_COLOR.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		if v == "" {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, v)
		}
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

// forceTerminal makes the one TTY probe in cmd/ultra answer yes — the seam
// that exists because there is no terminal in CI and every colour and pty
// decision hangs off this function.
func forceTerminal(t *testing.T, answer bool) {
	t.Helper()
	old := isTerminal
	isTerminal = func(*os.File) bool { return answer }
	t.Cleanup(func() { isTerminal = old })
}

// The decision table, in precedence order. It is deliberately the same table
// di/diag.WantColor implements, because a product's boot diagnostic and the
// banner ultra prints above it must never disagree about the terminal.
func TestColorForFollowsTheStandardRules(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		tty  bool
		file bool // write to os.Stdout rather than a buffer
		want bool
		why  string
	}{
		{"terminal", nil, true, true, true, "the default: a terminal gets colour"},
		{"not a terminal", nil, false, true, false, "a redirected stdout does not"},
		{"not a file at all", nil, true, false, false, "a bytes.Buffer is nobody's terminal"},
		{"NO_COLOR", map[string]string{"NO_COLOR": "1"}, true, true, false, "NO_COLOR wins over a terminal"},
		{"NO_COLOR empty value", map[string]string{"NO_COLOR": " "}, true, true, false, "any value counts (no-color.org)"},
		{"FORCE_COLOR", map[string]string{"FORCE_COLOR": "1"}, false, true, true, "forced over a pipe: CI logs that keep ANSI"},
		{"CLICOLOR_FORCE", map[string]string{"CLICOLOR_FORCE": "1"}, false, true, true, "the other spelling"},
		{"NO_COLOR beats FORCE_COLOR", map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1"}, true, true, false,
			"the user said no, and no is not a preference to be overridden"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setEnv(t, map[string]string{"NO_COLOR": "", "FORCE_COLOR": "", "CLICOLOR_FORCE": ""})
			setEnv(t, c.env)
			forceTerminal(t, c.tty)

			var w = os.Stdout
			if !c.file {
				if got := colorFor(&bytes.Buffer{}); bool(got) != c.want {
					t.Fatalf("colorFor(buffer) = %v, want %v — %s", got, c.want, c.why)
				}
				return
			}
			if got := colorFor(w); bool(got) != c.want {
				t.Fatalf("colorFor(os.Stdout) = %v, want %v — %s", got, c.want, c.why)
			}
		})
	}
}

// Off means BYTE-IDENTICAL to what was printed before colour existed. A
// palette that emits a reset sequence "just in case" breaks every golden test
// and every `ultra ... > file` in a script.
func TestPaletteOffIsExactlyPlainText(t *testing.T) {
	var off palette
	for _, got := range []string{
		off.red("x"), off.green("x"), off.yellow("x"), off.dim("x"), off.bold("x"),
	} {
		if got != "x" {
			t.Fatalf("a disabled palette wrote %q", got)
		}
	}
	if got := off.banner("●", "serving pid 1"); got != "● serving pid 1" {
		t.Fatalf("banner off = %q", got)
	}
}

// On, the GLYPH carries the colour and the words stay at the terminal's
// default — except `·`, where the aside dims whole.
func TestPaletteBannerColorsTheGlyph(t *testing.T) {
	on := palette(true)
	cases := []struct {
		glyph, seq string
	}{
		{"●", cGreen}, {"✗", cRed}, {"↻", cYellow},
	}
	for _, c := range cases {
		got := on.banner(c.glyph, "text")
		if want := c.seq + c.glyph + cReset + " text"; got != want {
			t.Errorf("banner(%q) = %q, want %q", c.glyph, got, want)
		}
	}
	if got, want := on.banner("·", "aside"), cDim+"· aside"+cReset; got != want {
		t.Errorf("banner(·) = %q, want %q — the whole aside dims", got, want)
	}
	// An empty string never gets wrapped: a bare reset in the output stream
	// is invisible until something else lands on it.
	if got := on.red(""); got != "" {
		t.Errorf("red(\"\") = %q", got)
	}
}

// The palette must be a strict wrapper — never touching the text it is given.
func TestPaletteKeepsTheTextIntact(t *testing.T) {
	on := palette(true)
	got := on.green("v0.9.16")
	if !strings.Contains(got, "v0.9.16") || !strings.HasSuffix(got, cReset) {
		t.Fatalf("green() = %q", got)
	}
}
