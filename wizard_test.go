package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
)

// wizardRuns are the shapes a human can leave the form in — the raw values huh
// writes into wizardForm, before any rule has been applied to them. Every law
// below is proven for each one.
var wizardRuns = []struct {
	name string
	form wizardForm
}{
	{"the paved road", wizardForm{Name: "watchpost", Caps: []string{capDB, capWeb, capAuth}, DS: dsConnected}},
	{"bare", wizardForm{Name: "tiny", DS: dsConnected}},
	{"db alone", wizardForm{Name: "ledger", Caps: []string{capDB}, DS: dsConnected}},
	{"web on the bare design system", wizardForm{Name: "kiosk", Caps: []string{capWeb}, DS: dsBare}},
	// The one combination the flag parser rejects outright, produced at the
	// form the way a human produces it: tick mqtt, leave auth alone.
	{"mqtt without auth", wizardForm{Name: "edge", Caps: []string{capMQTT}, DS: dsConnected}},
	{"everything", wizardForm{Name: "depot-reg", Caps: []string{capDB, capWeb, capAuth, capMQTT}, DS: dsBare}},
	{"react", wizardForm{Name: "kiosk", Caps: []string{capWeb, capAuth}, Frontend: feReact, DS: dsConnected}},
	// A revised run: web ticked, react chosen, then web unticked — the
	// answer must not leak into a command where --frontend is a usage error.
	{"react without web", wizardForm{Name: "tiny", Frontend: feReact, DS: dsBare}},
}

// TestWizardEmitsTheSameScaffoldAsItsEchoedCommand is the wizard's whole
// contract, mechanically.
//
// The form prints a command and calls it "equivalent". This runs that exact
// string back through the REAL flag parser and demands the product it parses
// to be identical to the one the summary described. Two failures are caught
// here and nowhere else: an echoed command the parser would reject (so the
// promise "reproducible, CI-safe" is a lie), and an echoed command that builds
// a DIFFERENT product than the one the user just approved on screen.
func TestWizardEmitsTheSameScaffoldAsItsEchoedCommand(t *testing.T) {
	for _, r := range wizardRuns {
		t.Run(r.name, func(t *testing.T) {
			f := r.form
			a := f.answers()

			var errW bytes.Buffer
			parsed, code := parseNewArgs(a.args(), &errW)
			if code != 0 {
				t.Fatalf("the wizard echoed a command its own parser rejects:\n  %s\n%s",
					a.command(), errW.String())
			}
			if got, want := parsed.data, a.data(); !reflect.DeepEqual(got, want) {
				t.Errorf("the summary and the echoed command describe different products\n"+
					"  command: %s\n  summary: %#v\n  parsed:  %#v", a.command(), want, got)
			}
		})
	}
}

// TestWizardResolvesRatherThanRejects pins the two rules the form applies for
// the user instead of failing them with a usage error.
func TestWizardResolvesRatherThanRejects(t *testing.T) {
	t.Run("mqtt turns auth on", func(t *testing.T) {
		f := wizardForm{Name: "edge", Caps: []string{capMQTT}, DS: dsConnected}
		a := f.answers()
		if !a.Auth {
			t.Error("mqtt must imply auth — the broker's CONNECT gate authenticates every device")
		}
		if !slices.Contains(a.args(), "--auth") {
			t.Errorf("the echoed command must carry --auth: %s", a.command())
		}
	})

	t.Run("no design system question without a frontend", func(t *testing.T) {
		// The DS group is hidden when web is off, so whatever the field holds
		// is an answer the user was never asked for. It must not reach the
		// command line, where `--ds` without `--web` is a usage error.
		f := wizardForm{Name: "tiny", DS: dsConnected}
		a := f.answers()
		if a.DS != "" {
			t.Errorf("DS must be empty without web, got %q", a.DS)
		}
		if slices.Contains(a.args(), "--ds") {
			t.Errorf("no --ds may be echoed when web is off: %s", a.command())
		}
	})

	t.Run("web without an answer takes the default", func(t *testing.T) {
		f := wizardForm{Name: "kiosk", Caps: []string{capWeb}}
		if got := f.answers().DS; got != dsConnected {
			t.Errorf("web must default to %s, got %q", dsConnected, got)
		}
	})
}

// TestWizardEchoesEveryDefaultOnCapability pins the echo's explicitness. A
// command that only lists what to ADD is not reproducible: it inherits
// whatever the defaults are on the day it is re-run.
func TestWizardEchoesEveryDefaultOnCapability(t *testing.T) {
	f := wizardForm{Name: "ledger", Caps: []string{capDB}, DS: dsConnected}
	got := f.answers().command()
	want := "ultra new ledger --db --no-web --no-auth"
	if got != want {
		t.Errorf("echoed command drifted:\n  got  %s\n  want %s", got, want)
	}
}

