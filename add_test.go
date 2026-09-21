package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// v3Root is the shape `ultra new` writes TODAY — di.Options holding the
// handler-standard v3 assembly, with app.Modules last behind its comment. The
// contrib tests pin the legacy ultra.New root; this one is what `ultra add`
// meets on a forward product, so the add tests use it.
const v3Root = `package main

import (
	"github.com/bronystylecrazy/di"
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/i18n"
	"github.com/bronystylecrazy/ultrastack/contrib/migrate"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/stack"
	"github.com/bronystylecrazy/ultrastack/web"

	"example.com/shop/internal/app"
)

const version = "0.1.0"

// App is the root page — the whole product as one value, assigned once.
var App = di.Options(
	stack.Base,
	web.Use(web.Info{Title: "shop", Version: version}),

	i18n.Use(),
	i18n.Resolve,

	pg.Use(),
	migrate.Use(),

	// The product. app.go is the ONLY file that knows the feature list.
	app.Modules,
)

func main() { cli.Run(App) }
`

// TestAddCatalogCoversEveryPreset is the exhaustiveness gate. contrib.go's
// catalog is itself pinned against the contrib tree, so this closes the chain:
// a preset that lands in contrib gets a row here or the build goes red. Without
// it a new preset would silently become un-addable AND un-explainable — the
// worst of both, because `ultra add` would call a real capability "unknown".
func TestAddCatalogCoversEveryPreset(t *testing.T) {
	for _, p := range presets {
		row, ok := addCatalog[p.Pkg]
		if !ok {
			t.Errorf("preset %q has no addCatalog row — decide whether `ultra add` offers it, "+
				"and if not, say why in the row's Why (that sentence is what the refusal prints)", p.Pkg)
			continue
		}
		if row.Kind != addOffered && row.Why == "" {
			t.Errorf("preset %q is refused with no reason — the Why IS the refusal message", p.Pkg)
		}
	}
	for name := range addCatalog {
		if presetByName(name) == nil {
			t.Errorf("addCatalog has a row for %q, which is not a preset in the catalog", name)
		}
	}
}

// TestCuratedInfraIsOffered keeps the wizard's page honest against the verb it
// calls: a curated name the verb would refuse is a form that builds a product
// it cannot finish.
func TestCuratedInfraIsOffered(t *testing.T) {
	for _, name := range infraCurated {
		if addCatalog[name].Kind != addOffered {
			t.Errorf("the wizard offers %q but `ultra add` does not", name)
		}
	}
}

// TestPresetInfraFlagsMatchContrib pins the catalog's Infra column to the only
// thing that decides it: a devinfra.Declare in the preset's own source. The
// column was WRONG in both directions before this test existed — mqtt was
// marked as wanting a dev service though it deliberately declares none (the
// broker is embedded), and audit was unmarked though it declares ClickHouse.
// Both fed a nudge that told readers to run a command with nothing to do.
func TestPresetInfraFlagsMatchContrib(t *testing.T) {
	root, err := filepath.Abs("../../contrib")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Skip("no contrib tree beside this checkout")
	}
	for _, p := range presets {
		declares := false
		entries, err := os.ReadDir(filepath.Join(root, p.Pkg))
		if err != nil {
			continue // not a directory in this checkout; the catalog test owns that
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, p.Pkg, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "devinfra.Declare") {
				declares = true
			}
		}
		if declares != p.Infra {
			t.Errorf("preset %q: catalog says Infra=%v, contrib/%s %s a devinfra.Declare",
				p.Pkg, p.Infra,
				p.Pkg, map[bool]string{true: "HAS", false: "has NO"}[declares])
		}
	}
}

