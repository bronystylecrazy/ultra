package main

// `ultra contrib` is the capability manager for ONE product: what is wired
// today, what the platform offers, and one command to wire another preset.
//
//	ultra contrib list [dir]              WIRED + AVAILABLE
//	ultra contrib add <preset> [dir]      the AST edit + the refresh chain
//	ultra contrib remove <preset> [dir]   the inverse, behind a dependency guard
//
// Detection is IMPORT-BASED: a product wires contrib/pg iff some file of it
// imports github.com/bronystylecrazy/ultrastack/contrib/pg. That is sound
// (the import is the only way to spell the preset) and needs no build — the
// whole command answers from source in milliseconds, which is what makes it
// usable mid-edit. `ultra fleet profiles` answers the same question the other
// way, from each product's runtime `graph --json`: authoritative about what
// actually REGISTERED, but it has to build and boot. Different tools for
// different questions; this one is the fast one.
//
// `add` and `remove` edit the canonical root and nothing else. When the root
// is not canonical they refuse and print the line to type by hand — the
// doctrine is worth more than a clever edit, and a wrong guess about someone's
// assembly is worse than no edit at all.
//
// `remove` additionally refuses while something still needs the preset: a
// product package that imports it, or another wired preset that consumes what
// it provides. `--force` is the escape hatch, and it is honest about the
// consequence — it removes, then builds, and keeps the edit either way.

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// contribModule is where the presets live — one module, one tag, off the root.
const contribModule = frameworkModule + "/contrib"

// preset is one row of the curated catalog below.
type preset struct {
	Pkg     string   // package name == the last path element == the CLI name
	Entry   string   // the exact call `add` inserts
	Doc     string   // one line, the capability in the product's words
	Section string   // the config section it binds, "" when it binds none
	Infra   bool     // it wants a dev service (a compose entry)
	Pairs   []string // presets it almost always travels with
}

// presets is the catalog: every contrib package whose canonical entry is
// `Use()` — the thing an assembly names. It is a hand-curated static table
// rather than a directory listing, because a listing cannot tell a preset from
// its neighbours, and the difference is the whole product of this command:
//
//   - contrib/ultra, contrib/fib and stack are product ROOTS (ultra.New,
//     fib.Product, stack.Product) — the call `add` edits, never an argument
//     it inserts.
//   - contrib/testkit is a test helper (testkit.Product(t, …)), not wiring.
//   - contrib/dicert and contrib/internal are internals with no Use().
//
// The pin is kept honest by TestPresetCatalogMatchesContrib, which walks
// contrib/ and fails when a package grows or loses a `func Use(`.
var presets = []preset{
	{Pkg: "api", Entry: `api.Use(api.Info{Title: "app", Version: "0.1.0"})`,
		Doc: "the typed HTTP edge: OpenAPI + TS client generators"},
	{Pkg: "audit", Entry: "audit.Use()", Section: "audit",
		Doc: "durable audit trail in Postgres (supersedes auth's log-only sink)", Pairs: []string{"pg", "migrate"}},
	{Pkg: "auth", Entry: "auth.Use()", Section: "auth",
		Doc: "identity: users, API keys, JWT/session strategies, route enforcement"},
	{Pkg: "authpg", Entry: "authpg.Use()",
		Doc: "durable auth stores in Postgres (users, keys, sessions)", Pairs: []string{"auth", "pg", "migrate"}},
	{Pkg: "cache", Entry: "cache.Use()",
		Doc: "in-process typed caches; redis.Caches() overrides the engine"},
	{Pkg: "conf", Entry: "conf.Use()",
		Doc: "the config file + env binding ([section] to a typed Config)"},
	{Pkg: "console", Entry: "console.Use()",
		Doc: "the embedded ops console at /ops/console (health, boot, preset cards)"},
	{Pkg: "i18n", Entry: "i18n.Use()", Section: "i18n",
		Doc: "locale catalogs + the request-locale ladder (codes on the wire)", Pairs: []string{"conf"}},
	{Pkg: "inference", Entry: "inference.Use()", Section: "inference",
		Doc: "the gRPC inference client"},
	{Pkg: "jobs", Entry: "jobs.Use()", Section: "jobs",
		Doc: "durable background jobs + cron"},
	{Pkg: "migrate", Entry: "migrate.Use()",
		Doc: "goose migrations, applied before anything serves", Pairs: []string{"pg"}},
	{Pkg: "mqtt", Entry: "mqtt.Use()", Section: "mqtt", Infra: true,
		Doc: "the MQTT broker; devices authenticate against contrib/auth", Pairs: []string{"auth"}},
	{Pkg: "notify", Entry: "notify.Use()", Section: "notify",
		Doc: "notification routes over email/Line/Telegram channels"},
	{Pkg: "otel", Entry: "otel.Use()", Section: "otel",
		Doc: "OpenTelemetry traces + metrics exporters"},
	{Pkg: "pg", Entry: "pg.Use()", Section: "postgres", Infra: true,
		Doc: "the Postgres pool, dialed in Start", Pairs: []string{"migrate"}},
	{Pkg: "rate", Entry: "rate.Use()", Section: "rate",
		Doc: "identity-keyed rate limiting"},
	{Pkg: "redis", Entry: "redis.Use()", Section: "redis", Infra: true,
		Doc: "the Redis client; also backs cache + rate stores fleet-wide"},
	{Pkg: "report", Entry: "report.Use()",
		Doc: "rendered reports: define, enqueue, store", Pairs: []string{"jobs", "s3"}},
	{Pkg: "s3", Entry: "s3.Use()", Section: "s3", Infra: true,
		Doc: "S3/MinIO object storage"},
	{Pkg: "seed", Entry: "seed.Use()",
		Doc: "named, applied-once data seeds (schema is migrate)", Pairs: []string{"pg", "migrate"}},
	{Pkg: "ws", Entry: "ws.Use()", Section: "ws",
		Doc: "the realtime websocket hub"},
	{Pkg: "zlog", Entry: "zlog.Use()", Section: "log",
		Doc: "zerolog as the slog engine"},
}

func presetByName(name string) *preset {
	for i := range presets {
		if presets[i].Pkg == name {
			return &presets[i]
		}
	}
	return nil
}

func (p *preset) path() string { return contribModule + "/" + p.Pkg }

