package main

// `ultra add <preset>...` is the GROWTH verb — the sanctioned way a product
// gains infrastructure after birth.
//
// The split it enforces is the whole design. `ultra new`'s four flags
// (--db --web --auth --mqtt) shape the TREE: they decide which files exist,
// so they can only be chosen once, at birth. An infrastructure preset shapes
// exactly ONE LINE in ultra.New(…) and no files at all — so it is a VERB
// instead, runnable on the day the product is born and on any day after. The
// wizard's Infrastructure page calls this same code; a retrofit onto a
// three-year-old product runs the identical path.
//
//	ultra add jobs redis        # two lines, two imports, one gofmt
//	ultra add rate --dry        # the diff, nothing written
//
// It edits an AST it can prove it understands, or it edits NOTHING: a product
// whose main.go has drifted from the scaffold's `var App = …` shape earns
// error[ADD0101] and the exact line to paste, never a guess. The insertion
// machinery itself is contrib.go's — loadRoot, withPreset, the preset catalog —
// because `ultra contrib add` already proved it and a second AST editor in one
// binary is a second set of bugs.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bronystylecrazy/di/diag"
)

// addKind says whether `ultra add` will insert a preset — and when it will
// not, why. The refusals are as much of this table's product as the offers:
// "unknown preset" for something that plainly exists would be a lie, so a
// preset this verb declines is declined IN WORDS, with the thing to do instead.
type addKind int

const (
	// addOffered: one Use() line at the anchor, and nothing else. The whole
	// edit is reviewable in a diff.
	addOffered addKind = iota
	// addCapability: `ultra new`'s flags own it, because it shapes the tree —
	// internal/db, the SPA seam, the login page. One line would leave the
	// files it needs missing.
	addCapability
	// addAlwaysOn: every scaffold already wires it. Adding it again is the
	// harmless diamond at best and a duplicate registration at worst.
	addAlwaysOn
	// addSurgery: the wiring is real but it is not ONE line — it replaces a
	// block, and a half-applied swap is worse than an unapplied one.
	addSurgery
)

// addRow is one row of the growth catalog: what this verb does about a preset,
// and the sentence a refusal prints.
type addRow struct {
	Kind addKind
	// Why is the refusal's reason, and for addSurgery the steps. Empty for
	// addOffered, whose reason is the preset's own Doc.
	Why string
	// Config is the TOML a preset needs before the product will BOOT, dev
	// values included — empty for the presets that boot on their defaults.
	//
	// This distinction is not cosmetic and it is not guessed: it was measured.
	// Every offered preset was scaffolded, added, built and put through the
	// product's own covenant, and three of them turn it RED on arrival —
	// redis (REDIS0201), s3 (S30201) and inference (INFER0201) all fail at
	// Start with no configured endpoint. A verb that printed "NEXT STEPS: go
	// test" and left the reader to discover that would be worse than one that
	// refused, so the block is printed WITH the edit.
	//
	// The values match the container the preset declares to devinfra, so a
	// paste plus `ultra dev` is a working pair rather than two guesses. They
	// are static text on purpose: `ultra` never runs the product's binary, so
	// it cannot ask `config reference` what the section looks like — and the
	// generated reference answers "" for exactly these keys anyway.
	Config string
}

