package main

import (
	"errors"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errBuild stands in for `go build`'s exit status in the --force test.
var errBuild = errors.New("exit status 1")

// writeProduct lays down a fixture product: a go.mod that already requires
// contrib (so no command ever reaches the network) and the main.go under test.
func writeProduct(t *testing.T, mainGo string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", `module example.com/shop

go 1.27rc2

require (
	github.com/bronystylecrazy/ultrastack v0.9.19
	github.com/bronystylecrazy/ultrastack/contrib v0.9.19
)
`)
	write("main.go", mainGo)
	for rel, body := range extra {
		write(rel, body)
	}
	return dir
}

func readMain(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertGofmt is the promise `add`/`remove` make about the file they leave
// behind: gofmt-clean, always. A product's `task check` runs gofmt, so an edit
// that is merely valid would fail the next CI run.
func assertGofmt(t *testing.T, src string) {
	t.Helper()
	want, err := format.Source([]byte(src))
	if err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, src)
	}
	if string(want) != src {
		t.Errorf("result is not gofmt-clean:\n--- got\n%s\n--- want\n%s", src, want)
	}
}

// ---- the canonical root variants ----

// scaffoldRoot is the shape `ultra new` writes: di.Options wrapping ultra.New,
// with app.Modules() last behind its comment.
const scaffoldRoot = `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/api"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
	"github.com/bronystylecrazy/ultrastack/di"

	"example.com/shop/internal/app"
)

const version = "0.1.0"

// App is the root page — the whole product as one value, assigned once.
var App = di.Options(
	ultra.New(
		api.Use(api.Info{Title: "shop", Version: version}),
		api.Docs(),

		// The product. app.go is the ONLY file that knows the feature list.
		app.Modules(),
	),
)

func main() { cli.Run(App) }
`

// bareUltraRoot has no app.Modules() and no di.Options wrapper: the argument
// has to land last.
const bareUltraRoot = `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	Routes(),
)

func main() { cli.Run(App) }
`

// fibRoot is the other bundle, single-line, with app.Modules() present.
const fibRoot = `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/fib"

	"example.com/shop/internal/app"
)

var App = fib.Product(app.Modules())

func main() { cli.Run(App) }
`

// stackRoot uses stack.Product from the root module — no framework import
// sorts after it, so the new one has to land at the end of the group.
const stackRoot = `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/stack"
)

var App = stack.Product(
	Routes(),
)

func main() { cli.Run(App) }
`

func TestContribAddScaffoldRootInsertsBeforeModules(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, nil)
	code, out, errOut := runCLI(t, "contrib", "add", "pg", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)

	want := `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/api"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
	"github.com/bronystylecrazy/ultrastack/di"

	"example.com/shop/internal/app"
)

const version = "0.1.0"

// App is the root page — the whole product as one value, assigned once.
var App = di.Options(
	ultra.New(
		api.Use(api.Info{Title: "shop", Version: version}),
		api.Docs(),

		pg.Use(),
		// The product. app.go is the ONLY file that knows the feature list.
		app.Modules(),
	),
)

func main() { cli.Run(App) }
`
	if got != want {
		t.Errorf("main.go:\n--- got\n%s\n--- want\n%s", got, want)
	}
	// The refresh chain, and the pair hint that is a hint and not magic.
	for _, phrase := range []string{
		"before app.Modules()",
		"go test ./...",
		"[postgres] is new",
		"go run . infra compose --write",
		"ultra contrib add migrate",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("output must mention %q, got:\n%s", phrase, out)
		}
	}
	// migrate is a printed hint, never an edit.
	if strings.Contains(got, "migrate") {
		t.Error("the pair hint must not wire migrate itself")
	}
}

func TestContribAddNoModulesGoesLast(t *testing.T) {
	dir := writeProduct(t, bareUltraRoot, nil)
	if code, _, errOut := runCLI(t, "contrib", "add", "redis", dir); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	want := `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/redis"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	Routes(),
	redis.Use(),
)