// entry is the call to insert. Only api needs shaping: its Use takes a
// required api.Info, so a bare api.Use() would not compile — the product's
// own name is a far better placeholder than "app".
func (p *preset) entry(product string) string {
	if p.Pkg == "api" && product != "" {
		return fmt.Sprintf(`api.Use(api.Info{Title: %q, Version: "0.1.0"})`, product)
	}
	return p.Entry
}

// presetDep is one preset-to-preset dependency the `remove` guard knows.
type presetDep struct {
	Dependent string // the wired preset that would break
	Needs     string // the preset being removed
	Why       string // what was READ in contrib to assert it
}

// presetDeps is the guard's evidence table. Every row was read out of the
// contrib source and the reason cites the line that proves it — a pair
// asserted on a hunch would produce a false refusal, which is worse than no
// guard at all. Imports alone do NOT qualify: contrib/report imports jobs and
// s3 but takes both as di.Optional, and contrib/rate imports redis only for
// the override seam. Neither is here, because neither breaks.
//
// The [section] → conf dependency is not listed: it is DERIVED from the
// catalog (conf.Section[T] provides T out of *conf.File), so it can never
// drift from the Section column.
var presetDeps = []presetDep{
	{"migrate", "pg", "migrate.NewRunner(db *pg.DB, …) — *pg.DB goes unprovided (DI0001 at Validate)"},
	{"jobs", "pg", "jobs' store is newStore(db *pg.DB) — *pg.DB goes unprovided (DI0001)"},
	{"audit", "pg", "audit's store is newStore(db *pg.DB, …) — *pg.DB goes unprovided (DI0001)"},
	{"authpg", "pg", "authpg.NewUserStore(db *pg.DB) — *pg.DB goes unprovided (DI0001)"},
	{"mqtt", "auth", "the broker constructor takes *auth.Auth and *auth.Audit (DI0001)"},
	{"jobs", "migrate", `jobs contributes migrate.Files("jobs", …) — with no Runner its tables are never created`},
	{"audit", "migrate", `audit contributes migrate.Files("audit", …) — with no Runner ultra_audit is never created`},
	{"authpg", "migrate", `authpg contributes migrate.Files("authpg", …) — with no Runner its tables are never created`},
}

// contribNode is the help tree's contrib subtree — the ONE place this
// command's usage text lives, shared with `ultra contrib --help`.
func contribNode(sub string) *command {
	c := ultraTree().find("contrib")
	if s := c.find(sub); s != nil {
		return s
	}
	return c
}

// badFlag is the wrong-flag screen every contrib subcommand shows: cobra's
// wording, then that subcommand's own help.
func badFlag(errW io.Writer, node *command, flag string) int {
	fmt.Fprintf(errW, "Error: unknown flag %q for %q\n\n", flag, node.path())
	node.help(errW)
	return 2
}

func cmdContrib(args []string, out, errW io.Writer) int {
	if len(args) == 0 {
		contribNode("").help(errW)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return contribList(args[1:], out, errW)
	case "add":
		return contribAdd(args[1:], out, errW)
	case "remove", "rm":
		return contribRemove(args[1:], out, errW)
	}
	contribNode("").unknown(errW, args[0])
	return 2
}

// ---- list ----

func contribList(args []string, out, errW io.Writer) int {
	dir := "."
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return badFlag(errW, contribNode("list"), a)
		}
		dir = a
	}

	wired, err := scanWired(dir)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib list", err.Error())
		return 1
	}

	// The pin belongs in this header: "which presets" and "which version of
	// them" are the same question, and `ultra upgrade --check` is one line away.
	header := fmt.Sprintf("%s — capabilities under %s/", productName(dir), contribModule)
	if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		if ps := modPins(string(b), upgradeModules); len(ps) > 0 {
			header += " @ " + ps[0].Version
		}
	}
	fmt.Fprintln(out, header)
	if root, err := loadRoot(dir); err == nil {
		fmt.Fprintf(out, "root: %s at main.go:%d\n", root.Kind, root.Fset.Position(root.Call.Pos()).Line)
	} else {
		fmt.Fprintf(out, "root: not canonical — %v\n", err)
	}
	fmt.Fprintln(out)

	names := make([]string, 0, len(wired))
	for name := range wired {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(out, "WIRED (%d)\n", len(names))
	if len(names) == 0 {
		fmt.Fprintln(out, "  (nothing — this product imports no contrib preset)")
	}
	// tabwriter, not %-54s: an entry spelling as long as
	// api.Use(api.Info{Title: "…"}) used to shove the file:line column off the
	// end of the row it belonged to.
	t := newTable(out)
	for _, name := range names {
		w := wired[name]
		spelling := w.Call
		if spelling == "" {
			// The type is consumed somewhere but nothing registers it — the
			// shape of a DI0001 waiting at Validate, so say it plainly.
			spelling = "(imported; no " + name + ".Use() — not registered)"
		}
		t.row("  "+name, spelling, fmt.Sprintf("%s:%d", w.File, w.Line))
	}
	t.flush()

	var avail []preset
	for _, p := range presets {
		if wired[p.Pkg] == nil {
			avail = append(avail, p)
		}
	}
	fmt.Fprintf(out, "\nAVAILABLE (%d) — ultra contrib add <preset>\n", len(avail))
	t = newTable(out)
	for _, p := range avail {
		t.row("  "+p.Pkg, p.Doc+p.tags())
	}
	t.flush()
	verdict(errW, "contrib list", fmt.Sprintf("%s wired, %d available", count(len(names), "preset"), len(avail)))
	return 0
}

// tags renders the two facts that change what a product does AFTER the wiring
// line lands: a config section to fill in, and a dev service to run.
func (p *preset) tags() string {
	var t []string
	if p.Section != "" {
		t = append(t, "["+p.Section+"]")
	}
	if p.Infra {
		t = append(t, "dev infra")
	}
	if len(t) == 0 {
		return ""
	}
	return " (" + strings.Join(t, ", ") + ")"
}

// wiredUse is one detected capability: where the import is, and the entry
// spelling the product actually used (pg.Use(), api.Use(api.Info{…})).
type wiredUse struct {
	File string
	Line int
	Call string
}