// addCatalog is the verdict for every preset in contrib.go's catalog. It is
// exhaustive BY TEST (TestAddCatalogCoversEveryPreset): a preset that lands in
// contrib without a row here fails the build rather than silently becoming
// un-addable, because "ultra add knows about it" is the promise this file makes.
var addCatalog = map[string]addRow{
	// ── offered, and green on the covenant with no config at all ────────────
	"jobs":    {Kind: addOffered},
	"ws":      {Kind: addOffered},
	"rate":    {Kind: addOffered},
	"cache":   {Kind: addOffered},
	"notify":  {Kind: addOffered},
	"seed":    {Kind: addOffered},
	"audit":   {Kind: addOffered},
	"console": {Kind: addOffered},
	"report":  {Kind: addOffered},

	// ── offered, but the product will not BOOT until the section is written ─
	"redis": {Kind: addOffered, Config: `[redis]
url = "redis://localhost:6379"   # the scheme is required (REDIS0202 without it)`},
	"s3": {Kind: addOffered, Config: `[s3]
endpoint   = "localhost:9000"    # host:port, NO scheme
access_key = "minioadmin"
secret_key = "minioadmin"
bucket     = "app"
use_ssl    = false`},
	"inference": {Kind: addOffered, Config: `[inference]
address = "localhost:50051"      # or "unix:///run/detector.sock"`},

	// ── the tree's shape: `ultra new` flags ─────────────────────────────────
	"pg": {Kind: addCapability, Why: "`ultra new --db` owns Postgres: it writes internal/db/migrations, " +
		"the sqlc config and the embed line pg+migrate need. Scaffold with --db, " +
		"or wire pg.Use() + migrate.Use() by hand alongside those files."},
	"migrate": {Kind: addCapability, Why: "migrations ride with `ultra new --db` — the embedded " +
		"internal/db/migrations tree is what migrate.Files points at."},
	"auth": {Kind: addCapability, Why: "`ultra new --auth` owns identity: the login page, the seeded " +
		"dev user, the [auth.jwt] secret and the six auth.* registrations are a tree, not a line."},
	"mqtt": {Kind: addCapability, Why: "`ultra new --mqtt` owns the device edge (it also needs --auth for " +
		"the broker's CONNECT gate)."},
	"api": {Kind: addCapability, Why: "the TYPED api road is the legacy edge, scaffolded by " +
		"`ultra new --from <openapi.json>`. A forward product rides handler-standard v3 (web.Use), " +
		"and a product cannot ride both."},

	// ── already in every assembly ───────────────────────────────────────────
	"conf": {Kind: addAlwaysOn, Why: "web.Use()/ultra.New already bundle the config machinery."},
	"otel": {Kind: addAlwaysOn, Why: "the scaffold wires the observability waterfall — dormant until " +
		"[otel] or an env var names an exporter."},
	"zlog": {Kind: addAlwaysOn, Why: "the scaffold wires zerolog on [log]."},
	"i18n": {Kind: addAlwaysOn, Why: "the scaffold wires i18n.Use() + i18n.Resolve + i18n.Files and commits " +
		"the message catalogs."},

	// ── not one line ────────────────────────────────────────────────────────
	// The steps name authpg.Use(), NOT authpg.Stores(): there is no Stores in
	// contrib/authpg — Use() and its older alias Module() are the whole
	// surface, and the di.Bind pair they register is what supersedes the seed.
	"authpg": {Kind: addSurgery, Why: "authpg SWAPS the dev-seed store rather than joining it. Its " +
		"di.Bind[auth.UserStore] and the scaffold's DEV ONLY block bind the same type, so leaving " +
		"both is an ambiguity (DI0004), not a merge. Do it by hand:\n" +
		"  1. delete the `DEV ONLY — DELETE ME` block in main.go, banner to banner\n" +
		"  2. put authpg.Use() in its place\n" +
		"  3. drop the now-unused `di` import if nothing else in main.go uses it\n" +
		"  4. go test ./...     then ./app user add — the in-memory user is gone\n" +
		"authpg also needs pg and migrate: without a Runner its tables are never\n" +
		"created and every login is a 500 (AUTHPG0101)."},
}

// infraCurated is the wizard's Infrastructure page, in order. Five, not
// twelve: a first-product form is a place to learn the shape of the platform,
// not to browse its catalog. Everything else stays one `ultra add` away, which
// is the page's own subtitle.
var infraCurated = []string{"jobs", "redis", "s3", "ws", "rate"}