func main() { cli.Run(App) }
`
	if got != want {
		t.Errorf("main.go:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestContribAddSingleLineFibRoot(t *testing.T) {
	dir := writeProduct(t, fibRoot, nil)
	if code, _, errOut := runCLI(t, "contrib", "add", "ws", dir); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	if !strings.Contains(got, "fib.Product(ws.Use(), app.Modules())") {
		t.Errorf("ws.Use() must land before app.Modules() on the one line:\n%s", got)
	}
	if !strings.Contains(got, `"github.com/bronystylecrazy/ultrastack/contrib/fib"`+"\n\t"+`"github.com/bronystylecrazy/ultrastack/contrib/ws"`) {
		t.Errorf("the import must sort into the framework group:\n%s", got)
	}
}

func TestContribAddStackRoot(t *testing.T) {
	dir := writeProduct(t, stackRoot, nil)
	if code, _, errOut := runCLI(t, "contrib", "add", "zlog", dir); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	want := `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/zlog"
	"github.com/bronystylecrazy/ultrastack/stack"
)

var App = stack.Product(
	Routes(),
	zlog.Use(),
)

func main() { cli.Run(App) }
`
	if got != want {
		t.Errorf("main.go:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// api's Use takes a required Info, so a bare api.Use() would not compile — the
// inserted call carries the product's own name.
func TestContribAddAPICarriesTheProductName(t *testing.T) {
	dir := writeProduct(t, bareUltraRoot, nil)
	if code, _, errOut := runCLI(t, "contrib", "add", "api", dir); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	if !strings.Contains(got, `api.Use(api.Info{Title: "shop", Version: "0.1.0"})`) {
		t.Errorf("api.Use must carry an Info:\n%s", got)
	}
}

func TestContribAddDryWritesNothing(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, nil)
	code, out, errOut := runCLI(t, "contrib", "add", "pg", dir, "--dry")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if got := readMain(t, dir); got != scaffoldRoot {
		t.Errorf("--dry must not write:\n%s", got)
	}
	for _, phrase := range []string{
		"@@ imports @@",
		`+ 	"github.com/bronystylecrazy/ultrastack/contrib/pg"`,
		"@@ var App @@",
		"+ 		pg.Use(),",
		"nothing written",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("--dry output must contain %q, got:\n%s", phrase, out)
		}
	}
}

// The assembly is the authority on "already wired". A feature package that
// imports contrib/pg for *pg.DB is exactly the product that still needs
// pg.Use() — reading that import as a wiring would refuse the one edit that
// fixes it (and leave a DI0001 at Validate).
func TestContribAddWhenAFeatureAlreadyImportsThePreset(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, map[string]string{
		"internal/app/app.go": `package app

import (
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/di"
)

func Modules() di.Reg { return di.Options(di.Provide(NewRepo)) }

type Repo struct{ DB *pg.DB }

func NewRepo(db *pg.DB) *Repo { return &Repo{db} }
`,
	})
	code, out, errOut := runCLI(t, "contrib", "add", "pg", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if strings.Contains(out, "already wired") {
		t.Fatalf("an import in a feature is not a wiring:\n%s", out)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	if !strings.Contains(got, "\t\tpg.Use(),\n") {
		t.Errorf("pg.Use() must land in the assembly:\n%s", got)
	}
}

// main.go may already import the preset for a type; then the argument is the
// whole edit and the import block is left alone.
func TestContribAddDoesNotDuplicateAnExistingImport(t *testing.T) {
	dir := writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/redis"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	Routes(),
)

func tune(c *redis.Config) {}

func main() { cli.Run(App) }
`, nil)
	if code, _, errOut := runCLI(t, "contrib", "add", "redis", dir); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	if n := strings.Count(got, `"github.com/bronystylecrazy/ultrastack/contrib/redis"`); n != 1 {
		t.Errorf("the import must appear once, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "\tredis.Use(),\n") {
		t.Errorf("the registration must land:\n%s", got)
	}
}

func TestContribAddIsIdempotent(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, nil)
	runCLI(t, "contrib", "add", "pg", dir)
	first := readMain(t, dir)
	code, out, _ := runCLI(t, "contrib", "add", "pg", dir)
	if code != 0 || !strings.Contains(out, "already wired") {
		t.Fatalf("a second add must be a no-op: code=%d out=%s", code, out)
	}
	if readMain(t, dir) != first {
		t.Error("a second add must not touch the file")
	}
}

// ---- refusals ----