// scanWired walks the product for imports of contrib presets. Files that do
// not parse are skipped rather than fatal: a product mid-edit still deserves
// an answer, and a broken file cannot hide a wiring the rest of the tree also
// shows.
func scanWired(dir string) (map[string]*wiredUse, error) {
	found := map[string]*wiredUse{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != dir && (strings.HasPrefix(name, ".") || name == "vendor" ||
				name == "node_modules" || name == "testdata" || name == "web") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		locals := map[string]string{} // local package name → preset
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			pkg, ok := strings.CutPrefix(p, contribModule+"/")
			if !ok || presetByName(pkg) == nil {
				continue // a subpackage (contrib/authpg/sqlcgen) is not a preset
			}
			local := pkg
			if imp.Name != nil {
				if imp.Name.Name == "_" || imp.Name.Name == "." {
					continue // a blank/dot import is not a wiring
				}
				local = imp.Name.Name
			}
			locals[local] = pkg
			if found[pkg] == nil {
				found[pkg] = &wiredUse{File: rel, Line: fset.Position(imp.Pos()).Line}
			}
		}
		if len(locals) == 0 {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Use" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			pkg, ok := locals[id.Name]
			if !ok {
				return true
			}
			if w := found[pkg]; w != nil && w.Call == "" {
				w.File, w.Line, w.Call = rel, fset.Position(call.Pos()).Line, oneLine(fset, call)
			}
			return true
		})
		return nil
	})
	return found, err
}

// ---- add ----

func contribAdd(args []string, out, errW io.Writer) int {
	name, dir, dry := "", "", false
	for _, a := range args {
		switch {
		case a == "--dry" || a == "-dry":
			dry = true
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, contribNode("add"), a)
		case name == "":
			name = a
		case dir == "":
			dir = a
		default:
			contribNode("add").help(errW)
			return 2
		}
	}
	if name == "" {
		contribNode("add").help(errW)
		return 2
	}
	if dir == "" {
		dir = "."
	}
	p := presetByName(name)
	if p == nil {
		fmt.Fprintf(errW, "unknown preset %q — `ultra contrib list` names every one\n", name)
		return 2
	}

	wired, err := scanWired(dir)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib add "+name, err.Error())
		return 1
	}

	root, err := loadRoot(dir)
	if err != nil {
		var refusal *rootRefusal
		if errors.As(err, &refusal) {
			// A registration already spelled somewhere means the product has
			// the capability; without a readable root we cannot say where.
			if w := wired[p.Pkg]; w != nil && w.Call != "" {
				fmt.Fprintf(out, "%s is already wired — %s at %s:%d\n", p.Pkg, w.Call, w.File, w.Line)
				verdict(errW, "contrib add "+p.Pkg, "already wired — nothing to do")
				return 0
			}
			fmt.Fprintf(errW, "ultra contrib add %s: %v\n\n", p.Pkg, err)
			fmt.Fprintf(errW, "The edit is one line, so do it by hand — add %s to your assembly\nand import %q.\n",
				p.entry(productName(dir)), p.path())
			failVerdict(errW, "contrib add "+p.Pkg, "refused: the root is not canonical")
			return 1
		}
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib add "+p.Pkg, err.Error())
		return 1
	}

	// The ASSEMBLY is the authority on "already wired", not the import graph:
	// a feature package importing contrib/pg for *pg.DB is precisely the
	// product that still needs pg.Use() — treating that import as a wiring
	// would refuse the one edit that fixes it.
	if have := root.presetArgs(p); len(have) > 0 {
		fmt.Fprintf(out, "%s is already wired — %s at main.go:%d\n",
			p.Pkg, oneLine(root.Fset, have[0]), root.Fset.Position(have[0].Pos()).Line)
		verdict(errW, "contrib add "+p.Pkg, "already wired — nothing to do")
		return 0
	}

	entry := p.entry(productName(dir))
	before, after, err := root.withPreset(p, entry)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib add "+p.Pkg, err.Error())
		return 1
	}

	if dry {
		fmt.Fprint(out, presetDiff(colorFor(out), root.File, "+ "+entry, before, after))
		fmt.Fprintf(out, "\n--dry: nothing written.\n")
		verdict(errW, "contrib add "+p.Pkg, "--dry: 1 edit planned, nothing written")
		return 0
	}

	if err := os.WriteFile(root.File, after, 0o644); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib add "+p.Pkg, err.Error())
		return 1
	}
	rel, _ := filepath.Rel(dir, root.File)
	fmt.Fprintf(out, "%s: added %s to %s (%s), imported %s\n", rel, entry, root.Kind, positionHint(root), p.path())
	fmt.Fprint(out, presetDiff(colorFor(out), root.File, "+ "+entry, before, after))

	if note := ensureContribRequire(dir); note != "" {
		fmt.Fprint(out, "\n"+note)
	}
	fmt.Fprint(out, "\n"+nextSteps(p, wired))
	verdict(errW, "contrib add "+p.Pkg, "wired into "+root.Kind+" ("+rel+")")
	return 0
}

// positionHint says WHERE the argument landed, because that is the one part of
// the edit a reader cannot infer from the command they typed.
func positionHint(root *productRoot) string {
	if root.Anchor != nil {
		return "before app.Modules()"
	}
	return "last argument"
}

// nextSteps is the refresh chain: what the platform itself will now tell you,
// in the order it will tell you. `ultra` never runs the product's binary —
// that is the product's business and its exit codes are its own.
func nextSteps(p *preset, wired map[string]*wiredUse) string {
	var b strings.Builder
	b.WriteString("NEXT STEPS\n")
	b.WriteString("  go test ./...                    the covenant names anything still missing\n")
	if p.Section != "" {
		fmt.Fprintf(&b, "  go run . config init --all       [%s] is new — write its section\n", p.Section)
	}
	if p.Infra {
		b.WriteString("  go run . infra compose --write   the dev service (if your framework version has it)\n")
	}
	var missing []string
	for _, pair := range p.Pairs {
		if wired[pair] == nil {
			missing = append(missing, pair)
		}
	}
	if len(missing) > 0 {
		spellings := make([]string, len(missing))
		for i, m := range missing {
			spellings[i] = m + ".Use()"
		}
		fmt.Fprintf(&b, "\n%s usually pairs with %s — add it too?\n  ultra contrib add %s\n",
			p.Pkg, strings.Join(spellings, " + "), strings.Join(missing, " "))
	}
	return b.String()
}

