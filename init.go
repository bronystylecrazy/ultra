package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// `ultra init [dir] [--force <file>] [--dry]` is the RETROFIT verb.
//
// `ultra new` emits the doctrine files at BIRTH and never again, so a product
// scaffolded by an older release — or ported, or written by hand — is missing
// whatever the scaffold learned since. There is no signal when that happens:
// the product builds, tests, and upgrades perfectly well without AGENTS.md,
// and stays un-agented forever.
//
// init closes that gap and nothing else. It renders the doctrine files the
// CURRENT scaffold emits that are ABSENT here, from the SAME templates, with
// the capability flags read off the tree in front of it rather than typed. A
// file that already exists is never touched, never merged, never diffed — the
// product's copy is the product's business, and a retrofit that edits
// hand-written prose is a retrofit nobody runs twice. --force is the one door
// out, and it shows the diff before it opens.
//
// The consequence is that init is idempotent by construction: the second run
// writes nothing, and says so.

// doctrineFiles are the OUTPUT paths init owns — the files `ultra new` emits
// as doctrine rather than as code. Which TEMPLATE each comes from is
// deliberately not restated: it is looked up in scaffoldFiles, so `ultra new`
// and `ultra init` cannot render different bytes under the same name, and a
// file that a shape does not earn (no web/, no e2e spec) drops out of the plan
// because scaffoldFiles never offered it.
var doctrineFiles = []string{
	"AGENTS.md",
	".mcp.json",
	// The covenant AGENTS.md cites. A product born before this file upgrades
	// perfectly, reads its regenerated AGENTS.md, and is told to satisfy
	// `task contracts` and TestContractDrift — neither of which exists here.
	// The artifacts need no rendering: the test bootstraps openapi.json (and
	// the client) on its first run and gates them from the second.
	"contract_test.go",
	"messages/en.toml",
	"messages/th.toml",
	"web/playwright.config.ts",
	"web/e2e/golden.spec.ts",
	// The other half of `bun run check`. Both scripts treat a missing
	// allowlist as an empty one, so only api-usage-allow.txt rides along:
	// i18n-allow.txt's bytes are the SCAFFOLD's own placeholder copy, and
	// writing three dead entries into a living product would be noise the
	// checker then reports back.
	"web/scripts/check-i18n.ts",
	"web/scripts/check-api-usage.ts",
	"web/api-usage-allow.txt",
}

// initEntry is one planned file: the template that renders it and where it
// lands, in slash form (doctrineFiles' spelling, and the report's).
type initEntry struct{ tmpl, out string }

// initPlan is the doctrine set for a detected shape, in doctrineFiles' order.
// Some files are unconditional in the scaffold only because `ultra new` always
// wires the preset behind them; a product that does not wire it is not missing
// them, so wired gates them here.
func initPlan(d scaffoldData, wired map[string]*wiredUse) []initEntry {
	byOut := map[string]string{}
	for tmpl, out := range scaffoldFiles(d) {
		byOut[out] = tmpl
	}
	var plan []initEntry
	for _, out := range doctrineFiles {
		tmpl, offered := byOut[out]
		switch {
		case !offered:
			continue
		// The catalogs: nothing reads them without i18n, and TestWiring never asks.
		case strings.HasPrefix(out, "messages/") && wired["i18n"] == nil:
			continue
		// The contract gate: `openapi` and `client` are contrib/api's verbs, so
		// a product with no api.Use() has no document to drift from, and the
		// test would fail on the first run rather than bootstrap.
		case out == "contract_test.go" && wired["api"] == nil:
			continue
		}
		plan = append(plan, initEntry{tmpl, out})
	}
	return plan
}

// initShape recovers the capability flags `ultra new` took as arguments from
// what is actually on disk — the tree is the only record a living product
// keeps of how it was born.
func initShape(dir, module string) (d scaffoldData, wired map[string]*wiredUse, err error) {
	d = scaffoldData{
		Name:      path.Base(module),
		Module:    module,
		Version:   resolveVersion(""),
		GoVersion: scaffoldGoVersion,
	}
	isDir := func(rel string) bool {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		return err == nil && fi.IsDir()
	}
	d.Web, d.DB = isDir("web"), isDir("internal/db")
	wired, err = scanWired(dir)
	if err != nil {
		return d, nil, err
	}
	d.Auth = wired["auth"] != nil
	if d.Web {
		// The design system is a dependency, not a flag anything else records:
		// the @connected scope in the frontend's manifest IS the answer.
		pkg, _ := os.ReadFile(filepath.Join(dir, "web", "package.json"))
		d.DS = dsBare
		if bytes.Contains(pkg, []byte("@connected/")) {
			d.DS = dsConnected
		}
	}
	return d, wired, nil
}