func TestContribAddRefusesNonCanonicalRoots(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"no var App", `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

func main() { cli.Run(ultra.New()) }
`, "var App = …"},
		{"reassigned", `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
	"github.com/bronystylecrazy/ultrastack/di"
)

var App = ultra.New()

func init() { App = di.Options(App) }

func main() { cli.Run(App) }
`, "reassigned"},
		{"unknown bundle", `package main

import "github.com/bronystylecrazy/ultrastack/cli"

var App = Assemble()

func main() { cli.Run(App) }
`, "no known bundle call"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeProduct(t, tc.src, nil)
			code, _, errOut := runCLI(t, "contrib", "add", "pg", dir)
			if code != 1 {
				t.Fatalf("code=%d err=%s", code, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("refusal must say %q, got:\n%s", tc.want, errOut)
			}
			// Every refusal hands back the manual one-liner.
			for _, phrase := range []string{"by hand", "pg.Use()", "contrib/pg"} {
				if !strings.Contains(errOut, phrase) {
					t.Errorf("refusal must mention %q, got:\n%s", phrase, errOut)
				}
			}
			if readMain(t, dir) != tc.src {
				t.Error("a refusal must write nothing")
			}
		})
	}
}

// TestContribUpgradePointsUp: presets version in lockstep, so `contrib
// upgrade` is a real verb filed one level too deep — the error must point at
// `ultra upgrade`, not shrug.
func TestContribUpgradePointsUp(t *testing.T) {
	code, _, errOut := runCLI(t, "contrib", "upgrade")
	if code != 2 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(errOut, `Did you mean "ultra upgrade"?`) {
		t.Errorf("must point at the top-level command, got:\n%s", errOut)
	}
}

func TestContribAddUnknownPreset(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, nil)
	code, _, errOut := runCLI(t, "contrib", "add", "postgres", dir)
	if code != 2 || !strings.Contains(errOut, "unknown preset") {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
}

// ---- list ----

func TestContribList(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, map[string]string{
		"internal/app/app.go": `package app

import (
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/di"
)

func Modules() di.Reg { return di.Options(di.Provide(func(db *pg.DB) *Repo { return &Repo{db} })) }

type Repo struct{ db *pg.DB }
`,
	})
	code, out, errOut := runCLI(t, "contrib", "list", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	for _, phrase := range []string{
		"root: ultra.New at main.go:",
		"WIRED (2)",
		`api  api.Use(api.Info{Title: "shop", Version: version})`,
		"main.go:17",
		"internal/app/app.go:4",
		"AVAILABLE (20)",
		"migrate    goose migrations, applied before anything serves",
		"redis      the Redis client",
		"([redis], dev infra)",
	} {
		if !strings.Contains(out, phrase) {
			t.Errorf("list output must contain %q, got:\n%s", phrase, out)
		}
	}
	// pg is imported by a feature package but nothing registers it — the
	// shape of a DI0001 waiting at Validate, so the row has to say so.
	if !strings.Contains(out, "(imported; no pg.Use() — not registered)") {
		t.Errorf("an import without a Use() must say so, got:\n%s", out)
	}
	// A non-canonical root is a note in list, never a failure.
	if strings.Contains(out, "not canonical") {
		t.Errorf("the scaffold root is canonical, got:\n%s", out)
	}
}

func TestContribListNonCanonicalRootIsANote(t *testing.T) {
	dir := writeProduct(t, `package main

import "github.com/bronystylecrazy/ultrastack/contrib/ws"

func main() { _ = ws.Use }
`, nil)
	code, out, _ := runCLI(t, "contrib", "list", dir)
	if code != 0 {
		t.Fatalf("list must not fail on a non-canonical root: %d", code)
	}
	if !strings.Contains(out, "root: not canonical") || !strings.Contains(out, "WIRED (1)") {
		t.Errorf("got:\n%s", out)
	}
}

// ---- remove ----

// authRoot wires five auth registrations — the case that makes removal an AST
// job rather than a line delete.
const authRoot = `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/auth"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"

	"example.com/shop/internal/app"
)

var App = ultra.New(
	// Secure by default.
	auth.Use(),
	auth.JWT(),
	auth.TokenRoutes,
	auth.LoginRoutes,
	auth.Protect, // the enforcement itself

	app.Modules(),
)

func main() { cli.Run(App) }
`