// ensureContribRequire adds the contrib module to go.mod when the product does
// not already have it (normally it does — the scaffold requires it). Failure
// is a note, never a rollback: the wiring line is correct either way, and
// `go test ./...` will say the rest.
func ensureContribRequire(dir string) string {
	modPath := filepath.Join(dir, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		return "" // no module here: not ours to fix
	}
	if strings.Contains(string(mod), contribModule) {
		return ""
	}
	target := contribModule
	if v := requiredVersion(string(mod), frameworkModule); v != "" {
		target += "@" + v
	}
	cmd := exec.Command("go", "get", target)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOPRIVATE="+privateGlob)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("note: `go get %s` did not finish (%v) — run it before `go test ./...`\n%s\n",
			target, err, strings.TrimSpace(string(out)))
	}
	return "go get " + target + "\n"
}

// requiredVersion reads the version a go.mod pins for a module — plain text,
// because the CLI does not depend on golang.org/x/mod.
func requiredVersion(gomod, module string) string {
	for _, line := range strings.Split(gomod, "\n") {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), " // indirect"))
		if len(f) >= 2 && f[0] == "require" && f[1] == module && len(f) >= 3 {
			return f[2]
		}
		if len(f) == 2 && f[0] == module {
			return f[1]
		}
	}
	return ""
}

// productName is the product's own name — the module's last path element,
// falling back to the directory's. It titles a generated api.Info.
func productName(dir string) string {
	if mod, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		for _, line := range strings.Split(string(mod), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
				return filepath.Base(f[1])
			}
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return filepath.Base(abs)
}

// ---- remove ----

// goBuild is the verification `--force` runs after an unguarded removal. A var
// so the tests can drive the forced path without a resolvable module.
var goBuild = func(dir string) (string, error) { return runIn(dir, "go", "build", "./...") }

func contribRemove(args []string, out, errW io.Writer) int {
	name, dir, dry, force := "", "", false, false
	for _, a := range args {
		switch {
		case a == "--dry" || a == "-dry":
			dry = true
		case a == "--force" || a == "-force":
			force = true
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, contribNode("remove"), a)
		case name == "":
			name = a
		case dir == "":
			dir = a
		default:
			contribNode("remove").help(errW)
			return 2
		}
	}
	if name == "" {
		contribNode("remove").help(errW)
		return 2
	}
	if dir == "" {
		dir = "."
	}
	p := presetByName(name)
	if p == nil {
		fmt.Fprintf(errW, "unknown preset %q — `ultra contrib list` names every one\n", name)
		return 2
	}

	wired, err := scanWired(dir)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib remove "+name, err.Error())
		return 1
	}
	if wired[p.Pkg] == nil {
		fmt.Fprintf(out, "%s is not wired — nothing to remove\n", p.Pkg)
		verdict(errW, "contrib remove "+p.Pkg, "not wired — nothing to remove")
		return 0
	}

	root, err := loadRoot(dir)
	if err != nil {
		var refusal *rootRefusal
		if errors.As(err, &refusal) {
			fmt.Fprintf(errW, "ultra contrib remove %s: %v\n\n", p.Pkg, err)
			fmt.Fprintf(errW, "Remove it by hand: drop every %s.* argument from your assembly\nand the %q import.\n", p.Pkg, p.path())
			failVerdict(errW, "contrib remove "+p.Pkg, "refused: the root is not canonical")
			return 1
		}
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib remove "+p.Pkg, err.Error())
		return 1
	}

	victims := root.presetArgs(p)
	if len(victims) == 0 {
		w := wired[p.Pkg]
		fmt.Fprintf(errW, "%s is imported (%s:%d) but %s holds no %s.* argument — nothing this command can remove\n",
			p.Pkg, w.File, w.Line, root.Kind, p.Pkg)
		failVerdict(errW, "contrib remove "+p.Pkg, "imported, but the assembly holds no argument to drop")
		return 1
	}

	col := colorFor(out)
	fmt.Fprintf(out, "%s: %d argument(s) in %s\n", p.Pkg, len(victims), root.Kind)
	for _, v := range victims {
		fmt.Fprintf(out, "  %s   %s\n", col.red("- "+oneLine(root.Fset, v)),
			col.dim(fmt.Sprintf("(main.go:%d)", root.Fset.Position(v.Pos()).Line)))
	}

	findings := removalFindings(dir, root, p, wired)
	if len(findings) > 0 {
		fmt.Fprintf(out, "\nDEPENDENCY GUARD (%d)\n", len(findings))
		for _, f := range findings {
			fmt.Fprintf(out, "  %s\n", f)
		}
	}

	if dry {
		switch {
		case len(findings) > 0 && !force:
			fmt.Fprintf(out, "\n--dry: the guard would REFUSE this removal — `--force` proceeds anyway.\n")
			verdict(errW, "contrib remove "+p.Pkg, "--dry: the guard would refuse ("+count(len(findings), "finding")+")")
		default:
			fmt.Fprintf(out, "\n--dry: nothing written.\n")
			verdict(errW, "contrib remove "+p.Pkg, "--dry: "+count(len(victims), "argument")+" would go, nothing written")
		}
		return 0
	}
	if len(findings) > 0 && !force {
		fmt.Fprintf(errW, "\nrefusing to remove %s: the findings above break when it goes.\n"+
			"Fix them first, or `ultra contrib remove %s --force` to remove it anyway\n"+
			"(the build runs afterwards and the edit is kept either way).\n", p.Pkg, p.Pkg)
		failVerdict(errW, "contrib remove "+p.Pkg, "refused by the dependency guard ("+count(len(findings), "finding")+")")
		return 1
	}

	before, after, err := root.withoutPreset(p, victims)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib remove "+p.Pkg, err.Error())
		return 1
	}
	if err := os.WriteFile(root.File, after, 0o644); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "contrib remove "+p.Pkg, err.Error())
		return 1
	}
	fmt.Fprint(out, presetDiff(colorFor(out), root.File, "- "+p.Pkg+".*", before, after))
	if bytes.Contains(after, []byte(strconv.Quote(p.path()))) {
		fmt.Fprintf(out, "\nkept the %s import: main.go still references the package.\n", p.Pkg)
	} else {
		fmt.Fprintf(out, "\ndropped the %s import: main.go no longer references it.\n", p.path())
	}

	code := 0
	if force {
		if buildOut, err := goBuild(dir); err != nil {
			fmt.Fprintf(errW, "\ngo build ./... failed after the forced removal — the edit is KEPT, as you asked:\n%s\n",
				headLines(buildOut, 20))
			fmt.Fprintf(errW, "`go test ./...` (the covenant) will name every provider that is now missing.\n")
			code = 1
		} else {
			fmt.Fprint(out, "\ngo build ./... ok\n")
		}
	}
	fmt.Fprint(out, "\n"+removalSteps(p))
	if code == 0 {
		verdict(errW, "contrib remove "+p.Pkg, "unwired "+count(len(victims), "argument"))
	} else {
		failVerdict(errW, "contrib remove "+p.Pkg, "unwired, but go build failed — the edit is kept")
	}
	return code
}