// missingDoctrine names every file `ultra init` would add to this product.
// `ultra upgrade` calls it to say so once — a bump is exactly when somebody is
// looking at what the framework learned since, and the contract gate is the
// sharpest case of all: the AGENTS.md a bump regenerates cites `task contracts`
// and TestContractDrift as the covenant, and a product born before
// contract_test.go can satisfy neither. A tree init cannot read is no nudge
// rather than a wrong one.
func missingDoctrine(dir string) []string {
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil
	}
	d, wired, err := initShape(dir, moduleNameOf(string(gomod)))
	if err != nil {
		return nil
	}
	var missing []string
	for _, e := range initPlan(d, wired) {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(e.out))); err != nil {
			missing = append(missing, e.out)
		}
	}
	return missing
}

func cmdInit(args []string, out, errW io.Writer) int {
	dir, force, dry := ".", "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry", a == "--dry-run":
			dry = true
		case a == "--force":
			if i+1 >= len(args) {
				fmt.Fprintln(errW, "ultra init: --force needs a file (e.g. --force AGENTS.md)")
				return 2
			}
			force = args[i+1]
			i++
		case strings.HasPrefix(a, "--force="):
			force = strings.TrimPrefix(a, "--force=")
		case strings.HasPrefix(a, "-"):
			node := ultraTree().find("init")
			fmt.Fprintf(errW, "Error: unknown flag %q for %q\n\n", a, node.path())
			node.help(errW)
			return 2
		default:
			dir = a
		}
	}

	// The same gate `ultra upgrade` keeps, in the same voice: this verb edits
	// a product, and "a product" means a module that requires the framework.
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		fmt.Fprintf(errW, "ultra init: no go.mod in %s — run it from inside a product module.\n"+
			"a product that does not exist yet is ultra new <name>; this verb retrofits one that does\n",
			displayDir(dir))
		failVerdict(errW, "init", "not a product module")
		return 2
	}
	module := moduleNameOf(string(gomod))
	if len(modPins(string(gomod), upgradeModules)) == 0 {
		fmt.Fprintf(errW, "ultra init: %s does not require %s — nothing to initialize\n",
			filepath.Join(dir, "go.mod"), frameworkModule)
		failVerdict(errW, "init", "this module does not require the framework")
		return 1
	}

	d, wired, err := initShape(dir, module)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "init", err.Error())
		return 1
	}
	plan := initPlan(d, wired)

	if force != "" {
		want := filepath.ToSlash(filepath.Clean(force))
		i := slices.IndexFunc(plan, func(e initEntry) bool { return e.out == want })
		if i < 0 {
			fmt.Fprintf(errW, "ultra init: --force %q is not one of the files init owns in this product:\n", force)
			for _, e := range plan {
				fmt.Fprintf(errW, "  %s\n", e.out)
			}
			failVerdict(errW, "init", "--force names a file init does not own")
			return 2
		}
		return initForce(dir, d, plan[i], dry, out, errW)
	}

	col := colorFor(out)
	// The shape is printed BEFORE the files, because every gated line under it
	// follows from it: a reader who disagrees with the plan is really
	// disagreeing with the detection, and this is where they see it.
	caps := strings.TrimPrefix(capsSuffix(d), ", ")
	// The two presets that gate a file but are not scaffold FLAGS, so
	// capsSuffix has never heard of them.
	for _, p := range []string{"i18n", "api"} {
		switch {
		case wired[p] == nil:
		case caps == "bare":
			caps = p
		default:
			caps += "+" + p
		}
	}
	if d.Web {
		caps += ", ds " + d.DS
	}
	fmt.Fprintf(out, "ultra init — %s\n\n  shape      %s\n\n", module, caps)

	todo := map[string]string{}
	t := newTable(out)
	for _, e := range plan {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(e.out))); err == nil {
			t.row("  "+e.out, col.dim("kept (exists)"))
			continue
		}
		todo[e.tmpl] = e.out
		t.row("  "+e.out, col.green(map[bool]string{true: "would write", false: "written"}[dry]))
	}
	t.flush()
	kept := len(plan) - len(todo)
	initWiring(dir, d, plan, out)

	if dry {
		fmt.Fprintf(out, "\n  --dry      nothing was written.\n")
		verdict(errW, "init", fmt.Sprintf("--dry: %d would be written, %d kept, nothing written", len(todo), kept))
		return 0
	}
	if err := renderAll(dir, todo, d); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "init", err.Error())
		return 1
	}
	// The doctrine AGENTS.md routes to is generated, never scaffolded, so a
	// freshly retrofitted product has a router pointing at nothing.
	if len(todo) > 0 {
		if _, err := os.Stat(filepath.Join(dir, skillDest, skillManifest)); err != nil {
			fmt.Fprintf(out, "\n  the doctrine these route to is not vendored here yet:\n    ultra skill install\n")
		}
	}
	verdict(errW, "init", fmt.Sprintf("%d written, %d kept", len(todo), kept))
	return 0
}

