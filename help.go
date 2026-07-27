package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// The help surface, cobra-shaped without cobra.
//
// Every command in this binary is one node in the tree below, and the tree is
// the ONLY place a description lives: the root listing, a parent's subcommand
// listing, a leaf's `--help`, and the did-you-mean on a typo all render from
// it. That is the whole reason it exists — the previous design kept one wall
// of prose in usage() and the real flag parsing somewhere else, so the two
// drifted every time a flag landed.
//
// The layout is cobra's, deliberately: title, Usage:, Available Commands:,
// Flags:, and the `Use "x [command] --help"` footer. Millions of Go CLIs have
// trained the reader's eye on that shape; importing 40k lines of dependency to
// reproduce ~150 is not a trade this repo makes.

// flagDoc is one row of a Flags: block. spec is the flag as typed
// ("--format github"), desc the one-line explanation.
type flagDoc struct{ spec, desc string }

// helpFlag is the row every node carries — the one flag that works at every
// level of the tree.
var helpFlag = flagDoc{"-h, --help", "help for %s"}

// command is one node. A node with subs renders a parent's help; a node
// without renders a leaf's.
type command struct {
	name    string
	aliases []string
	// short is the one-line description in a listing. Keep it under 58
	// characters: two columns plus the longest command name is a terminal
	// width, and a wrapped listing is a listing nobody scans.
	short string
	// args is what follows the command path on the Usage: line.
	args string
	// long is the description a leaf's --help prints above Usage. Empty
	// falls back to short, exactly as cobra does.
	long  string
	flags []flagDoc
	subs  []*command

	parent *command
}