// removalFindings is the guard: who breaks when this preset goes. Two sources,
// both read from source and neither a guess — the product's own packages, and
// the presets the assembly still wires.
func removalFindings(dir string, root *productRoot, p *preset, wired map[string]*wiredUse) []string {
	var out []string
	for _, pkgDir := range productImporters(dir, root.File, p.path()) {
		out = append(out, fmt.Sprintf("%s imports contrib/%s — removing %s leaves what it consumes unprovided (DI0001 at Validate)",
			pkgDir, p.Pkg, p.entry("")))
	}
	for _, d := range presetDeps {
		if d.Needs != p.Pkg || wired[d.Dependent] == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s is wired and needs %s: %s", d.Dependent, p.Pkg, d.Why))
	}
	// Derived, so it cannot drift: a section is decoded out of conf's *File.
	if p.Pkg == "conf" {
		for _, other := range presets {
			if other.Section == "" || wired[other.Pkg] == nil {
				continue
			}
			out = append(out, fmt.Sprintf("%s is wired and needs conf: it binds [%s] with conf.Section, which decodes out of *conf.File (DI0001)",
				other.Pkg, other.Section))
		}
	}
	return out
}

// productImporters lists the product's own package directories that import
// path — every file except the root itself, because the root is what is being
// edited.
func productImporters(dir, rootFile, path string) []string {
	seen := map[string]bool{}
	var out []string
	fset := token.NewFileSet()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != dir && (strings.HasPrefix(name, ".") || name == "vendor" ||
				name == "node_modules" || name == "testdata" || name == "web") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || sameFile(p, rootFile) {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if perr != nil {
			return nil
		}
		for _, imp := range f.Imports {
			if q, err := strconv.Unquote(imp.Path.Value); err == nil && q == path {
				rel, _ := filepath.Rel(dir, filepath.Dir(p))
				if rel == "." {
					rel = filepath.Base(p)
				}
				if !seen[rel] {
					seen[rel], out = true, append(out, rel)
				}
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func sameFile(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && aa == bb
}

// removalSteps is the other half of the refresh chain. The config section is
// first because it is the one that FAILS BOOT: conf's unused-section guard
// rejects a [section] nothing binds, so a removal that stops at main.go leaves
// a product that will not start.
func removalSteps(p *preset) string {
	var b strings.Builder
	b.WriteString("NEXT STEPS\n")
	if p.Section != "" {
		fmt.Fprintf(&b, "  config.toml: delete [%s] — conf's unused-section guard FAILS BOOT on a\n"+
			"               section nothing binds any more (it names the section, with a\n"+
			"               did-you-mean); config.toml.example and any deploy config too\n", p.Section)
	}
	if p.Infra {
		b.WriteString("  go run . infra compose --write   drop the dev service (if available in your framework version)\n")
	}
	b.WriteString("  go build ./...                   a //go:embed var or helper only this preset used is now unused\n")
	b.WriteString("  go test ./...                    the covenant names every provider now missing\n")
	return b.String()
}

func headLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… (%d more lines)", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

// presetArgs is every argument of the bundle call that belongs to the preset:
// pg.Use(), but equally auth.JWT(), auth.Protect (a value, not a call) and
// migrate.Files(…). The test is the qualifier at the base of the expression,
// so a generic instantiation and a call chain are recognized the same way.
func (r *productRoot) presetArgs(p *preset) []ast.Expr {
	locals := importLocals(r.Ast)
	var out []ast.Expr
	for _, a := range r.Call.Args {
		if id := baseQualifier(a); id != nil && locals[id.Name] == p.path() {
			out = append(out, a)
		}
	}
	return out
}

// baseQualifier peels calls, generic instantiations and parens off an
// expression and returns the package identifier it is rooted at, if any.
func baseQualifier(e ast.Expr) *ast.Ident {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.CallExpr:
			e = x.Fun
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.SelectorExpr:
			id, _ := x.X.(*ast.Ident)
			return id
		default:
			return nil
		}
	}
}

// withoutPreset deletes the arguments, then the import when nothing in the
// rewritten main.go references the package any more. Two passes on purpose:
// whether the import survives is a question about the file AFTER the
// arguments are gone, and the cheapest correct way to ask it is to look.
func (r *productRoot) withoutPreset(p *preset, victims []ast.Expr) (before, after []byte, err error) {
	if before, err = format.Source(r.Src); err != nil {
		return nil, nil, err
	}

	cuts := make([][2]int, 0, len(victims))
	for _, v := range victims {
		cuts = append(cuts, r.argCut(v))
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i][0] > cuts[j][0] })
	edited := r.Src
	for _, c := range cuts {
		edited = append(append([]byte{}, edited[:c[0]]...), edited[c[1]:]...)
	}
	edited, err = format.Source(edited)
	if err != nil {
		return nil, nil, fmt.Errorf("the edited main.go does not parse (%w) — nothing written", err)
	}
	edited = tidyAssembly(edited)

	if trimmed, ok := dropImport(edited, p); ok {
		edited = trimmed
	}
	return before, edited, nil
}