func TestContribRemoveEveryRegistrationAndTheImport(t *testing.T) {
	dir := writeProduct(t, authRoot, nil)
	code, out, errOut := runCLI(t, "contrib", "remove", "auth", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "auth: 5 argument(s) in ultra.New") {
		t.Errorf("output must name all five, got:\n%s", out)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	want := `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"

	"example.com/shop/internal/app"
)

var App = ultra.New(
	app.Modules(),
)

func main() { cli.Run(App) }
`
	if got != want {
		t.Errorf("main.go:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if !strings.Contains(out, "dropped the github.com/bronystylecrazy/ultrastack/contrib/auth import") {
		t.Errorf("output must report the dropped import, got:\n%s", out)
	}
	if !strings.Contains(out, "delete [auth]") || !strings.Contains(out, "FAILS BOOT") {
		t.Errorf("the orphaned config section must be a next step, got:\n%s", out)
	}
}

// The import survives when main.go still spells the package — a helper, a
// type, anything outside the assembly.
func TestContribRemoveKeepsAStillUsedImport(t *testing.T) {
	dir := writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
	"github.com/bronystylecrazy/ultrastack/contrib/ws"
)

var App = ultra.New(
	ws.Use(),
)

func broadcast(h *ws.Hub) {}

func main() { cli.Run(App) }
`, nil)
	if code, _, errOut := runCLI(t, "contrib", "remove", "ws", dir); code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	if !strings.Contains(got, `"github.com/bronystylecrazy/ultrastack/contrib/ws"`) {
		t.Errorf("the import is still used by broadcast — it must stay:\n%s", got)
	}
	if strings.Contains(got, "ws.Use()") {
		t.Errorf("the registration must be gone:\n%s", got)
	}
}

// Guard (a): a product package consumes what the preset provides.
func TestContribRemoveGuardsProductImporters(t *testing.T) {
	dir := writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"

	"example.com/shop/internal/app"
)

var App = ultra.New(
	pg.Use(),
	app.Modules(),
)

func main() { cli.Run(App) }
`, map[string]string{
		"internal/app/orders/orders.go": `package orders

import "github.com/bronystylecrazy/ultrastack/contrib/pg"

type Repo struct{ DB *pg.DB }
`,
	})
	before := readMain(t, dir)
	code, out, errOut := runCLI(t, "contrib", "remove", "pg", dir)
	if code != 1 {
		t.Fatalf("the guard must refuse: code=%d out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "internal/app/orders imports contrib/pg") {
		t.Errorf("the finding must name the package, got:\n%s", out)
	}
	if !strings.Contains(out, "DI0001") {
		t.Errorf("the finding must name the diagnostic, got:\n%s", out)
	}
	if !strings.Contains(errOut, "--force") {
		t.Errorf("the refusal must name the escape hatch, got:\n%s", errOut)
	}
	if readMain(t, dir) != before {
		t.Error("a refused removal must write nothing")
	}
}

// Guard (b): another wired preset needs this one.
func TestContribRemoveGuardsWiredPresets(t *testing.T) {
	dir := writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/migrate"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	pg.Use(),
	migrate.Use(),
)

func main() { cli.Run(App) }
`, nil)
	code, out, errOut := runCLI(t, "contrib", "remove", "pg", dir)
	if code != 1 {
		t.Fatalf("the guard must refuse: code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "migrate is wired and needs pg") ||
		!strings.Contains(out, "migrate.NewRunner(db *pg.DB") {
		t.Errorf("the finding must cite what it read in contrib, got:\n%s", out)
	}
	if !strings.Contains(errOut, "refusing to remove pg") {
		t.Errorf("got:\n%s", errOut)
	}
}

// The conf pair is derived from the catalog's Section column, so it can never
// drift from it.
func TestContribRemoveGuardsConfSections(t *testing.T) {
	dir := writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/conf"
	"github.com/bronystylecrazy/ultrastack/contrib/redis"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	conf.Use(),
	redis.Use(),
)

func main() { cli.Run(App) }
`, nil)
	code, out, _ := runCLI(t, "contrib", "remove", "conf", dir)
	if code != 1 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if !strings.Contains(out, "redis is wired and needs conf") || !strings.Contains(out, "[redis]") {
		t.Errorf("got:\n%s", out)
	}
}

func TestContribRemoveDryReportsTheGuardAndWritesNothing(t *testing.T) {
	src := `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/migrate"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	pg.Use(),
	migrate.Use(),
)

func main() { cli.Run(App) }
`
	dir := writeProduct(t, src, nil)
	code, out, errOut := runCLI(t, "contrib", "remove", "pg", dir, "--dry")
	if code != 0 {
		t.Fatalf("--dry never fails: code=%d err=%s", code, errOut)
	}
	for _, phrase := range []string{"pg: 1 argument(s)", "- pg.Use()", "DEPENDENCY GUARD (1)", "would REFUSE"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("--dry must contain %q, got:\n%s", phrase, out)
		}
	}
	if readMain(t, dir) != src {
		t.Error("--dry must not write")
	}
}

