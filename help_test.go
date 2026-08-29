package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

// The root screen is a GOLDEN. Its shape — title, Usage:, Available Commands:,
// Flags:, the `Use "…"` footer — is the shape every cobra CLI has trained the
// reader's eye on, and drifting off it one commit at a time is exactly how a
// help screen stops being recognisable. A new command changes this golden; the
// change should be visible in the diff.
func TestRootHelpGolden(t *testing.T) {
	const want = `ultra — the ultrastack companion

Usage:
  ultra [command]

Available Commands:
  add          grow a product: one infrastructure line per preset
  breaking     gate a contract change against its consumers
  brief        the orientation pack: one product, one page
  codes        list every diagnostic code with its summary
  compliance   the class-2 compliance templates: init
  contrib      this product's capabilities: list, add, remove
  dev          the inner loop in one terminal
  diff         semantic diff of two GraphSummary files
  explain      the mini-lesson behind one diagnostic code
  fleet        many products at once, from above
  help         help about any command
  init         bring an EXISTING product up to the current doctrine
  mcp          Model Context Protocol server over stdio
  new          scaffold a product, or a feature inside one
  records      frozen release evidence: freeze, sign
  report       file a field report where the fleet can read it
  req          the requirements ledger: approve, ask, change
  skill        the vendored doctrine: generate and install
  trace        requirements × contract × recorded runs, derived
  upgrade      move THIS product onto a framework release
  version      print the ultra, build and Go versions
  vet          static wiring checks before anything runs

Flags:
  -h, --help   help for ultra

Use "ultra [command] --help" for more information about a command.
`
	code, out, errOut := runCLI(t, "--help")
	if code != 0 {
		t.Fatalf("--help must exit 0, got %d (%s)", code, errOut)
	}
	if out != want {
		t.Errorf("ultra --help drifted:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	// `help` and `-h` are the same screen. A reader should never have to
	// discover which spelling this particular tool prefers.
	for _, spelling := range []string{"help", "-h"} {
		if _, alt, _ := runCLI(t, spelling); alt != want {
			t.Errorf("`ultra %s` printed a different screen than --help", spelling)
		}
	}
	// Bare `ultra` prints the same thing, but to STDERR and exit 2: a script
	// that lost its arguments must fail, not succeed quietly.
	code, out, errOut = runCLI(t)
	if code != 2 || out != "" || errOut != want {
		t.Errorf("bare ultra: code=%d stdout=%q stderr drift=%v", code, out, errOut != want)
	}
}

// A PARENT renders its own screen: its prose, its subcommands, its footer.
func TestParentHelpGolden(t *testing.T) {
	const want = `Manage the capability set of ONE product — the contrib presets its
assembly wires.

Run it from a product root (the directory holding main.go). Every
subcommand reads the same canonical root ultravet resolves, so what
contrib reports and what the graph actually builds cannot disagree.

There is no "contrib upgrade": the presets ship as one module under one
tag, so they version in lockstep — ultra upgrade moves them all, and
ultra upgrade --check compares without writing.

Usage:
  ultra contrib [command]

Available Commands:
  add      wire one preset into the assembly
  list     WIRED and AVAILABLE capabilities, one line each
  remove   unwire one preset, behind a dependency guard

Flags:
  -h, --help   help for contrib

Use "ultra contrib [command] --help" for more information about a command.
`
	code, out, errOut := runCLI(t, "contrib", "--help")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if out != want {
		t.Errorf("ultra contrib --help drifted:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	// Invoked bare, the same screen goes to stderr and exits 2.
	code, out, errOut = runCLI(t, "contrib")
	if code != 2 || out != "" || errOut != want {
		t.Errorf("bare parent: code=%d stdout=%q stderr drift=%v", code, out, errOut != want)
	}
}

// A LEAF renders its long description and its flags — including the two that
// only exist as flags (--json, --format), which the old wall of prose buried
// three levels of indentation deep.
func TestLeafHelpGolden(t *testing.T) {
	const want = "Wire one preset.\n" + `
The argument goes into the assembly's bundle call (before app.Modules
when there is one), the import is added, the file is gofmt'd, and the
refresh chain is printed. A non-canonical root is refused with the manual
one-liner to paste instead — this command edits an AST it can prove it
understands, or it edits nothing.

Usage:
  ultra contrib add <preset> [dir] [--dry]

Flags:
  --dry        show the diff and write nothing
  -h, --help   help for add
`
	code, out, errOut := runCLI(t, "contrib", "add", "--help")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if out != want {
		t.Errorf("ultra contrib add --help drifted:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// -h/--help works at all three levels, and never runs the command.
func TestHelpAtEveryLevel(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-h"}, "Available Commands:"},
		{[]string{"--help"}, "Available Commands:"},
		{[]string{"fleet", "-h"}, `Use "ultra fleet [command] --help"`},
		{[]string{"fleet", "--help"}, "Available Commands:"},
		{[]string{"fleet", "status", "-h"}, "record today's fingerprints as the baseline"},
		{[]string{"fleet", "status", "--help"}, "Usage:\n  ultra fleet status [dir] [--save] [--json]"},
		{[]string{"help", "fleet", "bump"}, "The many-products form of ultra upgrade"},
		{[]string{"skill", "install", "--help"}, "fail when the vendored copy is stale (for CI)"},
		// A leaf that would otherwise DO something must not do it.
		{[]string{"vet", "--help"}, "apply the edits marked [fixable]"},
		{[]string{"new", "feature", "--help"}, "which is the doctrine's growth"},
	}
	for _, c := range cases {
		code, out, errOut := runCLI(t, c.args...)
		if code != 0 {
			t.Errorf("ultra %v: code=%d err=%s", c.args, code, errOut)
			continue
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("ultra %v missing %q:\n%s", c.args, c.want, out)
		}
		if errOut != "" {
			t.Errorf("ultra %v wrote to stderr: %q", c.args, errOut)
		}
	}
}

// The typo screen, at both levels, with cobra's wording.
func TestUnknownCommandDidYouMean(t *testing.T) {
	code, out, errOut := runCLI(t, "contib")
	if code != 2 || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	const wantRoot = `Error: unknown command "contib" for "ultra"

Did you mean "contrib"?

Run 'ultra --help' for usage.
`
	if errOut != wantRoot {
		t.Errorf("root typo screen:\n--- got ---\n%s\n--- want ---\n%s", errOut, wantRoot)
	}

	code, _, errOut = runCLI(t, "contrib", "lst")
	if code != 2 {
		t.Fatalf("code=%d", code)
	}
	const wantSub = `Error: unknown command "lst" for "ultra contrib"

Did you mean "list"?

Run 'ultra contrib --help' for usage.
`
	if errOut != wantSub {
		t.Errorf("subcommand typo screen:\n--- got ---\n%s\n--- want ---\n%s", errOut, wantSub)
	}

	// Distance > 2 gets no guess: a wrong suggestion is worse than none.
	if _, _, errOut := runCLI(t, "frobnicate"); strings.Contains(errOut, "Did you mean") {
		t.Errorf("a distant word must not be 'corrected':\n%s", errOut)
	}
	// An alias is a real spelling, and suggesting it points at the canonical
	// name rather than the nickname.
	if _, _, errOut := runCLI(t, "contrib", "l"); !strings.Contains(errOut, `Did you mean "list"?`) {
		t.Errorf("alias-adjacent typo:\n%s", errOut)
	}
}

// ORDER 5, mechanically: a Short longer than a listing column is a Short that
// wraps, and a wrapped listing is one nobody scans.
func TestEveryShortIsOneLine(t *testing.T) {
	var walk func(*command)
	walk = func(c *command) {
		if c.parent != nil {
			if c.short == "" {
				t.Errorf("%s has no short description", c.path())
			}
			if n := len([]rune(c.short)); n > 58 {
				t.Errorf("%s: short is %d chars, max 58 — %q", c.path(), n, c.short)
			}
			if strings.Contains(c.short, "\n") {
				t.Errorf("%s: short must be one line", c.path())
			}
		}
		for _, s := range c.subs {
			walk(s)
		}
	}
	walk(ultraTree())
}

// Every command the dispatcher answers to is in the tree, and every command in
// the tree is one the dispatcher answers to. The tree IS the surface; a
// command reachable but undocumented is the drift this design exists to stop.
func TestTreeMatchesDispatch(t *testing.T) {
	for _, c := range ultraTree().subs {
		var out, errW bytes.Buffer
		if code := run([]string{c.name, "--help"}, &out, &errW); code != 0 {
			t.Errorf("%q is in the tree but --help does not answer (code %d)", c.name, code)
		}
	}
	// And the inverse, spot-checked: a name that is not in the tree is a typo,
	// never a hidden verb.
	var out, errW bytes.Buffer
	if code := run([]string{"secretly-supported"}, &out, &errW); code != 2 {
		t.Errorf("an undocumented name must be unknown, got %d", code)
	}
}

// ---- ORDER 3: the version fallback chain ----

func TestUltraVersionFallbackChain(t *testing.T) {
	cases := []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string
		why  string
	}{
		{"installed from a module",
			&debug.BuildInfo{Main: debug.Module{Version: "v0.9.16"}}, true,
			"v0.9.16", "go install …@v0.9.16 records the module version"},
		{"built in a clean checkout",
			&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "4f2a1c9d8e7b6a5c4d3e2f1a"},
				{Key: "vcs.modified", Value: "false"},
			}}, true,
			"4f2a1c9d8e7b", "no module version, so the revision answers — short, like git"},
		{"built in a dirty checkout",
			&debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "4f2a1c9d8e7b6a5c4d3e2f1a"},
				{Key: "vcs.modified", Value: "true"},
			}}, true,
			"4f2a1c9d8e7b-dirty", "uncommitted changes are part of the answer"},
		{"neither", &debug.BuildInfo{}, true, "devel",
			"a tarball build knows nothing, and says so"},
		{"no build info at all", nil, false, "devel", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ultraVersion(c.bi, c.ok).Version; got != c.want {
				t.Errorf("version = %q, want %q — %s", got, c.want, c.why)
			}
		})
	}
}

func TestVersionCommandPrintsAndVerdicts(t *testing.T) {
	old := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{GoVersion: "go1.26.3", Main: debug.Module{Version: "v0.9.16"}}, true
	}
	t.Cleanup(func() { readBuildInfo = old })

	code, out, errOut := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out, "ultra v0.9.16\ngo1.26.3 ") {
		t.Errorf("version stdout = %q", out)
	}
	if !strings.Contains(errOut, "✓ version — ultra v0.9.16") {
		t.Errorf("version verdict = %q", errOut)
	}
}