// tidyAssembly clears the blank lines a removal strands inside the assembly:
// the one left hanging under the opening paren, the one above the closing
// paren, and any doubled pair where a group used to be. gofmt preserves
// vertical whitespace by design, so this is the one thing it will not do for
// us — and an assembly that opens on an empty line reads like a mistake.
func tidyAssembly(src []byte) []byte {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return src
	}
	lo, hi := -1, -1
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if n.Name == "App" {
					lo = fset.Position(gd.Pos()).Line
					hi = fset.Position(gd.End()).Line
				}
			}
		}
	}
	if lo < 0 {
		return src
	}
	lines := strings.Split(string(src), "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		n := i + 1 // 1-based, matching token.Position
		if strings.TrimSpace(line) == "" && n > lo && n <= hi {
			prev := ""
			if len(out) > 0 {
				prev = strings.TrimSpace(out[len(out)-1])
			}
			next := ""
			if i+1 < len(lines) {
				next = strings.TrimSpace(lines[i+1])
			}
			if prev == "" || strings.HasSuffix(prev, "(") || strings.HasPrefix(next, ")") {
				continue
			}
		}
		out = append(out, line)
	}
	tidied := strings.Join(out, "\n")
	if formatted, err := format.Source([]byte(tidied)); err == nil {
		return formatted
	}
	return src
}

// argCut is the byte range one argument occupies: the argument, its trailing
// comma and end-of-line comment, and — when it owns its lines — the doc
// comment above it and the newline below, so removing it leaves no blank hole.
func (r *productRoot) argCut(arg ast.Expr) [2]int {
	pos := func(p token.Pos) int { return r.Fset.Position(p).Offset }
	start, end, rparen := pos(arg.Pos()), pos(arg.End()), pos(r.Call.Rparen)
	if doc := docBefore(r.Ast, r.Fset, arg, r.prevEnd(arg)); doc != nil {
		start = pos(doc.Pos())
	}
	src := r.Src

	j := end
	for j < rparen && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j < rparen && src[j] == ',' {
		j++
	}
	k := j
	for k < rparen && (src[k] == ' ' || src[k] == '\t') {
		k++
	}
	if k+1 < rparen && src[k] == '/' && (src[k+1] == '/' || src[k+1] == '*') {
		if nl := bytes.IndexByte(src[k:rparen], '\n'); nl >= 0 {
			j = k + nl
		} else {
			j = rparen
		}
	}
	// Whole lines: swallow the indentation before and the newline after, so a
	// removed argument does not leave an empty line behind.
	if ls := lineStart(src, start); blank(src[ls:start]) {
		start = ls
		if nl := bytes.IndexByte(src[j:], '\n'); nl >= 0 && blank(src[j:j+nl]) {
			j += nl + 1
		}
	}
	return [2]int{start, j}
}

func blank(s []byte) bool {
	for _, c := range s {
		if c != ' ' && c != '\t' && c != '\r' {
			return false
		}
	}
	return true
}

// dropImport removes the preset's import when the rewritten file no longer
// mentions the package. "Mentions" is any qualified reference to the local
// name — main.go may still spell pg.Config in a helper, and an import that is
// still used is not ours to delete.
func dropImport(src []byte, p *preset) ([]byte, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	var spec *ast.ImportSpec
	local := p.Pkg
	for _, imp := range f.Imports {
		q, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil || q != p.path() {
			continue
		}
		spec = imp
		if imp.Name != nil {
			local = imp.Name.Name
		}
	}
	if spec == nil {
		return nil, false
	}
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
			used = true
		}
		return !used
	})
	if used {
		return nil, false
	}
	off := func(pos token.Pos) int { return fset.Position(pos).Offset }
	start, end := off(spec.Pos()), off(spec.End())
	if ls := lineStart(src, start); blank(src[ls:start]) {
		start = ls
		if nl := bytes.IndexByte(src[end:], '\n'); nl >= 0 && blank(src[end:end+nl]) {
			end += nl + 1
		}
	}
	out := append(append([]byte{}, src[:start]...), src[end:]...)
	formatted, err := format.Source(out)
	if err != nil {
		return nil, false
	}
	return formatted, true
}

// ---- the canonical root ----

// bundles are the assembly calls `add` knows how to edit, in the order it
// prefers them. The order is a containment order, not a ranking:
// di.Options(ultra.New(…)) is the scaffold's own shape, and the preset belongs
// INSIDE the bundle that gives it its neighbours, never beside it.
var bundles = []struct{ Path, Name string }{
	{contribModule + "/ultra", "New"},
	{contribModule + "/fib", "Product"},
	{frameworkModule + "/stack", "Product"},
	{frameworkModule + "/di", "Options"},
}

// rootRefusal is a root this command declines to edit — a SHAPE problem, not
// an IO one. It is the difference between "I could not read main.go" and "your
// root is not the canonical one"; only the second earns the manual one-liner.
type rootRefusal struct{ reason string }

func (e *rootRefusal) Error() string { return e.reason }

type productRoot struct {
	Dir    string
	File   string // <dir>/main.go
	Src    []byte
	Fset   *token.FileSet
	Ast    *ast.File
	Call   *ast.CallExpr // the bundle call to edit
	Kind   string        // as written: "ultra.New", "di.Options", …
	Anchor ast.Node      // the app.Modules() argument to insert before, or nil
}

// loadRoot resolves the canonical root: `var App = …` in package main, assigned
// exactly once, holding a bundle call. The rules are the analyzer's (see
// analyzer/ultravet/extract.go summarizeVar): a var with one initializer and no
// other writer in the package is the only one that can be resolved without
// guessing. `add` holds itself to the same bar the vet does — a root that
// ultravet cannot read is a root this command will not edit.
func loadRoot(dir string) (*productRoot, error) {
	file := filepath.Join(dir, "main.go")
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("no main.go in %s — run `ultra contrib` from the product root", dir)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	if f.Name.Name != "main" {
		return nil, &rootRefusal{fmt.Sprintf("main.go declares package %s, not main", f.Name.Name)}
	}

	var init ast.Expr
	n := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				if name.Name == "App" {
					init, n = vs.Values[i], n+1
				}
			}
		}
	}
	switch {
	case n == 0:
		return nil, &rootRefusal{"no `var App = …` in main.go — the root is the whole product as one value, assigned once"}
	case n > 1:
		return nil, &rootRefusal{"main.go declares App more than once"}
	}
	if where := appReassigned(dir); where != "" {
		return nil, &rootRefusal{"App is reassigned at " + where + " — a root with a second writer is not resolvable"}
	}

	root := &productRoot{Dir: dir, File: file, Src: src, Fset: fset, Ast: f}
	if err := root.findBundle(init); err != nil {
		return nil, err
	}
	return root, nil
}