func TestContribRemoveForceKeepsTheEditWhenTheBuildFails(t *testing.T) {
	dir := writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/cli"
	"github.com/bronystylecrazy/ultrastack/contrib/migrate"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
)

var App = ultra.New(
	pg.Use(),
	migrate.Use(),
)

func main() { cli.Run(App) }
`, nil)
	orig := goBuild
	goBuild = func(string) (string, error) {
		return "./main.go:11:2: undefined: pg", errBuild
	}
	defer func() { goBuild = orig }()

	code, out, errOut := runCLI(t, "contrib", "remove", "pg", dir, "--force")
	if code != 1 {
		t.Fatalf("a failed build reports non-zero: code=%d", code)
	}
	got := readMain(t, dir)
	assertGofmt(t, got)
	if strings.Contains(got, "pg.Use()") || strings.Contains(got, "contrib/pg") {
		t.Errorf("--force must have removed it:\n%s", got)
	}
	if !strings.Contains(errOut, "the edit is KEPT") || !strings.Contains(errOut, "undefined: pg") {
		t.Errorf("the failure must be reported without reverting, got:\n%s", errOut)
	}
	if !strings.Contains(out, "go test ./...") {
		t.Errorf("the next steps still print, got:\n%s", out)
	}
}

func TestContribRemoveForceBuildsClean(t *testing.T) {
	dir := writeProduct(t, bareUltraRoot, nil)
	runCLI(t, "contrib", "add", "redis", dir)
	orig := goBuild
	goBuild = func(string) (string, error) { return "", nil }
	defer func() { goBuild = orig }()

	code, out, errOut := runCLI(t, "contrib", "remove", "redis", dir, "--force")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "go build ./... ok") {
		t.Errorf("got:\n%s", out)
	}
	if readMain(t, dir) != bareUltraRoot {
		t.Errorf("add then remove must round-trip:\n%s", readMain(t, dir))
	}
}

func TestContribRemoveNotWired(t *testing.T) {
	dir := writeProduct(t, scaffoldRoot, nil)
	code, out, _ := runCLI(t, "contrib", "remove", "pg", dir)
	if code != 0 || !strings.Contains(out, "pg is not wired") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

// ---- the catalog ----

// TestPresetCatalogMatchesContrib keeps the static table honest: every contrib
// package that exports `func Use(` is a preset, and every preset is a package.
// The exceptions are named here with their reason, so a new one is a conscious
// act rather than a silent divergence.
func TestPresetCatalogMatchesContrib(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Not presets, on purpose:
	//   ultra/fib  product ROOTS — the bundle call `add` edits, not an argument
	//   testkit    a test helper (testkit.Product(t, …)), never assembled
	//   internal   not importable
	skip := map[string]bool{"ultra": true, "fib": true, "testkit": true, "internal": true}

	entries, err := os.ReadDir(filepath.Join(root, "contrib"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || skip[e.Name()] {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, "contrib", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, "contrib", e.Name(), f.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "\nfunc Use(") {
				found[e.Name()] = true
			}
		}
	}
	for name := range found {
		if presetByName(name) == nil {
			t.Errorf("contrib/%s exports Use() but is missing from the catalog in contrib.go", name)
		}
	}
	for _, p := range presets {
		if !found[p.Pkg] {
			t.Errorf("the catalog lists %q, but contrib/%s exports no Use()", p.Pkg, p.Pkg)
		}
	}
}

// Every dependency the guard asserts must be between two catalog entries —
// a typo in the table would be a refusal nobody can act on.
func TestPresetDepsAreCatalogEntries(t *testing.T) {
	for _, d := range presetDeps {
		if presetByName(d.Dependent) == nil {
			t.Errorf("presetDeps: unknown dependent %q", d.Dependent)
		}
		if presetByName(d.Needs) == nil {
			t.Errorf("presetDeps: unknown need %q", d.Needs)
		}
		if d.Why == "" {
			t.Errorf("presetDeps: %s→%s asserts no evidence", d.Dependent, d.Needs)
		}
	}
}