// addOffers is every preset this verb will insert, sorted — the did-you-mean
// corpus and the help screen's table.
func addOffers() []string {
	var out []string
	for name, row := range addCatalog {
		if row.Kind == addOffered {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// addNames is every preset the catalog has a verdict about — the did-you-mean
// corpus. It is deliberately WIDER than addOffers: a reader who types `pgg`
// meant pg, and "did you mean pg?" followed by pg's own explanation of why it
// rides --db is a better answer than a shrug about a name nobody recognizes.
func addNames() []string {
	out := make([]string, 0, len(addCatalog))
	for name := range addCatalog {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// didYouMean is the closest catalog name within an edit distance of 2 —
// help.go's rule for command names, applied to preset names, and its
// editDistance rather than a second copy of Levenshtein.
func didYouMean(typo string, corpus []string) string {
	best, bestD := "", 3
	for _, name := range corpus {
		if d := editDistance(strings.ToLower(typo), name); d < bestD {
			best, bestD = name, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" — did you mean %q?", best)
}

// addNeeds is the dependency guard's table: preset → the presets that must
// already be wired for its line to mean anything.
//
// It is DERIVED from presetDeps — contrib.go's evidence table, whose every row
// cites the contrib source line that proves it — rather than restated here.
// That matters more than the duplication it saves: `remove` refuses to unwire
// pg while jobs is wired, and `add` refuses to wire jobs while pg is not. Those
// are the same fact read in two directions, and a second table would let them
// disagree about it.
func addNeeds(name string) []presetDep {
	var out []presetDep
	for _, d := range presetDeps {
		if d.Dependent == name {
			out = append(out, d)
		}
	}
	return out
}

func cmdAdd(args []string, out, errW io.Writer) int {
	var names []string
	dir, dry := ".", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--dry" || a == "-dry":
			dry = true
		case (a == "--dir" || a == "-dir") && i+1 < len(args):
			dir, i = args[i+1], i+1
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("add"), a)
		default:
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		ultraTree().find("add").help(errW)
		return 2
	}
	return addPresets(dir, names, dry, out, errW)
}

// addPresets is the verb's whole body, and the seam the wizard enters through:
// a scaffold that has just been written is the same retrofit target as a
// product from three years ago, so there is ONE path and the wizard is a caller
// of it — the same law the wizard already holds for `ultra new`.
//
// Everything is validated BEFORE anything is written. A run that would refuse
// its second preset must not have edited main.go for its first: a half-applied
// growth edit is the one outcome that leaves a reader unable to tell what
// happened.
func addPresets(dir string, names []string, dry bool, out, errW io.Writer) int {
	label := "add " + strings.Join(names, " ")

	chosen, code := resolveAddNames(names, errW, label)
	if code != 0 {
		return code
	}

	// The root is read once here purely to REFUSE early — the edit loop below
	// re-reads it per preset, because each write moves every offset after it.
	if _, err := loadRoot(dir); err != nil {
		return addRootRefusal(err, dir, chosen, out, errW, label)
	}

	wired, err := scanWired(dir)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, label, err.Error())
		return 1
	}
	if code := checkAddDeps(chosen, wired, errW, label); code != 0 {
		return code
	}

	var added, already []*preset
	for _, p := range chosen {
		// Re-read per preset: the previous iteration rewrote main.go, so a
		// root held across the loop would splice at stale offsets.
		root, err := loadRoot(dir)
		if err != nil {
			return addRootRefusal(err, dir, chosen, out, errW, label)
		}
		// The ASSEMBLY is the authority on "already wired", not the import
		// graph — a feature importing contrib/redis for a type is exactly the
		// product that still needs redis.Use().
		if have := root.presetArgs(p); len(have) > 0 {
			fmt.Fprintf(out, "  %s already wired — %s at main.go:%d\n",
				p.Pkg, oneLine(root.Fset, have[0]), root.Fset.Position(have[0].Pos()).Line)
			already = append(already, p)
			continue
		}
		entry := p.entry(productName(dir))
		before, after, err := root.withPreset(p, entry)
		if err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, label, err.Error())
			return 1
		}
		if dry {
			fmt.Fprint(out, presetDiff(colorFor(out), root.File, "+ "+entry, before, after))
			added = append(added, p)
			continue
		}
		if err := os.WriteFile(root.File, after, 0o644); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, label, err.Error())
			return 1
		}
		// The line is read back off DISK, after gofmt has had its say, so the
		// file:line printed is the one a reader's editor will jump to — the
		// point-to-the-line law. A computed guess would drift the moment the
		// formatter moved anything.
		fmt.Fprintf(out, "  %s  %s\n", entry, addedAt(dir, p))
		added = append(added, p)
	}

	if dry {
		fmt.Fprintf(out, "\n--dry: %s planned, nothing written.\n", count(len(added), "edit"))
		verdict(errW, label, fmt.Sprintf("--dry: %s planned", count(len(added), "edit")))
		return 0
	}
	if len(added) == 0 {
		verdict(errW, label, "already wired — nothing to do")
		return 0
	}
	if note := ensureContribRequire(dir); note != "" {
		fmt.Fprint(out, "\n"+note)
	}
	fmt.Fprint(out, "\n"+addNudge(dir, added, wired))
	verdict(errW, label, fmt.Sprintf("%s wired into main.go", count(len(added), "preset")))
	return 0
}