// appReassigned reports the first `App = …` in any package-main file of the
// product directory — the writer that makes the initializer an unsafe answer.
func appReassigned(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil || f.Name.Name != "main" {
			continue
		}
		where := ""
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || where != "" {
				return where == ""
			}
			for _, lhs := range as.Lhs {
				if id, ok := ast.Unparen(lhs).(*ast.Ident); ok && id.Name == "App" {
					pos := fset.Position(id.Pos())
					where = fmt.Sprintf("%s:%d", e.Name(), pos.Line)
				}
			}
			return where == ""
		})
		if where != "" {
			return where
		}
	}
	return ""
}

// findBundle picks the call to edit out of the root expression: the innermost
// call of the most-preferred bundle kind present. Two candidates at the same
// depth is an ambiguity, and an ambiguous assembly is refused rather than
// guessed at.
func (r *productRoot) findBundle(init ast.Expr) error {
	locals := importLocals(r.Ast)
	type cand struct {
		call  *ast.CallExpr
		kind  string
		pref  int
		depth int
	}
	var cands []cand
	depth := 0
	ast.Inspect(init, func(n ast.Node) bool {
		if n == nil {
			depth--
			return true
		}
		depth++
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		for pref, b := range bundles {
			if locals[id.Name] == b.Path && sel.Sel.Name == b.Name {
				cands = append(cands, cand{call, id.Name + "." + sel.Sel.Name, pref, depth})
			}
		}
		return true
	})
	if len(cands) == 0 {
		return &rootRefusal{"`var App` holds no known bundle call (ultra.New, fib.Product, stack.Product, di.Options)"}
	}
	best := cands[0]
	ties := 0
	for _, c := range cands {
		switch {
		case c.pref < best.pref, c.pref == best.pref && c.depth > best.depth:
			best, ties = c, 0
		case c.pref == best.pref && c.depth == best.depth && c.call != best.call:
			ties++
		}
	}
	if ties > 0 {
		return &rootRefusal{fmt.Sprintf("`var App` holds %d %s calls at the same depth — which one owns the preset is a decision, not a guess", ties+1, best.kind)}
	}
	r.Call, r.Kind = best.call, best.kind
	r.Anchor = modulesArg(best.call)
	return nil
}

// importLocals maps a file's local package names to import paths.
func importLocals(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path_Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

// path_Base is filepath.Base for import paths, which are always slash-separated.
func path_Base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// modulesArg finds the `app.Modules` argument — the product's own feature
// list, which by doctrine is the LAST thing in the assembly. A preset the
// product's features may consume has to be registered before it reads. Both
// spellings count: the taught var (`app.Modules`) and the older call form
// (`app.Modules()`), which shipped products still carry.
func modulesArg(call *ast.CallExpr) ast.Node {
	for _, a := range call.Args {
		expr := ast.Unparen(a)
		if c, ok := expr.(*ast.CallExpr); ok {
			expr = c.Fun
		}
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Modules" {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "app" {
			return a
		}
	}
	return nil
}

// ---- the edit ----

// withPreset returns the gofmt'd source before and after the edit. Both are
// formatted, so the diff shows the wiring change and not gofmt's opinion —
// which is also the deal this command makes: `add` accepts gofmt normalization
// of main.go, because the alternative is a byte-preserving editor nobody can
// review.
func (r *productRoot) withPreset(p *preset, entry string) (before, after []byte, err error) {
	argOff, argText := r.argEdit(entry)
	edited := splice(r.Src, argOff, argText)

	// main.go may already import the preset — for a type in a helper, say —
	// in which case the argument is the whole edit.
	if !r.imports(p.path()) {
		impOff, impText, ok := r.importEdit(p.path())
		if !ok {
			return nil, nil, &rootRefusal{"main.go has no import block to add " + p.path() + " to"}
		}
		// The import block precedes the assembly, so splicing it second
		// cannot move the offset the first splice already used.
		edited = splice(edited, impOff, impText)
	}

	if before, err = format.Source(r.Src); err != nil {
		return nil, nil, err
	}
	if after, err = format.Source(edited); err != nil {
		return nil, nil, fmt.Errorf("the edited main.go does not parse (%w) — nothing written", err)
	}
	return before, after, nil
}

// imports reports whether main.go already imports path.
func (r *productRoot) imports(path string) bool {
	for _, imp := range r.Ast.Imports {
		if q, err := strconv.Unquote(imp.Path.Value); err == nil && q == path {
			return true
		}
	}
	return false
}

func splice(src []byte, off int, text string) []byte {
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:off]...)
	out = append(out, text...)
	return append(out, src[off:]...)
}

// argEdit computes where the new argument goes and the exact text to splice.
//
// The rules, in order:
//
//	before app.Modules() when the assembly has one (and above its comment,
//	                     so the comment keeps the argument it describes)
//	last                 otherwise — after the final argument's line
//	inline               when the whole call is on one line
func (r *productRoot) argEdit(entry string) (off int, text string) {
	pos := func(p token.Pos) int { return r.Fset.Position(p).Offset }
	line := func(p token.Pos) int { return r.Fset.Position(p).Line }
	multiline := line(r.Call.Lparen) != line(r.Call.Rparen)

	if r.Anchor != nil {
		anchor := r.Anchor
		if !multiline {
			return pos(anchor.Pos()), entry + ", "
		}
		if doc := docBefore(r.Ast, r.Fset, anchor, r.prevEnd(anchor)); doc != nil {
			anchor = doc
		}
		ls := lineStart(r.Src, pos(anchor.Pos()))
		return ls, leadingWS(r.Src, ls) + entry + ",\n"
	}

	if len(r.Call.Args) == 0 {
		return pos(r.Call.Lparen) + 1, entry
	}
	last := r.Call.Args[len(r.Call.Args)-1]
	end, rparen := pos(last.End()), pos(r.Call.Rparen)
	if !multiline {
		return end, ", " + entry
	}
	rest := r.Src[end:rparen]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 || !hasComma(rest[:nl]) {
		// No line to land on, or no trailing comma to follow: append inline
		// and let gofmt decide the shape.
		return end, ", " + entry
	}
	ls := lineStart(r.Src, pos(last.Pos()))
	return end + nl + 1, leadingWS(r.Src, ls) + entry + ",\n"
}