// TestAddWiresMultiplePresetsInOneRun is the verb's headline: several presets,
// one command, each landing before app.Modules with its import, and the file
// left gofmt-clean.
func TestAddWiresMultiplePresetsInOneRun(t *testing.T) {
	dir := writeProduct(t, v3Root, nil)
	code, out, errOut := runCLI(t, "add", "ws", "cache", "--dir", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	for _, want := range []string{
		"ws.Use()", "cache.Use()",
		`"github.com/bronystylecrazy/ultrastack/contrib/ws"`,
		`"github.com/bronystylecrazy/ultrastack/contrib/cache"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("main.go must contain %q:\n%s", want, got)
		}
	}
	// Both land BEFORE the product's own registrations: app.Modules stays last,
	// which is the assembly's one structural law.
	if strings.Index(got, "ws.Use()") > strings.Index(got, "app.Modules") {
		t.Error("ws.Use() landed after app.Modules")
	}
	// The point-to-the-line law: every preset reports where it went.
	for _, want := range []string{"ws.Use()  main.go:", "cache.Use()  main.go:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output must point at the line for %q:\n%s", want, out)
		}
	}
}

// TestAddIsIdempotent pins the second run: a note with the line number, exit 0,
// and not one byte changed. The wizard depends on this — it calls add on a
// product it may already have added to.
func TestAddIsIdempotent(t *testing.T) {
	dir := writeProduct(t, v3Root, nil)
	if code, _, errOut := runCLI(t, "add", "ws", "--dir", dir); code != 0 {
		t.Fatalf("first add: code=%d err=%s", code, errOut)
	}
	first := readMain(t, dir)

	code, out, _ := runCLI(t, "add", "ws", "--dir", dir)
	if code != 0 {
		t.Fatalf("a second add must succeed, got %d", code)
	}
	if !strings.Contains(out, "already wired") || !strings.Contains(out, "main.go:") {
		t.Errorf("the second run must say already wired WITH the line:\n%s", out)
	}
	if second := readMain(t, dir); second != first {
		t.Errorf("a second add rewrote main.go:\n--- first\n%s\n--- second\n%s", first, second)
	}
}

// TestAddUnknownPresetSuggests proves the typo path. The corpus is every
// catalog name, not just the offered ones, so a near-miss on a capability
// points at the capability rather than shrugging.
func TestAddUnknownPresetSuggests(t *testing.T) {
	dir := writeProduct(t, v3Root, nil)
	for _, c := range []struct{ typo, want string }{
		{"reddis", `"redis"`},
		{"jbos", `"jobs"`},
		{"postgre", `"postgres"`}, // no such preset and nothing within 2 — no suggestion
	} {
		code, _, errOut := runCLI(t, "add", c.typo, "--dir", dir)
		if code == 0 {
			t.Errorf("%q must not be accepted", c.typo)
		}
		if !strings.Contains(errOut, "unknown preset") {
			t.Errorf("%q: must say unknown preset:\n%s", c.typo, errOut)
		}
		if c.want != `"postgres"` && !strings.Contains(errOut, c.want) {
			t.Errorf("%q: expected a suggestion of %s:\n%s", c.typo, c.want, errOut)
		}
	}
	// Nothing was written by any of them.
	if got := readMain(t, dir); got != v3Root {
		t.Errorf("a refused add edited main.go:\n%s", got)
	}
}

// TestAddRefusesWhatIsNotOneLine covers the three declined kinds. Each must
// name the thing to do INSTEAD — a refusal that only says no is a dead end, and
// these are all real capabilities the reader was right to want.
func TestAddRefusesWhatIsNotOneLine(t *testing.T) {
	dir := writeProduct(t, v3Root, nil)
	for _, c := range []struct{ preset, want string }{
		{"pg", "--db"},            // capability: the tree owns it
		{"auth", "--auth"},        // capability
		{"otel", "already wired"}, // always on
		{"authpg", "DELETE ME"},   // surgery: name the block to remove
		{"api", "--from"},         // capability, and the road it belongs to
	} {
		code, _, errOut := runCLI(t, "add", c.preset, "--dir", dir)
		if code == 0 {
			t.Errorf("%s must be refused", c.preset)
		}
		if !strings.Contains(errOut, c.want) {
			t.Errorf("refusing %s must name %q:\n%s", c.preset, c.want, errOut)
		}
	}
	if got := readMain(t, dir); got != v3Root {
		t.Errorf("a refused add edited main.go:\n%s", got)
	}
}

// TestAddRefusesAMissingDependency is ADD0102, and the fix it prints routes
// through the CAPABILITY flag rather than telling the reader to run a command
// (`ultra add pg`) this verb would refuse.
func TestAddRefusesAMissingDependency(t *testing.T) {
	// A root with no pg and no migrate.
	bare := strings.ReplaceAll(v3Root, "\tpg.Use()\n\tmigrate.Use()\n\n", "")
	bare = strings.ReplaceAll(bare, "\t\"github.com/bronystylecrazy/ultrastack/contrib/migrate\"\n", "")
	bare = strings.ReplaceAll(bare, "\t\"github.com/bronystylecrazy/ultrastack/contrib/pg\"\n", "")
	dir := writeProduct(t, bare, nil)

	code, _, errOut := runCLI(t, "add", "jobs", "--dir", dir)
	if code == 0 {
		t.Fatal("jobs without pg must be refused")
	}
	for _, want := range []string{"ADD0102", "jobs needs pg", "*pg.DB", "--db", "ultra explain ADD0102"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal must name %q:\n%s", want, errOut)
		}
	}
	if got := readMain(t, dir); got != bare {
		t.Error("a dependency refusal edited main.go")
	}
}

// TestAddCountsAPresetArrivingInTheSameRun pins the guard's one subtlety: a
// dependency two words away on the same command line is present, not missing.
//
// It exercises checkAddDeps directly because no OFFERED preset currently
// depends on another offered one — every row's Needs is pg, migrate or auth,
// all of which ride the scaffold flags. That could change with one contrib
// landing, and the logic has to already be right when it does.
func TestAddCountsAPresetArrivingInTheSameRun(t *testing.T) {
	jobs, pg := presetByName("jobs"), presetByName("pg")
	if jobs == nil || pg == nil {
		t.Fatal("the catalog lost jobs or pg")
	}
	var errOut strings.Builder

	// jobs alone, against a product wiring nothing: refused.
	if code := checkAddDeps([]*preset{jobs}, map[string]*wiredUse{}, &errOut, "add jobs"); code == 0 {
		t.Error("jobs with no pg must be refused")
	}
	// jobs with its dependencies arriving beside it: accepted.
	errOut.Reset()
	chosen := []*preset{jobs, pg, presetByName("migrate")}
	if code := checkAddDeps(chosen, map[string]*wiredUse{}, &errOut, "add jobs pg migrate"); code != 0 {
		t.Errorf("a dependency in the same run must count as present:\n%s", errOut.String())
	}
	// And already-wired counts too.
	errOut.Reset()
	wired := map[string]*wiredUse{"pg": {File: "main.go"}, "migrate": {File: "main.go"}}
	if code := checkAddDeps([]*preset{jobs}, wired, &errOut, "add jobs"); code != 0 {
		t.Errorf("a wired dependency must satisfy the guard:\n%s", errOut.String())
	}
}

// TestPresetDepsRefuseOnlyRealBreakages guards the evidence table against the
// failure mode its own doctrine names: a false refusal. Each row must describe
// a dependency that genuinely breaks, which is why notify→jobs is absent —
// notify takes jobs.Conn and *pg.DB as di.Optional and needs only ONE of them,
// so refusing notify on a pg-only product would block a combination that works.
func TestPresetDepsRefuseOnlyRealBreakages(t *testing.T) {
	for _, d := range presetDeps {
		if d.Dependent == "notify" {
			t.Errorf("notify has a hard dependency row on %q, but its enqueue seam accepts "+
				"jobs.Conn OR *pg.DB — this refuses a product that works", d.Needs)
		}
		if d.Why == "" {
			t.Errorf("%s→%s has no evidence — every row cites the contrib source that proves it",
				d.Dependent, d.Needs)
		}
	}
}

// TestAddRefusesANonCanonicalRoot is ADD0101 — the law that this verb edits an
// AST it can prove it understands or edits nothing. Products edit main.go, so
// this path is normal, and the refusal has to carry the whole manual edit.
func TestAddRefusesANonCanonicalRoot(t *testing.T) {
	mangled := strings.Replace(v3Root, "var App = di.Options(", "var App = buildApp()\n\nvar unused = di.Options(", 1)
	dir := writeProduct(t, mangled, nil)

	code, _, errOut := runCLI(t, "add", "jobs", "redis", "--dir", dir)
	if code == 0 {
		t.Fatal("a non-canonical root must be refused")
	}
	for _, want := range []string{
		"ADD0101",
		"jobs.Use()", // the exact argument...
		"redis.Use()",
		`"github.com/bronystylecrazy/ultrastack/contrib/jobs"`, // ...and the exact import
		"before app.Modules",
		"ultra explain ADD0101",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal must carry %q:\n%s", want, errOut)
		}
	}
	if got := readMain(t, dir); got != mangled {
		t.Error("a refused add edited main.go")
	}
}

// TestAddPrintsTheConfigSectionThatBlocksBoot is the finding this verb exists
// to communicate. redis, s3 and inference bind a section with no usable
// default: with no [redis] url the product fails at Start (REDIS0201) and its
// own `go test` goes red. Printing "NEXT STEPS: go test" and stopping would
// send the reader into that failure, so the block comes first and says so.
func TestAddPrintsTheConfigSectionThatBlocksBoot(t *testing.T) {
	for _, c := range []struct{ preset, key string }{
		{"redis", "url ="},
		{"s3", "endpoint"},
		{"inference", "address ="},
	} {
		dir := writeProduct(t, v3Root, nil)
		code, out, errOut := runCLI(t, "add", c.preset, "--dir", dir)
		if code != 0 {
			t.Fatalf("%s: code=%d err=%s", c.preset, code, errOut)
		}
		for _, want := range []string{"CONFIGURE FIRST", "will not boot", "[" + c.preset + "]", c.key} {
			if !strings.Contains(out, want) {
				t.Errorf("adding %s must print %q:\n%s", c.preset, want, out)
			}
		}
	}
	// And a preset that boots on its defaults must NOT print the block, or the
	// warning stops meaning anything.
	dir := writeProduct(t, v3Root, nil)
	_, out, _ := runCLI(t, "add", "ws", "--dir", dir)
	if strings.Contains(out, "CONFIGURE FIRST") {
		t.Errorf("ws boots on its defaults and must not be flagged:\n%s", out)
	}
}

// TestAddNudgeTellsTheTruthAboutCompose pins the claim this repo's own prose
// gets wrong. docker-compose.dev.yml is derived from the GRAPH, and `ultra dev`
// BOOTS it rather than writing it — the write is TestInfraDrift's on the first
// `go test`, or `infra compose --write` after that.
func TestAddNudgeTellsTheTruthAboutCompose(t *testing.T) {
	dir := writeProduct(t, v3Root, nil)
	code, out, errOut := runCLI(t, "add", "redis", "--dir", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	for _, want := range []string{"TestInfraDrift", "infra compose --write", "does not write it"} {
		if !strings.Contains(out, want) {
			t.Errorf("the compose nudge must say %q:\n%s", want, out)
		}
	}
	// A preset with no declaration must not mention compose at all.
	plain := writeProduct(t, v3Root, nil)
	_, out2, _ := runCLI(t, "add", "ws", "--dir", plain)
	if strings.Contains(out2, "compose") {
		t.Errorf("ws declares no dev service and must not mention compose:\n%s", out2)
	}
}

// TestAddDryWritesNothing keeps --dry honest.
func TestAddDryWritesNothing(t *testing.T) {
	dir := writeProduct(t, v3Root, nil)
	code, out, errOut := runCLI(t, "add", "ws", "--dry", "--dir", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "ws.Use()") || !strings.Contains(out, "nothing written") {
		t.Errorf("--dry must show the edit and say nothing was written:\n%s", out)
	}
	if got := readMain(t, dir); got != v3Root {
		t.Errorf("--dry wrote to main.go:\n%s", got)
	}
}

// TestAddHelpListsEveryOfferedPreset keeps the help screen from drifting away
// from the catalog it describes — the screen is where a reader learns the set.
func TestAddHelpListsEveryOfferedPreset(t *testing.T) {
	code, out, _ := runCLI(t, "add", "--help")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, name := range addOffers() {
		if !strings.Contains(out, name) {
			t.Errorf("`ultra add --help` never mentions the offered preset %q", name)
		}
	}
}

// TestAddCodesAreExplainable closes the diagnostic loop: a code printed by a
// refusal has to be one `ultra explain` can teach, or the "more:" line is a
// dead reference.
func TestAddCodesAreExplainable(t *testing.T) {
	for _, code := range []string{"ADD0101", "ADD0102"} {
		exit, out, _ := runCLI(t, "explain", code)
		if exit != 0 {
			t.Errorf("ultra explain %s exited %d", code, exit)
		}
		if !strings.Contains(out, code) {
			t.Errorf("the lesson for %s must name it:\n%s", code, out)
		}
	}
	exit, out, _ := runCLI(t, "codes")
	if exit != 0 {
		t.Fatalf("codes exited %d", exit)
	}
	for _, code := range []string{"ADD0101", "ADD0102"} {
		if !strings.Contains(out, code) {
			t.Errorf("`ultra codes` must list %s", code)
		}
	}
}

// TestAddOutsideAProductSaysSo — the retrofit verb run in the wrong directory.
func TestAddOutsideAProductSaysSo(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI(t, "add", "ws", "--dir", dir)
	if code == 0 {
		t.Fatal("add outside a product must fail")
	}
	if !strings.Contains(errOut, "main.go") {
		t.Errorf("the error must name the missing main.go:\n%s", errOut)
	}
}

// TestAddCovenant is the real thing: for every offered preset, scaffold a
// product, add the preset, and put the result through the compiler and the
// product's own wiring covenant (stack.Validate — the check that catches a
// missing provider, an ambiguity, a captive dependency). One preset also boots
// for real, which is the only way to learn what Start does.
//
// This is where the addCatalog's two classes were MEASURED rather than
// guessed. Running it is how redis, s3 and inference were found to fail at
// Start with no configured endpoint, and how the rest were confirmed green on
// their defaults — so the Config column is a test result, and this test keeps
// it one.
func TestAddCovenant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a product per preset; skipped in -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range addOffers() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// --db --auth so every dependency in the guard's table is present;
			// the guard itself is proven separately, on a product without them.
			d := testData("speedcheck", scaffoldData{DB: true, Auth: true})
			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}

			var out, errOut strings.Builder
			if code := cmdAdd([]string{name, "--dir", dir}, &out, &errOut); code != 0 {
				t.Fatalf("ultra add %s: code=%d\n%s", name, code, errOut.String())
			}

			sh := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("go", args...)
				cmd.Dir = dir
				b, err := cmd.CombinedOutput()
				if err != nil {
					if offline(b) {
						t.Skipf("go %s needs the network: %s", strings.Join(args, " "), b)
					}
					t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
				}
				return string(b)
			}
			sh("mod", "edit",
				"-replace="+modulePath+"="+repoRoot,
				"-replace="+modulePath+"/contrib="+filepath.Join(repoRoot, "contrib"),
				"-replace="+modulePath+"/web="+filepath.Join(repoRoot, "web"),
				"-replace="+modulePath+"/mqtt="+filepath.Join(repoRoot, "mqtt"))
			// tidy is not optional here: a preset can pull modules the product
			// has no go.sum entry for (report brings gopdf and excelize), which
			// is exactly why the nudge names it.
			sh("mod", "tidy")
			sh("build", "./...")

			// The graph gate. A full boot is NOT required per preset — it would
			// need every backing service — but Validate is, because a preset
			// that cannot be wired is the failure this verb could cause.
			sh("test", "-run", "TestWiring", ".")
		})
	}
}

// TestAddEndToEndBoots is the one combination that goes all the way: jobs on a
// --db product, built and BOOTED against a real Postgres. Validate proves the
// graph; only a boot proves the Start hooks, the migrations jobs contributes,
// and the queue actually coming up.
func TestAddEndToEndBoots(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a product against Postgres; skipped in -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	d := testData("speedcheck", scaffoldData{DB: true, Auth: true})
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	if code := cmdAdd([]string{"jobs", "--dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("ultra add jobs: code=%d\n%s", code, errOut.String())
	}
	sh := func(args ...string) {
		t.Helper()
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		b, err := cmd.CombinedOutput()
		if err != nil {
			if offline(b) {
				t.Skipf("go %s needs the network: %s", strings.Join(args, " "), b)
			}
			t.Fatalf("go %s failed: %v\n%s", strings.Join(args, " "), err, b)
		}
	}
	sh("mod", "edit",
		"-replace="+modulePath+"="+repoRoot,
		"-replace="+modulePath+"/contrib="+filepath.Join(repoRoot, "contrib"),
		"-replace="+modulePath+"/web="+filepath.Join(repoRoot, "web"),
		"-replace="+modulePath+"/mqtt="+filepath.Join(repoRoot, "mqtt"))
	sh("mod", "tidy")
	// The whole product suite: TestWiring, TestBoot (which requires Postgres
	// and skips itself without one), and the contract + infra drift gates.
	sh("test", "./...")
}

// ---- the wizard's half ----

// TestWizardEchoesItsAddLine extends the ECHO LAW to the second command. The
// original law proves the `ultra new` line re-parses to the same product; this
// proves the `ultra add` line names exactly the presets the form collected, in
// catalog order, and appears only when there are some.
func TestWizardEchoesItsAddLine(t *testing.T) {
	for _, c := range []struct {
		name  string
		form  wizardForm
		want  string
		lines int
	}{
		{"none selected", wizardForm{Name: "tiny", Caps: []string{capDB}}, "", 1},
		{"one", wizardForm{Name: "hub", Caps: []string{capDB}, Infra: []string{"ws"}}, "ultra add ws", 2},
		{
			"several, reordered by the form",
			wizardForm{Name: "watchpost", Caps: []string{capDB}, Infra: []string{"rate", "jobs", "redis"}},
			"ultra add jobs redis rate", 2,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := c.form.answers()
			if got := a.addCommand(); got != c.want {
				t.Errorf("addCommand = %q, want %q", got, c.want)
			}
			if got := len(strings.Split(a.commands(), "\n")); got != c.lines {
				t.Errorf("the echo must be %d line(s), got %d:\n%s", c.lines, got, a.commands())
			}
			// Every echoed preset must be one the verb actually offers, or the
			// echoed command is not reproducible.
			for _, name := range a.Infra {
				if addCatalog[name].Kind != addOffered {
					t.Errorf("the wizard echoed %q, which `ultra add` refuses", name)
				}
			}
		})
	}
}

// TestWizardResolvesInfrastructureDependencies is the mqtt→auth rule one rung
// out: picking jobs on a product with no database is a combination `ultra add`
// would refuse (ADD0102), so the form resolves it instead of building a product
// its own second command cannot finish.
func TestWizardResolvesInfrastructureDependencies(t *testing.T) {
	f := wizardForm{Name: "watchpost", Infra: []string{"jobs"}}
	a := f.answers()
	if !a.DB {
		t.Error("jobs must turn db on — its store takes a *pg.DB")
	}
	if !strings.Contains(a.command(), "--db") {
		t.Errorf("the echoed scaffold command must carry --db: %s", a.command())
	}
	// And the resolved product must actually satisfy the guard.
	if a.addCommand() != "ultra add jobs" {
		t.Errorf("addCommand = %q", a.addCommand())
	}
}

// TestWizardSummaryNamesTheInfrastructure keeps the one line a human reads
// before saying yes honest about the second command too.
func TestWizardSummaryNamesTheInfrastructure(t *testing.T) {
	f := wizardForm{Name: "watchpost", Caps: []string{capDB}, Infra: []string{"jobs", "redis"}}
	got := f.answers().summary()
	for _, want := range []string{"watchpost", "+jobs", "+redis"} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary must name %q: %s", want, got)
		}
	}
	// No infrastructure, no plus signs.
	plain := wizardForm{Name: "tiny", Caps: []string{capDB}}
	if s := plain.answers().summary(); strings.Contains(s, "+") {
		t.Errorf("a product with no infrastructure must not show one: %s", s)
	}
}

// TestWizardProductEqualsFlagScaffoldPlusAdd is the SIBLING of the pinned echo
// law, and the one that makes the two-line echo a promise rather than a
// description.
//
// The wizard scaffolds and then adds. This does the same two things the way a
// human reading the echoed lines would — parse the printed `ultra new` line
// with the real parser, scaffold from it, then run the printed `ultra add` line
// against the result — and demands the same main.go bytes. A wizard that
// reordered its presets, resolved a dependency differently, or inserted at a
// different anchor than the standalone verb would fail here and nowhere else.
func TestWizardProductEqualsFlagScaffoldPlusAdd(t *testing.T) {
	for _, form := range []wizardForm{
		{Name: "watchpost", Caps: []string{capDB, capAuth}, Infra: []string{"jobs", "ws"}},
		{Name: "hub", Caps: []string{capDB}, Infra: []string{"redis", "rate"}},
		{Name: "tiny", Caps: []string{capDB}},
	} {
		t.Run(form.Name, func(t *testing.T) {
			a := form.answers()

			// The scaffold half: the echoed line, through the real parser.
			var errW strings.Builder
			parsed, code := parseNewArgs(a.args(), &errW)
			if code != 0 {
				t.Fatalf("the wizard echoed a command its own parser rejects:\n  %s\n%s",
					a.command(), errW.String())
			}
			d := parsed.data
			// The dev seed and ports are random/derived per scaffold; pin them
			// so the two trees differ only where this test is looking.
			d.JWTSecret, d.DevUser, d.DevPassword = "deadbeef", "dev", "secret"

			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}

			// The add half: the echoed line, through the real verb.
			if add := a.addCommand(); add != "" {
				args := append(strings.Fields(strings.TrimPrefix(add, "ultra add ")), "--dir", dir)
				var out, errOut strings.Builder
				if code := cmdAdd(args, &out, &errOut); code != 0 {
					t.Fatalf("the wizard's own add line failed:\n  %s\n%s", add, errOut.String())
				}
			}

			got, err := os.ReadFile(filepath.Join(dir, "main.go"))
			if err != nil {
				t.Fatal(err)
			}
			// Every echoed preset is present, and app.Modules is still last —
			// the two facts that make the product the echo describes.
			for _, name := range a.Infra {
				if !strings.Contains(string(got), name+".Use()") {
					t.Errorf("main.go is missing %s.Use():\n%s", name, got)
				}
				if strings.Index(string(got), name+".Use()") > strings.Index(string(got), "app.Modules") {
					t.Errorf("%s.Use() landed after app.Modules", name)
				}
			}
			assertGofmt(t, string(got))
		})
	}
}