// addedAt is the file:line the preset actually landed on, re-read from disk
// after the write.
func addedAt(dir string, p *preset) string {
	root, err := loadRoot(dir)
	if err != nil {
		return "main.go"
	}
	args := root.presetArgs(p)
	if len(args) == 0 {
		return "main.go"
	}
	rel, err := filepath.Rel(dir, root.File)
	if err != nil {
		rel = "main.go"
	}
	return fmt.Sprintf("%s:%d", rel, root.Fset.Position(args[0].Pos()).Line)
}

// resolveAddNames turns typed words into catalog rows, refusing the whole run
// on the first one it cannot serve. Duplicates collapse — `ultra add jobs jobs`
// is one edit, not a duplicate registration.
func resolveAddNames(names []string, errW io.Writer, label string) ([]*preset, int) {
	var chosen []*preset
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		row, known := addCatalog[name]
		p := presetByName(name)
		if !known || p == nil {
			fmt.Fprintf(errW, "unknown preset %q%s\n", name, didYouMean(name, addNames()))
			fmt.Fprintf(errW, "\n`ultra add --help` lists every preset this verb wires.\n")
			failVerdict(errW, label, "unknown preset "+name)
			return nil, 2
		}
		if row.Kind != addOffered {
			fmt.Fprintf(errW, "ultra add %s: %s\n", name, addRefusalHead(row.Kind))
			fmt.Fprintf(errW, "\n%s\n", row.Why)
			failVerdict(errW, label, name+" is not a one-line preset")
			return nil, 1
		}
		chosen = append(chosen, p)
	}
	return chosen, 0
}

func addRefusalHead(k addKind) string {
	switch k {
	case addCapability:
		return "that is a scaffold CAPABILITY, not an infrastructure line."
	case addAlwaysOn:
		return "already wired by every scaffold."
	case addSurgery:
		return "that edit is not one line, so this verb will not guess at it."
	}
	return ""
}

// addRootRefusal is ADD0101: a main.go this verb cannot prove it understands.
// Products EDIT main.go — that is the point of the file — so a drifted root is
// a normal thing to meet, not a corruption. The answer is the exact lines to
// paste and where, never a guess-edit.
func addRootRefusal(err error, dir string, chosen []*preset, out, errW io.Writer, label string) int {
	var refusal *rootRefusal
	if !errors.As(err, &refusal) {
		fmt.Fprintln(errW, err)
		failVerdict(errW, label, err.Error())
		return 1
	}
	fmt.Fprintf(errW, "error[ADD0101]: %v\n\n", err)
	fmt.Fprintf(errW, "%s\n", "The edit is one line per preset, so do it by hand — add these to your\nassembly (before app.Modules, if it has one):")
	for _, p := range chosen {
		fmt.Fprintf(errW, "\n  %s\n      import %q\n", p.entry(productName(dir)), p.path())
	}
	fmt.Fprintf(errW, "\n  more: ultra explain ADD0101\n")
	failVerdict(errW, label, "refused: the root is not canonical")
	return 1
}