// TestWizardNameValidationIsTheFlagPathsRule proves the form and the scaffold
// agree on what a name is, by asserting the wizard's validator against nameRe
// itself — the value scaffold checks. A copied pattern would drift; this
// cannot.
func TestWizardNameValidationIsTheFlagPathsRule(t *testing.T) {
	for _, s := range []string{
		"watchpost", "a", "a-b-2", "depot-reg", "x9",
		"", "Bad", "9lives", "has_underscore", "-lead", "UPPER", "with space", "trailing-",
	} {
		want := nameRe.MatchString(s)
		if got := validateProductName(s) == nil; got != want {
			t.Errorf("name %q: the form accepts=%v, nameRe accepts=%v", s, got, want)
		}
	}
}

// TestWizardNeverFiresWithoutATerminal is the gate that keeps agents and CI
// out of the form. Every clause of wizardWanted is exercised, and the last
// case is the one that matters most: a stubbed-true terminal probe still must
// not open a form on a writer that is not the real stdout, because every test
// in this package passes a buffer.
func TestWizardNeverFiresWithoutATerminal(t *testing.T) {
	var buf bytes.Buffer
	for _, c := range []struct {
		name string
		args []string
		out  io.Writer
		want bool
	}{
		{"zero args to a buffer", nil, &buf, false},
		{"zero args to real stdout", nil, os.Stdout, isTerminal(os.Stdin) && isTerminal(os.Stdout)},
		{"a name is the flag form", []string{"watchpost"}, os.Stdout, false},
		{"a flag is the flag form", []string{"--bare"}, os.Stdout, false},
		{"help is the flag form", []string{"--help"}, os.Stdout, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := wizardWanted(c.args, c.out); got != c.want {
				t.Errorf("wizardWanted = %v, want %v", got, c.want)
			}
		})
	}

	// And with the probe forced ON — the shape a test that stubs isTerminal
	// for its own reasons would otherwise create.
	old := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = old })
	if wizardWanted(nil, &buf) {
		t.Error("a buffer is never a terminal, however isTerminal answers")
	}
}

// TestBareNewStillPrintsHelp pins the headless behaviour the wizard must not
// have changed: `ultra new` with no name, writing somewhere that is not a
// terminal, is a usage error with the command's help — never a prompt.
func TestBareNewStillPrintsHelp(t *testing.T) {
	var out, errW bytes.Buffer
	code := cmdNew(nil, &out, &errW)
	if code != 2 {
		t.Fatalf("bare `ultra new` must be a usage error, got %d", code)
	}
	if out.String() != "" {
		t.Errorf("nothing may go to stdout:\n%s", out.String())
	}
	for _, want := range []string{"Usage:", "ultra new <name> [flags]", "--bare", "--ds connected|bare"} {
		if !strings.Contains(errW.String(), want) {
			t.Errorf("the help must name %q:\n%s", want, errW.String())
		}
	}
}

// TestWizardAbortWritesNothing pins the abort contract: a form left before
// Confirm — ctrl-c, or Cancel at the last question — must leave the working
// directory exactly as it found it. The scaffold is the LAST thing the wizard
// does, after the only answer that authorises it.
//
// finish is called directly rather than through cmdNewWizard because the form
// half needs a terminal to reach a decision; a test that waited for one would
// hang instead of failing. Everything that can write is on this side of it.
func TestWizardAbortWritesNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		form wizardForm
		err  error
		want string
	}{
		{"ctrl-c", wizardForm{Name: "watchpost", Caps: []string{capDB}}, huh.ErrUserAborted, "aborted"},
		{"cancel at the summary", wizardForm{Name: "watchpost", Caps: []string{capDB}}, nil, "cancelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			var out, errW bytes.Buffer
			f := c.form // OK is false: nobody confirmed
			if code := f.finish(c.err, &out, &errW); code != 1 {
				t.Errorf("an abort must exit 1, got %d", code)
			}
			if !strings.Contains(errW.String(), c.want) ||
				!strings.Contains(errW.String(), "nothing written") {
				t.Errorf("the abort must say %q and that nothing was written: %s", c.want, errW.String())
			}
			left, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				var names []string
				for _, e := range left {
					names = append(names, e.Name())
				}
				t.Errorf("an aborted wizard wrote %v into %s", names, filepath.Base(dir))
			}
		})
	}
}

// TestWizardSummaryNamesTheProduct keeps the one line a human actually reads
// before saying yes honest about all four of its facts.
func TestWizardSummaryNamesTheProduct(t *testing.T) {
	f := wizardForm{Name: "watchpost", Caps: []string{capDB, capWeb, capAuth}, DS: dsConnected}
	got := f.answers().summary()
	for _, want := range []string{"watchpost", "db, web, auth", dsConnected + " DS", scaffoldGoVersion, resolveVersion("")} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary must name %q: %s", want, got)
		}
	}
	// A bare product has no design system to name.
	bare := wizardForm{Name: "tiny"}
	if s := bare.answers().summary(); strings.Contains(s, "DS") {
		t.Errorf("a bare product's summary must not mention a design system: %s", s)
	}
}