// ultraTree is the command surface of this binary, and the source every help
// screen renders from.
func ultraTree() *command {
	root := &command{
		name:  "ultra",
		short: "the ultrastack companion",
		subs: []*command{
			{
				name:  "codes",
				short: "list every diagnostic code with its summary",
				long: `List every diagnostic code this binary knows, one line each.

ultra links no preset, so this is the kernel's DIxxxx plus the analyzer's
UVxxxx. A product binary answers the same question about ITS registry —
./app codes adds the codes the presets it wired registered at init.`,
			},
			{
				name:  "contrib",
				short: "this product's capabilities: list, add, remove",
				long: `Manage the capability set of ONE product — the contrib presets its
assembly wires.

Run it from a product root (the directory holding main.go). Every
subcommand reads the same canonical root ultravet resolves, so what
contrib reports and what the graph actually builds cannot disagree.

There is no "contrib upgrade": the presets ship as one module under one
tag, so they version in lockstep — ultra upgrade moves them all, and
ultra upgrade --check compares without writing.`,
				subs: []*command{
					{
						name:    "list",
						aliases: []string{"ls"},
						args:    "[dir]",
						short:   "WIRED and AVAILABLE capabilities, one line each",
						long: `Inventory this product's capabilities.

WIRED is the presets it imports, with the entry spelling it actually used
(pg.Use(), api.Use(api.Info{…})) and the file:line that wired it — an
import with no Use() is called out, because that is a DI0001 waiting at
Validate. AVAILABLE is the rest, each with the config section it reads and
whether it wants a dev service.`,
					},
					{
						name:  "add",
						args:  "<preset> [dir] [--dry]",
						short: "wire one preset into the assembly",
						long: `Wire one preset.

The argument goes into the assembly's bundle call (before app.Modules()
when there is one), the import is added, the file is gofmt'd, and the
refresh chain is printed. A non-canonical root is refused with the manual
one-liner to paste instead — this command edits an AST it can prove it
understands, or it edits nothing.`,
						flags: []flagDoc{{"--dry", "show the diff and write nothing"}},
					},
					{
						name:    "remove",
						aliases: []string{"rm"},
						args:    "<preset> [dir] [--dry] [--force]",
						short:   "unwire one preset, behind a dependency guard",
						long: `Unwire one preset — add's inverse, with a guard.

It refuses while product packages import the preset, or while a wired
preset needs it (jobs without pg is a DI0001, not a smaller product), and
names what is holding it. --force proceeds anyway and reports the build.`,
						flags: []flagDoc{
							{"--dry", "show the diff and write nothing"},
							{"--force", "unwire despite the dependency guard"},
						},
					},
				},
			},
			{
				name:  "dev",
				args:  `[--no-web] [--no-infra] [--no-pty] [-- <serve args>]`,
				short: "the inner loop in one terminal",
				long: `THE inner loop, in one terminal.

It boots docker-compose.dev.yml, builds and serves, then on every .go
change rebuilds and restarts GRACEFULLY and regenerates openapi.json plus
the typed client (changed bytes only). A red build keeps the previous
binary serving. The frontend dev server runs beside it under the same
Ctrl-C. On a terminal every child runs on a PTY, so vite and the API
colour and buffer as if you had run them yourself.`,
				flags: []flagDoc{
					{"--no-web", "skip the frontend dev server"},
					{"--no-infra", "skip docker-compose.dev.yml"},
					{"--no-pty", "run children on pipes, not a pty"},
					{`--build-flags "..."`, "extra flags for the go build (e.g. -race)"},
					{"-- <serve args>", "everything after -- goes to the served binary"},
				},
			},
			{
				name:  "diff",
				args:  "<old> <new>",
				short: "semantic diff of two GraphSummary files",
				long: `Diff two GraphSummary files semantically.

Providers are matched by head — type, kind, module — so a changed
dependency list reads as a modification, not a remove plus an add.

Graph files come from the app itself: write app.GraphSummary() to a file
(a one-line test or an ops endpoint) and commit it. Architecture drift
then shows up in review, and this command exits 1 when the graphs differ,
so CI can fail on it.`,
			},
			{
				name:  "explain",
				args:  "<code>",
				short: "the mini-lesson behind one diagnostic code",
				long: `Print the worked mini-lesson for a diagnostic code.

Kernel DIxxxx and analyzer UVxxxx only. A PRESET code (PG0101) belongs to
a package this companion deliberately does not link — the product binary
that wired it is the one process that can explain it:

  ./app explain PG0101`,
			},
			{
				name:  "fleet",
				short: "many products at once, from above",
				long: `The fleet layer: one team, many products, one command.

Every product is a toolbox binary with machine-readable answers (graph
--json, doctor, ultravet), so these subcommands need no special hooks —
they walk a directory for go.mod files requiring the framework and ask
each product about itself. State lives in .ultra-fleet.json at the root.`,
				subs: []*command{
					{
						name:  "status",
						args:  "[dir] [--save] [--json]",
						short: "versions, fingerprints and drift across the fleet",
						long: `One row per product: framework version, graph fingerprint, component
count, and drift against the saved baseline.

Exits 1 when any product drifted or failed to answer, so a scheduled job
can gate on it.`,
						flags: []flagDoc{
							{"--save", "record today's fingerprints as the baseline"},
							{"--json", "the table as one JSON array"},
						},
					},
					{
						name:  "vet",
						args:  "[dir]",
						short: "run the analyzer across every product",
						long: `Run ultravet over every discovered product and report per product.

Needs an installed ultravet — a fleet sweep shells out once per repo, and
` + "`go run @latest`" + ` per repo is a different command's patience.`,
					},
					{
						name:  "bump",
						args:  "[dir] --to vX.Y.Z [--push] [--pr] [--full]",
						short: "move products onto a release, on a branch",
						long: `The many-products form of ultra upgrade, driven from above.

Every product behind the target gets a branch, the pins moved, and a
build plus wiring test as verification; a failure reverts that product and
leaves the rest alone. Products already current, dirty, or on a local
replace are reported and skipped.`,
						flags: []flagDoc{
							{"--to vX.Y.Z", "the target framework version (required)"},
							{"--push", "push the branch"},
							{"--pr", "open a pull request (implies --push)"},
							{"--full", "run the full test suite, not the wiring test"},
							{"--json", "the report as one JSON array"},
						},
					},
					{
						name:  "profiles",
						args:  "[dir] [--json]",
						short: "group products by the capability set they wire",
						long: `Descriptive discovery, not a prescription.

Products are grouped by the set of preset modules they wire — the blessed
capability combos the fleet already runs. It reports what the architecture
looks like today; it recommends nothing.`,
						flags: []flagDoc{{"--json", "the profiles as one JSON document"}},
					},
				},
			},
			{
				name:  "mcp",
				short: "Model Context Protocol server over stdio",
				long: `Serve the graph intelligence as agent tools, over stdio.

  claude mcp add ultrastack -- ultra mcp

The tools are the same answers this CLI gives — explain, codes, vet,
graph, blast — so an agent and a human read one registry.`,
			},
			{
				name:  "new",
				args:  "<name> [flags]",
				short: "scaffold a product, or a feature inside one",
				long: `Scaffold a product on the paved road: main.go + internal/app +
config.toml + Taskfile + AGENTS.md.

--db --web --auth are ON by default; --bare turns all three off, and a
later --db/--web/--auth turns one back on.

--ds picks the frontend's design system (--web only). connected (the
default) wires @connected/svelte-connected-design from the depot registry, so
bun install needs depot auth. bare wires Tailwind v4 and an empty @theme:
the open foundation, for a product that diverges deliberately.

--from is REVERSE scaffolding, the legacy-service on-ramp: an OpenAPI 3.x
document in, a doctrine-shaped product out — a feature package per tag,
one api.Handle per path+method, request/response structs from the schemas,
and every handler a 501 stub (<feature>.<op>.not_implemented) so the
product boots and answers on arrival. It prints a migration report: what
came across, what was guessed, and — by path — what did not. JSON only;
convert YAML first.`,
				flags: []flagDoc{
					{"--module github.com/org/name", "the module path (default: the name)"},
					{"--version vX.Y.Z", "the framework version to require"},
					{"--bare", "no db, no web, no auth"},
					{"--db, --no-db", "Postgres pool + migrations (on by default)"},
					{"--web, --no-web", "the SvelteKit frontend (on by default)"},
					{"--auth, --no-auth", "identity + route enforcement (on by default)"},
					{"--ds connected|bare", "the design system for --web (default: connected)"},
					{"--from openapi.json", "reverse-scaffold from an OpenAPI 3.x document"},
				},
				subs: []*command{
					{
						name:  "feature",
						args:  "<name>",
						short: "one file at internal/app/<name>/<name>.go",
						long: `Write the single-file collapse form of a feature package: one file
exporting Use(), plus its errors.go.

Manifest files (handler.go, service.go, types.go) are NOT scaffolded —
they appear when content demands them, which is the doctrine's growth
rule. Run it from the product root; one line in app.go finishes the job.`,
					},
				},
			},
			{
				name:  "skill",
				short: "the vendored doctrine: generate and install",
				long: `The doctrine agents read — generated from this repo, vendored into a
product.`,
				subs: []*command{
					{
						name:  "gen",
						short: "regenerate references/ from the code",
						long: `Regenerate references/ — errors.md, and the go-doc and compiled-example
marker blocks inside the hand-written prose.

Run it from the repo root. The drift test gates it, so a doc block and the
symbol it documents cannot disagree.`,
					},
					{
						name:  "install",
						args:  "[--check] [--force]",
						short: "vendor SKILL.md + references/ into .claude/skills",
						long: `Vendor SKILL.md + references/ into .claude/skills/ultrastack at the
EXACT version go.mod pins (a replace wins: the live checkout).

Agents then read doctrine that matches the code in front of them rather
than whatever the model remembers. ultra upgrade refreshes it with the
bump.`,
						flags: []flagDoc{
							{"--check", "fail when the vendored copy is stale (for CI)"},
							{"--force", "re-vendor even when it is current"},
						},
					},
				},
			},
			{
				name:  "upgrade",
				args:  "[dir] [--check] [--to vX.Y.Z] [--all] [--dry]",
				short: "move THIS product onto a framework release",
				long: `Move one product onto a framework release — latest unless --to.

It trues the ultrastack pins, then go mod tidy, build, test. A failure
restores go.mod/go.sum, so a bad release leaves nothing behind. Contract
drift is refreshed and reported, never committed. It never touches git.

--check only answers "am I behind?": pinned vs latest, nothing written,
exit 1 when a newer release exists — a CI-friendly staleness gate.

A whole workspace at once is a fleet operation: ultra fleet bump.`,
				flags: []flagDoc{
					{"--check", "compare the pins against the latest release; write nothing"},
					{"--to vX.Y.Z", "the target version (default: the latest release)"},
					{"--all", `also "go get -u ./..." every other dependency`},
					{"--dry", "show the plan and write nothing"},
				},
			},
			{
				name:  "version",
				short: "print the ultra, build and Go versions",
				long: `Print this binary's version.

Installed from a module, that is the module version go install resolved.
Built from a checkout, it is the VCS revision the build stamped, with
-dirty when the tree had uncommitted changes. Neither available (go run
from a tarball) reports devel. The Go toolchain version rides along,
because "which Go built it" is the second question every bug report asks.`,
			},
			{
				name:  "vet",
				args:  "[packages] [-fix]",
				short: "static wiring checks before anything runs",
				long: `Static wiring checks before anything runs: the DI0001-DI0106 graph
family and the UV0001-UV0006 lints, including the product-structure laws.

-fix applies the machine-safe edits the report marks [fixable]; the rest
stay advisory, because they need a decision. Patterns default to ./...,
so a bare ` + "`ultra vet -fix`" + ` means this whole module.

Exit codes are the analyzer's, every format: 1 with findings, 0 clean, 2
on a load error.`,
				flags: []flagDoc{
					{"-fix", "apply the edits marked [fixable]"},
					{"--json", "the findings as one JSON array"},
					{"--format github", "GitHub Actions annotations (auto on GITHUB_ACTIONS)"},
				},
			},
			{
				name:  "help",
				args:  "[command]",
				short: "help about any command",
				long:  `Print the help for any command in the tree.`,
			},
		},
	}
	link(root)
	return root
}