// checkAddDeps is ADD0102. A preset whose dependency is missing does not fail
// at `ultra add` time — it fails at Validate, as a DI0001 about a type the
// reader never asked for. Refusing here turns that into a sentence naming the
// command that fixes it.
func checkAddDeps(chosen []*preset, wired map[string]*wiredUse, errW io.Writer, label string) int {
	// A preset being added in the same run counts as present: `ultra add
	// jobs pg` must not refuse jobs for a pg that is two words away.
	incoming := map[string]bool{}
	for _, p := range chosen {
		incoming[p.Pkg] = true
	}
	for _, p := range chosen {
		var missing []presetDep
		for _, d := range addNeeds(p.Pkg) {
			if wired[d.Needs] == nil && !incoming[d.Needs] {
				missing = append(missing, d)
			}
		}
		if len(missing) == 0 {
			continue
		}
		fmt.Fprintf(errW, "error[ADD0102]: %s needs a preset this product does not wire\n\n", p.Pkg)
		for _, d := range missing {
			fmt.Fprintf(errW, "  %s needs %s\n      %s\n", d.Dependent, d.Needs, d.Why)
		}
		// The fix names the CAPABILITY when one owns the dependency, because
		// `ultra add pg` is a refusal of its own — pg rides --db.
		fmt.Fprintf(errW, "\n%s\n", addDepFix(missing))
		fmt.Fprintf(errW, "\n  more: ultra explain ADD0102\n")
		failVerdict(errW, label, p.Pkg+" needs "+missing[0].Needs)
		return 1
	}
	return 0
}