// initWiring names the wiring init CANNOT do.
//
// contract_test.go and the check scripts are FILES, and init writes files. The
// commands that RUN them are lines inside Taskfile.yml and web/package.json —
// two files a product hand-edits, whose contents are nobody's business but its
// own. A marked section would be a foreign body in a hand-owned file; a merge
// would break the promise that makes this verb safe to run twice. So init does
// what the rest of this CLI does when it cannot act safely: refuses, and prints
// the exact bytes. Shape-aware, and silent when the wiring is already there.
func initWiring(dir string, d scaffoldData, plan []initEntry, out io.Writer) {
	owns := func(rel string) bool {
		return slices.ContainsFunc(plan, func(e initEntry) bool { return e.out == rel })
	}
	col := colorFor(out)
	if task, _ := os.ReadFile(filepath.Join(dir, "Taskfile.yml")); owns("contract_test.go") &&
		!strings.Contains(string(task), "contracts:") {
		client := ""
		if d.Web {
			client = "\n      - go run . client --out web/src/lib/api"
		}
		fmt.Fprintf(out, "\n  %s       contract_test.go points at `task contracts`, and there is no such\n"+
			"             task in Taskfile.yml. That file is yours — init will not rewrite\n"+
			"             it. Add:\n\n"+
			"  contracts:\n"+
			"    desc: Refresh the COMMITTED derived artifacts — the fix TestContractDrift asks for\n"+
			"    cmds:\n"+
			"      - go run . openapi > openapi.json%s\n", col.yellow("note"), client)
	}
	if pkg, err := os.ReadFile(filepath.Join(dir, "web", "package.json")); err == nil &&
		owns("web/scripts/check-api-usage.ts") && !strings.Contains(string(pkg), "check-api-usage.ts") {
		fmt.Fprintf(out, "\n  %s       web/package.json is yours too, and its \"check\" script does not\n"+
			"             run the scripts above. Append to it:\n\n"+
			"    && bun scripts/check-i18n.ts && bun scripts/check-api-usage.ts\n", col.yellow("note"))
	}
}

// initForce regenerates ONE file that already exists. The diff comes first and
// is the whole point: the default never touches a file precisely because the
// bytes in it might be somebody's, and --force is only honest if it shows what
// it is about to lose.
func initForce(dir string, d scaffoldData, e initEntry, dry bool, out, errW io.Writer) int {
	before, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e.out)))
	rendered, err := renderTemplate(e.tmpl, d)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "init", err.Error())
		return 1
	}
	if string(before) == rendered {
		fmt.Fprintf(out, "  %s is already exactly what the current template renders\n", e.out)
		verdict(errW, "init --force "+e.out, "already current, nothing written")
		return 0
	}
	fmt.Fprintf(out, "\n--- %s\n+++ %s   (the current scaffold)\n", e.out, e.out)
	fmt.Fprint(out, hunk(colorFor(out), string(before), rendered))
	if dry {
		fmt.Fprintf(out, "\n  --dry      nothing was written.\n")
		verdict(errW, "init --force "+e.out, "--dry: the diff above, nothing written")
		return 0
	}
	if err := renderAll(dir, map[string]string{e.tmpl: e.out}, d); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "init", err.Error())
		return 1
	}
	verdict(errW, "init --force "+e.out, "regenerated — nothing was committed, read the diff")
	return 0
}