// link fills in parent pointers and sorts each listing, so rendering never
// depends on the order the tree literal happens to be written in.
func link(c *command) {
	sort.SliceStable(c.subs, func(i, j int) bool { return c.subs[i].name < c.subs[j].name })
	for _, s := range c.subs {
		s.parent = c
		link(s)
	}
}

// find resolves one child by name or alias.
func (c *command) find(name string) *command {
	for _, s := range c.subs {
		if s.name == name {
			return s
		}
		for _, a := range s.aliases {
			if a == name {
				return s
			}
		}
	}
	return nil
}

// path is the command as the reader types it: "ultra contrib add".
func (c *command) path() string {
	if c.parent == nil {
		return c.name
	}
	return c.parent.path() + " " + c.name
}

// usage is the Usage: line's body.
func (c *command) usage() string {
	switch {
	case c.args != "":
		return c.path() + " " + c.args
	case len(c.subs) > 0:
		return c.path() + " [command]"
	default:
		return c.path()
	}
}

// help renders one node's screen. The root gets its title line; every other
// node opens with its long description, cobra-style.
func (c *command) help(w io.Writer) {
	if c.parent == nil {
		fmt.Fprintf(w, "%s — %s\n\n", c.name, c.short)
	} else if desc := strings.TrimSpace(c.longOrShort()); desc != "" {
		fmt.Fprintf(w, "%s\n\n", desc)
	}

	fmt.Fprintf(w, "Usage:\n  %s\n", c.usage())
	if len(c.subs) > 0 && c.args != "" {
		// A node that both takes arguments and nests (ultra new) shows both
		// forms — the second is the one a reader would otherwise never find.
		fmt.Fprintf(w, "  %s [command]\n", c.path())
	}

	if len(c.subs) > 0 {
		fmt.Fprint(w, "\nAvailable Commands:\n")
		t := newHelpTable(w)
		for _, s := range c.subs {
			t.row("  "+s.name, s.short)
		}
		t.flush()
	}

	fmt.Fprint(w, "\nFlags:\n")
	t := newHelpTable(w)
	for _, f := range c.flags {
		t.row("  "+f.spec, f.desc)
	}
	t.row("  "+helpFlag.spec, fmt.Sprintf(helpFlag.desc, c.name))
	t.flush()

	if len(c.subs) > 0 {
		fmt.Fprintf(w, "\nUse \"%s [command] --help\" for more information about a command.\n", c.path())
	}
}