// addDepFix is the one line to type. It routes through the capability flag when
// the missing preset is one the tree owns, so the advice is never `ultra add
// pg` — a command this verb refuses.
func addDepFix(missing []presetDep) string {
	var flags, adds []string
	for _, d := range missing {
		switch addCatalog[d.Needs].Kind {
		case addCapability:
			switch d.Needs {
			case "pg", "migrate":
				flags = appendUnique(flags, "--db")
			case "auth":
				flags = appendUnique(flags, "--auth")
			case "mqtt":
				flags = appendUnique(flags, "--mqtt")
			}
		default:
			adds = appendUnique(adds, d.Needs)
		}
	}
	var b strings.Builder
	b.WriteString("The fix:\n")
	if len(flags) > 0 {
		// Name only the files the flags actually being suggested bring, so the
		// parenthetical never explains a flag the reader was not offered.
		var brings []string
		for _, f := range flags {
			switch f {
			case "--db":
				brings = append(brings, "internal/db/migrations")
			case "--auth":
				brings = append(brings, "the login page and dev seed")
			case "--mqtt":
				brings = append(brings, "the mqtt feature surface")
			}
		}
		fmt.Fprintf(&b, "  scaffold with %s — those presets ride the tree they need (%s)\n",
			strings.Join(flags, " "), strings.Join(brings, ", "))
	}
	if len(adds) > 0 {
		fmt.Fprintf(&b, "  ultra add %s\n", strings.Join(adds, " "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// addNudge is the ONE block printed after a run — what the product can now do
// that it could not before, and the two things the wiring line does NOT do by
// itself.
//
// The compose sentence is worded from what the code actually does, which is
// not what the surrounding prose assumes. docker-compose.dev.yml is derived
// from the GRAPH, not from config: pg, redis and s3 fold their
// devinfra.Declare into Use() unconditionally, and their config section only
// moves the host port. And nothing in `ultra dev` writes the file — it boots a
// compose file that already exists. The write comes from the scaffolded
// TestInfraDrift on the first `go test`, or from `infra compose --write`. So
// the nudge names go test as the step that produces the file, and says plainly
// that ultra dev consumes it.
func addNudge(dir string, added []*preset, wired map[string]*wiredUse) string {
	var b strings.Builder

	// The blocking step comes FIRST, before the routine ones. A preset that
	// cannot boot without a config key is not a "next step" — it is the
	// difference between a product that runs and one that does not.
	var blocking []*preset
	for _, p := range added {
		if addCatalog[p.Pkg].Config != "" {
			blocking = append(blocking, p)
		}
	}
	if len(blocking) > 0 {
		names := make([]string, len(blocking))
		for i, p := range blocking {
			names[i] = p.Pkg
		}
		fmt.Fprintf(&b, "CONFIGURE FIRST — %s will not boot on the defaults.\nPaste into config.toml:\n\n",
			strings.Join(names, " and "))
		for _, p := range blocking {
			for _, line := range strings.Split(addCatalog[p.Pkg].Config, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("NEXT STEPS\n")
	b.WriteString("  go mod tidy                      resolve what the new import pulls in\n")
	b.WriteString("  go test ./...                    the covenant names anything still missing\n")

	var sections, infra []string
	for _, p := range added {
		if p.Section != "" {
			sections = appendUnique(sections, p.Section)
		}
		if p.Infra {
			infra = appendUnique(infra, p.Pkg)
		}
	}
	if len(sections) > 0 {
		fmt.Fprintf(&b, "  go run . config reference        the annotated [%s] — every key, generated\n",
			strings.Join(sections, "] ["))
	}
	if len(infra) > 0 {
		// A verb takes its s in the SINGULAR, which is the opposite of the
		// noun `plural` handles: "redis declares" but "redis + s3 declare".
		declares := "declare"
		if len(infra) == 1 {
			declares = "declares"
		}
		fmt.Fprintf(&b, "\n%s %s a dev service, and docker-compose.dev.yml is DERIVED from the\ngraph — the first `go test ./...` writes it (TestInfraDrift), and after that a\nnew declaration fails there until `go run . infra compose --write` refreshes\nit. `ultra dev` BOOTS that file; it does not write it.\n",
			strings.Join(infra, " + "), declares)
		// audit is the one declarer whose container is config-gated: its
		// newInfraService returns the zero Service unless the backend is
		// clickhouse, and a nameless declaration renders nothing.
		if slices.Contains(infra, "audit") {
			b.WriteString("audit only declares one when [audit] backend = \"clickhouse\" — on the\ndefault postgres backend it renders no service at all.\n")
		}
	}

	// A pair this run already added is not missing. Without this the nudge
	// tells you to add the two presets it just added, one line above.
	justAdded := map[string]bool{}
	for _, p := range added {
		justAdded[p.Pkg] = true
	}
	var pairs []string
	for _, p := range added {
		for _, pair := range p.Pairs {
			if wired[pair] == nil && !justAdded[pair] && addCatalog[pair].Kind == addOffered {
				pairs = appendUnique(pairs, pair)
			}
		}
	}
	if len(pairs) > 0 {
		fmt.Fprintf(&b, "\nusually pairs with %s:\n  ultra add %s\n",
			strings.Join(pairs, " + "), strings.Join(pairs, " "))
	}
	return b.String()
}

// The ADD family: this verb's own refusals. Registered here so `ultra explain
// ADD0101` teaches them and `ultra codes` lists them, exactly as the dev loop's
// DEV0101 and the requirements tree's REQ01xx do.
func init() {
	diag.Register("ADD0101", `ADD0101 — ultra add will not edit a root it cannot resolve

`+"`ultra add`"+` inserts one argument into the product's assembly call, and it
holds itself to the bar the static analyzer holds: the root must be
`+"`var App = …`"+` in package main, assigned exactly ONCE, holding a bundle call
it knows (ultra.New, fib.Product, stack.Product, di.Options). A root with a
second writer, with no App at all, or with two candidate bundle calls at the
same depth cannot be resolved without guessing.

This is not corruption, and main.go is not off limits — products EDIT their
root, which is the entire point of the file. It only means the edit is yours
to make. The refusal prints the exact argument and import for every preset you
asked for; paste them into the assembly (before app.Modules when there is one,
so the product's own registrations stay last) and the result is
byte-identical to what this command would have written.

If you WANT the command back, the shape it needs is the scaffold's: one
`+"`var App`"+`, one bundle call, never reassigned. `+"`ultra contrib list`"+` reports
which root it resolved, or why it could not.`)

	diag.Register("ADD0102", `ADD0102 — a preset was added without the preset it needs

Some presets are built on others: jobs stores its queue in Postgres, audit
writes its trail there, and both contribute migrate.Files whose tables are
never created without a migrate Runner. Wiring one whose dependency is absent
compiles fine and then fails at Validate as a DI0001 about a type nobody in
the product ever named — the least useful place to learn it.

So the check runs at `+"`ultra add`"+` time instead, off the same evidence table
`+"`ultra contrib remove`"+` guards with: each row cites the constructor in contrib
that proves the dependency, never a hunch. A pair that does NOT break is
deliberately absent — contrib/rate imports redis only for an override seam, so
rate works without it, and refusing there would be a false alarm.

The fix is in the message, and it routes through the right door: a dependency
the TREE owns (pg and migrate ride --db, auth rides --auth) names the scaffold
flag, because those presets need files — internal/db/migrations, the login
page — that no single wiring line can create. Everything else is one more
`+"`ultra add`"+`, and adding both in ONE run is accepted: the guard counts
presets arriving together as present.`)
}