// prevEnd is the end of the argument before the anchor — the boundary a doc
// comment cannot cross backwards.
func (r *productRoot) prevEnd(anchor ast.Node) token.Pos {
	prev := r.Call.Lparen
	for _, a := range r.Call.Args {
		if a == anchor {
			break
		}
		prev = a.End()
	}
	return prev
}

// docBefore returns the comment group that sits immediately above node (and
// any group stacked above that one), so an insertion lands above the comment
// rather than between the comment and what it describes.
func docBefore(f *ast.File, fset *token.FileSet, node ast.Node, prevEnd token.Pos) ast.Node {
	var best ast.Node
	line := fset.Position(node.Pos()).Line
	for i := len(f.Comments) - 1; i >= 0; i-- {
		cg := f.Comments[i]
		if cg.End() > node.Pos() {
			continue
		}
		if prevEnd.IsValid() && cg.Pos() < prevEnd {
			break
		}
		if fset.Position(cg.End()).Line != line-1 {
			break
		}
		best, line = cg, fset.Position(cg.Pos()).Line
	}
	return best
}

// importEdit places the import inside the framework's own import group, in
// sorted position — what goimports would do, done by hand because the
// companion's module has no dependencies and this is not worth one.
func (r *productRoot) importEdit(path string) (off int, text string, ok bool) {
	quoted := strconv.Quote(path)
	var decl *ast.GenDecl
	for _, d := range r.Ast.Decls {
		gd, isGen := d.(*ast.GenDecl)
		if isGen && gd.Tok == token.IMPORT && gd.Lparen.IsValid() {
			decl = gd
			break
		}
	}
	if decl == nil {
		return 0, "", false
	}
	pos := func(p token.Pos) int { return r.Fset.Position(p).Offset }

	var anchor *ast.ImportSpec
	before := false
	for _, s := range decl.Specs {
		is, isImp := s.(*ast.ImportSpec)
		if !isImp {
			continue
		}
		p, err := strconv.Unquote(is.Path.Value)
		if err != nil || (p != frameworkModule && !strings.HasPrefix(p, frameworkModule+"/")) {
			continue
		}
		if p > path {
			anchor, before = is, true
			break
		}
		anchor, before = is, false
	}

	if anchor == nil {
		// No framework import to join: open a group of its own at the end.
		ls := lineStart(r.Src, pos(decl.Rparen))
		return ls, "\n\t" + quoted + "\n", true
	}
	if before {
		node := ast.Node(anchor)
		if doc := docBefore(r.Ast, r.Fset, anchor, decl.Lparen); doc != nil {
			node = doc
		}
		ls := lineStart(r.Src, pos(node.Pos()))
		return ls, leadingWS(r.Src, ls) + quoted + "\n", true
	}
	end := pos(anchor.End())
	rest := r.Src[end:pos(decl.Rparen)]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		ls := lineStart(r.Src, pos(decl.Rparen))
		return ls, "\t" + quoted + "\n", true
	}
	ls := lineStart(r.Src, pos(anchor.Pos()))
	return end + nl + 1, leadingWS(r.Src, ls) + quoted + "\n", true
}

// ---- the diff ----

// presetDiff shows the two regions the edit touches — the import block and the
// assembly — and nothing else. A whole-file diff would be one hunk stretching
// from the imports to `var App`, which is exactly the review nobody reads.
func presetDiff(p palette, file, label string, before, after []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n--- %s\n+++ %s   (%s)\n", filepath.Base(file), filepath.Base(file), label)
	bi, ba, err1 := regions(before)
	ai, aa, err2 := regions(after)
	if err1 != nil || err2 != nil {
		return b.String()
	}
	b.WriteString(p.dim("@@ imports @@") + "\n")
	b.WriteString(hunk(p, bi, ai))
	b.WriteString(p.dim("@@ var App @@") + "\n")
	b.WriteString(hunk(p, ba, aa))
	return b.String()
}

// regions slices a formatted main.go into its import block and its `var App`
// declaration.
func regions(src []byte) (imports, assembly string, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.SkipObjectResolution)
	if err != nil {
		return "", "", err
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		if gd.Tok == token.IMPORT && imports == "" {
			imports = string(src[off(gd.Pos()):off(gd.End())])
		}
		if gd.Tok == token.VAR {
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					if n.Name == "App" {
						assembly = string(src[off(gd.Pos()):off(gd.End())])
					}
				}
			}
		}
	}
	return imports, assembly, nil
}

// hunk diffs two blocks that differ only by insertion: trim the common prefix
// and suffix, then show one line of context around what changed.
func hunk(p palette, before, after string) string {
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")
	head := 0
	for head < len(b) && head < len(a) && b[head] == a[head] {
		head++
	}
	tail := 0
	for tail < len(b)-head && tail < len(a)-head && b[len(b)-1-tail] == a[len(a)-1-tail] {
		tail++
	}
	if head == len(b) && head == len(a) {
		return "  (unchanged)\n"
	}
	var out strings.Builder
	ctx := func(i int) {
		if i >= 0 && i < len(b) {
			fmt.Fprintf(&out, "  %s\n", b[i])
		}
	}
	ctx(head - 2)
	ctx(head - 1)
	for _, line := range b[head : len(b)-tail] {
		fmt.Fprintln(&out, p.red("- "+line))
	}
	for _, line := range a[head : len(a)-tail] {
		fmt.Fprintln(&out, p.green("+ "+line))
	}
	ctx(len(b) - tail)
	ctx(len(b) - tail + 1)
	return out.String()
}

// ---- small text helpers ----

func lineStart(src []byte, off int) int {
	if i := bytes.LastIndexByte(src[:off], '\n'); i >= 0 {
		return i + 1
	}
	return 0
}

func leadingWS(src []byte, ls int) string {
	i := ls
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return string(src[ls:i])
}

// hasComma reports whether a comma appears in code (not in a comment) — the
// test for "the argument list already ends in a trailing comma".
func hasComma(s []byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' && i+1 < len(s) && (s[i+1] == '/' || s[i+1] == '*') {
			return false
		}
		if s[i] == ',' {
			return true
		}
	}
	return false
}

// oneLine renders a node as a single line, for a table cell.
func oneLine(fset *token.FileSet, n ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, fset, n); err != nil {
		return ""
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	if len(s) > 52 {
		s = s[:51] + "…"
	}
	return s
}