// unknown renders the did-you-mean screen for a name this node does not have,
// and is the only place that error is worded.
func (c *command) unknown(w io.Writer, typo string) {
	fmt.Fprintf(w, "Error: unknown command %q for %q\n", typo, c.path())
	if s := c.nearest(typo); s != "" {
		fmt.Fprintf(w, "\nDid you mean %q?\n", s)
	} else if hit := c.rootward(typo); hit != nil {
		// Not a typo — a real verb, filed one level too deep. `ultra contrib
		// upgrade` is a reasonable guess (presets version in lockstep, so the
		// top-level command IS the per-preset one); point at the right level
		// instead of shrugging.
		fmt.Fprintf(w, "\nDid you mean %q?\n", hit.path())
	}
	fmt.Fprintf(w, "\nRun '%s --help' for usage.\n", c.path())
}

// rootward resolves a name against the ancestors' children — the command the
// reader meant when they nested a real verb under the wrong parent.
func (c *command) rootward(name string) *command {
	for p := c.parent; p != nil; p = p.parent {
		if hit := p.find(name); hit != nil {
			return hit
		}
	}
	return nil
}

// nearest is the closest child name within an edit distance of 2 — close
// enough to be a typo, far enough that a different word is not "corrected"
// into one the reader never meant.
func (c *command) nearest(typo string) string {
	best, bestD := "", 3
	for _, s := range c.subs {
		for _, name := range append([]string{s.name}, s.aliases...) {
			if d := editDistance(strings.ToLower(typo), name); d < bestD {
				best, bestD = s.name, d
			}
		}
	}
	return best
}

// editDistance is the classic Levenshtein distance, on bytes: command names
// are ASCII, and a typo is by definition not multi-byte.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = minOf(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func minOf(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func (c *command) longOrShort() string {
	if c.long != "" {
		return c.long
	}
	return c.short
}

// wantsHelp reports whether -h/--help appears in args, stopping at a bare
// "--": everything past that belongs to the child process (`ultra dev -- -h`
// is the served binary's flag, not ours).
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}
