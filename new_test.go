package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// combos are the shapes the scaffold promises: the paved road, the core, and
// the core plus one capability. Everything the covenant test proves, it
// proves for each of them.
var combos = []struct {
	name string
	args []string
	data scaffoldData
}{
	{"default", nil, scaffoldData{DB: true, Web: true, Auth: true, DS: dsConnected}},
	{"bare", []string{"--bare"}, scaffoldData{}},
	{"bare+db", []string{"--bare", "--db"}, scaffoldData{DB: true}},
	// The combination whose boot smoke actually RUNS: no database to dial,
	// so Start happens for real and the auth-guards-ops assertion is proven
	// rather than skipped. It also carries the --ds bare frontend, so the two
	// web legs cover one design system each.
	{"bare+web+auth", []string{"--bare", "--web", "--auth", "--ds", "bare"},
		scaffoldData{Web: true, Auth: true, DS: dsBare}},
}

func testData(name string, d scaffoldData) scaffoldData {
	d.Name, d.Module = name, "example.com/"+name
	d.Version, d.GoVersion = scaffoldVersion, scaffoldGoVersion
	return d
}

func TestScaffoldValidatesInput(t *testing.T) {
	dir := t.TempDir()
	if err := scaffold(filepath.Join(dir, "x"), testData("Bad Name", scaffoldData{})); err == nil {
		t.Fatal("invalid names must be rejected")
	}
	target := filepath.Join(dir, "taken")
	os.MkdirAll(target, 0o755)
	if err := scaffold(target, testData("taken", scaffoldData{})); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("existing dirs must be refused: %v", err)
	}
}

// TestFlagsShapeTheTree pins the doctrine's structure: the core is the core,
// every other file is earned by a flag, and the layout the framework killed
// never comes back.
func TestFlagsShapeTheTree(t *testing.T) {
	core := []string{
		".gitignore", ".mcp.json", "AGENTS.md", "Taskfile.yml", "config.toml", "contract_test.go",
		"go.mod", "internal/app/app.go", "main.go", "main_test.go",
		"messages/en.toml", "messages/th.toml",
	}
	web := []string{
		"spa.go", "spa_embed.go",
		"web/.gitignore", "web/e2e/golden.spec.ts", "web/i18n-allow.txt",
		"web/package.json",
		"web/playwright.config.ts", "web/scripts/check-i18n.ts",
		"web/src/app.css", "web/src/app.html",
		"web/src/lib/api/.gitkeep", "web/src/lib/api/vite.proxy.json",
		"web/src/routes/+layout.svelte", "web/src/routes/+layout.ts",
		"web/src/routes/+page.svelte",
		"web/svelte.config.js", "web/tsconfig.json", "web/vite.config.ts",
	}
	db := []string{
		"internal/db/migrations/00001_init.sql",
		"internal/db/queries/.gitkeep",
		"internal/db/sqlc.yaml",
	}

	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			d := testData("speedcheck", c.data)
			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}
			want := append([]string{}, core...)
			if d.Web {
				want = append(want, web...)
			}
			if d.Web && d.Auth {
				want = append(want, "web/src/routes/login/+page.svelte")
			}
			if d.DS == dsConnected {
				want = append(want, "web/.npmrc")
			}
			if d.DB {
				want = append(want, db...)
			}
			sort.Strings(want)
			got := treeOf(t, dir)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("tree mismatch\n got:\n%s\nwant:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, f := range got {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(b, []byte("{{")) {
					t.Errorf("%s has unrendered template markers", f)
				}
				if len(bytes.TrimSpace(b)) == 0 {
					t.Errorf("%s is empty — a file exists only when it has content", f)
				}
			}
			// The dead v0.2.0 layout, gone for good.
			for _, dead := range []string{"modules.go", "cmd", "internal/ui", "SKILL.md",
				"README.md", ".github", "e2e", "references"} {
				if _, err := os.Stat(filepath.Join(dir, dead)); err == nil {
					t.Errorf("%s must not be scaffolded", dead)
				}
			}
		})
	}
}

// TestGoModWiring: the module path, the current framework version, and the
// commented replace pair that makes a local checkout one uncomment away.
func TestGoModWiring(t *testing.T) {
	d := testData("speedcheck", scaffoldData{DB: true, Web: true, Auth: true})
	d.Module = "github.com/acme/speedcheck"
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"module github.com/acme/speedcheck",
		"go " + scaffoldGoVersion,
		"ultrastack " + scaffoldVersion,
		"ultrastack/contrib " + scaffoldVersion,
		"// replace github.com/bronystylecrazy/ultrastack => ../ultrastack",
		"// replace github.com/bronystylecrazy/ultrastack/contrib => ../ultrastack/contrib",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("go.mod missing %q:\n%s", want, b)
		}
	}
}

// TestI18nCoverageGateIsWired: the checker is worthless unless `bun run check`
// runs it. A scaffolded script nobody invokes is the same blind spot it exists
// to close — catalog parity only audits keys that already exist, so hardcoded
// English otherwise passes every gate.
func TestI18nCoverageGateIsWired(t *testing.T) {
	d := testData("speedcheck", scaffoldData{Web: true})
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	read := func(parts ...string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(append([]string{dir}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	pkg := read("web", "package.json")
	if !strings.Contains(pkg, "bun scripts/check-i18n.ts") {
		t.Errorf("the check script must run the i18n gate:\n%s", pkg)
	}
	if !strings.Contains(pkg, "svelte-check --tsconfig ./tsconfig.json && bun scripts/check-i18n.ts") {
		t.Errorf("the i18n gate runs AFTER svelte-check, in the same `check`:\n%s", pkg)
	}

	// The checker itself: svelte's own parser, the two rule families, and the
	// three directories that are generated or vocabulary rather than copy.
	script := read("web", "scripts", "check-i18n.ts")
	for _, want := range []string{
		"from 'svelte/compiler'",
		"src/lib/api",
		"src/lib/i18n",
		"src/lib/ui",
		"placeholder",
		"aria-label",
		"i18n-allow.txt",
		"process.exit(1)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("check-i18n.ts is missing %q:\n%s", want, script)
		}
	}

	// The allowlist ships explaining itself, and pre-absolves the placeholder
	// copy in the +page.svelte the product owner is about to delete.
	allow := read("web", "i18n-allow.txt")
	if !strings.Contains(allow, "#") {
		t.Errorf("i18n-allow.txt must explain what belongs in it:\n%s", allow)
	}
	page := read("web", "src", "routes", "+page.svelte")
	for _, line := range strings.Split(allow, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		// Whitespace-collapsed, exactly as the checker compares it.
		if !strings.Contains(strings.Join(strings.Fields(page), " "), s) {
			t.Errorf("i18n-allow.txt seeds %q, which the scaffolded +page.svelte does not contain:\n%s", s, page)
		}
	}
}

// TestWebDepsCoverTheGeneratedCode: `./app client` emits create.ts importing
// @tanstack/svelte-query, so the scaffolded package.json must list it. A
// generated import the manifest does not declare is a `bun add` the product
// owner has to discover from a build error.
func TestWebDepsCoverTheGeneratedCode(t *testing.T) {
	d := testData("speedcheck", scaffoldData{Web: true})
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	// v6 is the contract: the runes adapter the generated create*/streams
	// layers are written against (Svelte 5.25+).
	if !strings.Contains(string(b), `"@tanstack/svelte-query": "^6.`) {
		t.Errorf("package.json must declare the TanStack v6 runes adapter:\n%s", b)
	}
	// vite.config.ts and playwright.config.ts read process.env, and svelte-kit's
	// tsconfig includes the config files — without node types `bun run check` is
	// red on a file the product owner never wrote.
	if !strings.Contains(string(b), `"@types/node": "^`) {
		t.Errorf("package.json must declare node types for the config files:\n%s", b)
	}
}

// TestDesignSystemShapesTheFrontend: --ds is recorded nowhere but in the files
// it writes, so the files are what this test reads. Both legs get the same
// Tailwind v4 foundation and the same root shell; only the design system on
// top of it differs.
func TestDesignSystemShapesTheFrontend(t *testing.T) {
	render := func(ds string) func(string) string {
		dir := filepath.Join(t.TempDir(), "speedcheck")
		if err := scaffold(dir, testData("speedcheck", scaffoldData{Web: true, DS: ds})); err != nil {
			t.Fatal(err)
		}
		return func(rel string) string {
			b, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil && !strings.Contains(rel, ".npmrc") {
				t.Fatal(err)
			}
			return string(b)
		}
	}

	connected := render(dsConnected)
	for _, want := range []struct{ file, s string }{
		// Caret-pinned, not pinned to a minor: bumping the design system is a
		// one-line template edit and must not be a test edit as well.
		{"web/package.json", `"@connected/svelte-connected-design": "^`},
		{"web/package.json", `"tailwindcss": "^4`},
		{"web/package.json", `"@tailwindcss/vite": "^4`},
		// The trailing slash is not cosmetic: without it bun mis-joins the
		// scope path and depot answers with an HTML page.
		{"web/.npmrc", "@connected:registry=https://depot.connectedtech.dev/npm/\n"},
		{"web/src/app.css", `@import "@connected/tailwindcss-connected-design/themes/default.css";`},
		// app.css is at web/src, node_modules at web/ — ONE level up. Two
		// resolves to a directory that does not exist, Tailwind reports
		// nothing, and every DS component ships unstyled while the whole
		// toolchain stays green. assertStyled proves the consequence when
		// depot is reachable; this proves the path always.
		{"web/src/app.css", `@source "../node_modules/@connected/svelte-connected-design/dist";`},
		{"web/src/routes/+layout.svelte", `import { Toaster } from '@connected/svelte-connected-design/toast';`},
		{"web/src/routes/+layout.svelte", "<Toaster />"},
		{"web/vite.config.ts", "plugins: [tailwindcss(), sveltekit()]"},
	} {
		if !strings.Contains(connected(want.file), want.s) {
			t.Errorf("--ds connected: %s missing %q:\n%s", want.file, want.s, connected(want.file))
		}
	}
	// The theme file already imports tailwindcss; a second import registers
	// every utility twice.
	if strings.Contains(connected("web/src/app.css"), `@import "tailwindcss"`) {
		t.Errorf("--ds connected must not double-import tailwindcss:\n%s", connected("web/src/app.css"))
	}

	bare := render(dsBare)
	for _, want := range []struct{ file, s string }{
		{"web/package.json", `"tailwindcss": "^4`},
		{"web/src/app.css", `@import "tailwindcss";`},
		{"web/src/app.css", "@theme {"},
		{"web/src/routes/+layout.svelte", "import '../app.css';"},
		{"web/src/routes/+layout.svelte", "{@render children()}"},
	} {
		if !strings.Contains(bare(want.file), want.s) {
			t.Errorf("--ds bare: %s missing %q:\n%s", want.file, want.s, bare(want.file))
		}
	}
	// No design system, and therefore no private registry to reach.
	if strings.Contains(bare("web/package.json"), "@connected/") {
		t.Errorf("--ds bare must not depend on the design system:\n%s", bare("web/package.json"))
	}
	if bare("web/.npmrc") != "" {
		t.Errorf("--ds bare must not write an .npmrc:\n%s", bare("web/.npmrc"))
	}
}

// TestDesignSystemFlag: the two usage errors. A typo'd value must name both
// choices rather than silently scaffolding the default.
func TestDesignSystemFlag(t *testing.T) {
	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"unknown value", "unknown --ds value", []string{"speedcheck", "--ds", "shadcn"}},
		{"without --web", "--ds needs --web", []string{"speedcheck", "--no-web", "--ds", "bare"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errW bytes.Buffer
			if code := cmdNew(c.args, &out, &errW); code != 2 {
				t.Fatalf("must be a usage error, got %d", code)
			}
			if !strings.Contains(errW.String(), c.want) {
				t.Errorf("the error must say %q:\n%s", c.want, errW.String())
			}
			for _, ds := range []string{dsConnected, dsBare} {
				if !strings.Contains(errW.String(), ds) {
					t.Errorf("the usage text must name %q:\n%s", ds, errW.String())
				}
			}
		})
	}
}

// TestAuthScaffoldIsLoginnable pins the dev seed. A product whose login routes
// are mounted over an empty user store with a commented-out signing key boots
// perfectly and can never be signed in to — the worst kind of scaffold bug,
// because nothing says so. So --auth writes both halves, they agree with each
// other, and they are freshly random per product.
func TestAuthScaffoldIsLoginnable(t *testing.T) {
	d := testData("speedcheck", scaffoldData{Auth: true})
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cfg, main, mainTest := read("config.toml"), read("main.go"), read("main_test.go")

	// A real 32-byte key, on an UNCOMMENTED line: a commented one is exactly
	// the auth.jwt_unconfigured failure this exists to prevent.
	secret := regexp.MustCompile(`(?m)^secret = "([0-9a-f]{64})"$`).FindStringSubmatch(cfg)
	if secret == nil {
		t.Fatalf("config.toml must carry a live [auth.jwt] secret:\n%s", cfg)
	}
	// The seeded role must be one [auth.roles] actually grants.
	if !strings.Contains(cfg, `admin = ["*"]`) {
		t.Errorf("[auth.roles] must grant the seeded role:\n%s", cfg)
	}

	// One user, marked as the temporary thing it is, with the credentials the
	// `ultra new` output printed.
	pw := regexp.MustCompile(`auth\.NewUser\("dev", "([0-9a-f]{18})", "admin"\)`).FindStringSubmatch(main)
	if pw == nil {
		t.Fatalf("main.go must seed one dev user:\n%s", main)
	}
	for _, want := range []string{"DEV ONLY — DELETE ME", "authpg.Stores()", "di.Bind[auth.UserStore]"} {
		if !strings.Contains(main, want) {
			t.Errorf("the seed must be loudly temporary, missing %q:\n%s", want, main)
		}
	}
	// ...and the boot smoke actually signs in with them, so "loginnable" is
	// part of the covenant rather than a claim.
	if !strings.Contains(mainTest, `"password":"`+pw[1]+`"`) ||
		!strings.Contains(mainTest, `http.Post(base+"/auth/token"`) {
		t.Errorf("TestBoot must prove a real sign-in with the seeded credentials:\n%s", mainTest)
	}

	// Freshly random per product: no two scaffolds share a secret, so no
	// default can ever leak into production by being the value everybody has.
	other := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(other, testData("speedcheck", scaffoldData{Auth: true})); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(other, "config.toml"))
	if strings.Contains(string(b), secret[1]) {
		t.Error("two products must not share a dev signing key")
	}

	// A --no-auth product carries none of it.
	bare := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(bare, testData("speedcheck", scaffoldData{Web: true, DS: dsBare})); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(bare, "config.toml"))
	if strings.Contains(string(b), "[auth.jwt]") {
		t.Errorf("a product without --auth gets no jwt secret:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(bare, "web", "src", "routes", "login")); err == nil {
		t.Error("a product without --auth gets no login page — there is nothing to sign in to")
	}
	b, _ = os.ReadFile(filepath.Join(bare, "web", "src", "routes", "+layout.ts"))
	if strings.Contains(string(b), "redirect(") {
		t.Errorf("a product without --auth must not guard its own routes:\n%s", b)
	}
}

// TestAuthWebScaffoldHasADoor is the frontend half of "loginnable". Two of
// three products built by agents on the --web --auth scaffold shipped with auth
// machinery and NO way in: every route 401s, and the UI has no screen that
// could ever say who the user is. So the door ships by default — a login page,
// a guard that sends anonymous visitors to it, and a golden path that walks
// through it, all agreeing on the same labels and the same seeded user.
func TestAuthWebScaffoldHasADoor(t *testing.T) {
	for _, ds := range []string{dsConnected, dsBare} {
		t.Run(ds, func(t *testing.T) {
			d := testData("speedcheck", scaffoldData{Web: true, Auth: true, DS: ds})
			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}
			read := func(rel string) string {
				b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			login := read("web/src/routes/login/+page.svelte")
			label := scaffoldData{}.Login()

			// The page signs in through the GENERATED store — not a hand-rolled
			// fetch that has to relearn the cookie, the problem shape, and the
			// reload story — and it is marked as the placeholder it is. Not
			// DELETE ME: a product that enforces auth needs SOME login page.
			for _, want := range []string{
				"import { session } from '$lib/api/auth.svelte'",
				"session.signIn({ username, password })",
				"localize(err)", // the problem's own message, in the reader's language
				"t('login.submit')",
				"REPLACE IT, DO NOT DELETE IT",
			} {
				if !strings.Contains(login, want) {
					t.Errorf("the login page is missing %q:\n%s", want, login)
				}
			}
			if strings.Contains(login, "DELETE ME") {
				t.Errorf("the login page is replaced, never deleted:\n%s", login)
			}
			// DS-composed where there is a design system, and hand-rolled only
			// where there is none.
			if used := strings.Contains(login, "@connected/svelte-connected-design/field"); used != (ds == dsConnected) {
				t.Errorf("--ds %s: Field used = %v:\n%s", ds, used, login)
			}

			// Every visible string is a key, in EVERY catalog: a key in en and
			// not th fails the product's own TestWiring with I18N0202.
			for _, cat := range []string{"messages/en.toml", "messages/th.toml"} {
				for _, key := range []string{"login.title", "login.username", "login.password",
					"login.submit", "auth.invalid_credentials"} {
					if !strings.Contains(read(cat), `"`+key+`"`) {
						t.Errorf("%s is missing %q", cat, key)
					}
				}
			}
			// The English values ARE the labels the spec types into.
			en := read("messages/en.toml")
			for _, v := range []string{label.Username, label.Password, label.Submit} {
				if !strings.Contains(en, `= "`+v+`"`) {
					t.Errorf("messages/en.toml must carry the label %q:\n%s", v, en)
				}
			}

			// The guard: without it the login page is a page nobody is ever
			// sent to, and the product is exactly as unenterable as before.
			layout := read("web/src/routes/+layout.ts")
			for _, want := range []string{"session.authenticated", "redirect(307, '/login')", "'/login'"} {
				if !strings.Contains(layout, want) {
					t.Errorf("+layout.ts must send anonymous visitors to the login page, missing %q:\n%s", want, layout)
				}
			}

			// And the golden path goes through the door rather than around it —
			// with the credentials main.go actually seeds. A spec that still
			// expected the old anonymous landing page would fail on the first
			// run of `task e2e`, which is the covenant this pins.
			pw := regexp.MustCompile(`auth\.NewUser\("dev", "([0-9a-f]{18})"`).FindStringSubmatch(read("main.go"))
			if pw == nil {
				t.Fatal("main.go must seed one dev user")
			}
			spec := read("web/e2e/golden.spec.ts")
			for _, want := range []string{
				"toHaveURL(/\\/login$/)",
				"getByLabel('" + label.Username + "')",
				"getByLabel('" + label.Password + "')",
				"getByRole('button', { name: '" + label.Submit + "' })",
				"?? '" + pw[1] + "'",
			} {
				if !strings.Contains(spec, want) {
					t.Errorf("the golden spec must sign in through the login page, missing %q:\n%s", want, spec)
				}
			}
		})
	}
}

// TestScaffoldVersionTracksRelease keeps scaffoldVersion honest: it is the
// version a new product requires, so it must be the version the repo itself
// releases. contrib/go.mod's requirement on the kernel is bumped by the
// same release commit, which makes it the in-repo source of truth.
func TestScaffoldVersionTracksRelease(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "contrib", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`github\.com/bronystylecrazy/ultrastack (v\d+\.\d+\.\d+)`).
		FindStringSubmatch(string(b))
	if m == nil {
		t.Skip("contrib/go.mod does not pin a released kernel version")
	}
	if m[1] != scaffoldVersion {
		t.Fatalf("scaffoldVersion is %s but the repo releases %s — bump the const "+
			"in cmd/ultra/new.go with the release commit", scaffoldVersion, m[1])
	}
}

// TestVersionResolution: --version wins, and the fallback is the pin.
func TestVersionResolution(t *testing.T) {
	if got := resolveVersion("v1.2.3"); got != "v1.2.3" {
		t.Fatalf("--version must win: %s", got)
	}
	// `go test` builds the main module as "(devel)", so the pin answers.
	if got := resolveVersion(""); got != scaffoldVersion {
		t.Fatalf("a devel build must fall back to the pin: %s", got)
	}
}

func TestNewFeatureScaffold(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if code := cmdNew([]string{"feature", "zones"}, &out, &errW); code != 0 {
		t.Fatalf("new feature failed (%d): %s", code, errW.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "internal", "app", "zones", "zones.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package zones", "func Use() di.Reg", "di.Pkg("} {
		if !strings.Contains(string(b), want) {
			t.Errorf("feature file missing %q:\n%s", want, b)
		}
	}
	// No manifest files: they appear when content demands them.
	entries, _ := os.ReadDir(filepath.Join(dir, "internal", "app", "zones"))
	if len(entries) != 2 {
		t.Errorf("a new feature is its front page plus its error contract — 2 files, got %d", len(entries))
	}
	if !strings.Contains(out.String(), "zones.Use()") {
		t.Errorf("the next step (one line in app.go) must be printed:\n%s", out.String())
	}

	// Re-running must refuse rather than clobber.
	out.Reset()
	errW.Reset()
	if code := cmdNew([]string{"feature", "zones"}, &out, &errW); code == 0 {
		t.Error("an existing feature must not be overwritten")
	}
	// A dashed name is not a Go package name.
	if code := cmdNew([]string{"feature", "my-zones"}, &out, &errW); code == 0 {
		t.Error("dashed feature names must be rejected")
	}
}

// TestScaffoldCovenant is the scaffold dogfooding itself: for every flag
// combination, generate a product against THIS checkout and make it prove
// its own covenant — it builds, and its wiring test plus boot smoke pass.
// The frontend is never built: a dev build serves no frontend by design, so
// bun is not on this path.
func TestScaffoldCovenant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three full products; skipped in -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := testData("speedcheck", c.data)
			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}
			t.Logf("ultra new speedcheck %s\nspeedcheck/\n%s",
				strings.Join(c.args, " "), strings.Join(treeOf(t, dir), "\n"))

			// The MCP wiring is a config file a tool parses, so parse it: a
			// malformed .mcp.json is silently ignored by Claude Code, and
			// "the tools were never there" is not a failure a session reports.
			var mcp struct {
				Servers map[string]struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"mcpServers"`
			}
			raw, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &mcp); err != nil {
				t.Fatalf(".mcp.json is not valid JSON: %v\n%s", err, raw)
			}
			if got := mcp.Servers["ultra"]; got.Command != "ultra" || len(got.Args) != 1 || got.Args[0] != "mcp" {
				t.Errorf(".mcp.json must run `ultra mcp`, got %+v", got)
			}

			sh := func(name string, args ...string) {
				t.Helper()
				cmd := exec.Command(name, args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					if offline(out) {
						t.Skipf("%s %s needs the network: %s", name, strings.Join(args, " "), out)
					}
					t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
				}
				t.Logf("%s %s\n%s", name, strings.Join(args, " "), out)
			}
			// Point the generated product at this checkout instead of the
			// network — the same two lines the go.mod comment describes.
			sh("go", "mod", "edit",
				"-replace="+modulePath+"="+repoRoot,
				"-replace="+modulePath+"/contrib="+filepath.Join(repoRoot, "contrib"))
			sh("go", "mod", "tidy")
			sh("go", "build", "./...")
			sh("go", "test", "./...")

			// The drift gate, proven end to end. The first `go test`
			// BOOTSTRAPPED the committed contract artifacts (nothing can
			// generate them at scaffold time — they come out of the binary
			// that does not exist yet), so they must now be on disk...
			artifacts := []string{"openapi.json"}
			if d.Web {
				// The client AND the typed catalogs ride one bootstrap run.
				artifacts = append(artifacts, "web/src/lib/api/common.ts", "web/src/lib/i18n/index.ts")
			}
			for _, f := range artifacts {
				if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
					t.Fatalf("the drift gate did not bootstrap %s: %v", f, err)
				}
			}
			// ...a second run must be clean (the generators are
			// deterministic, so a committed artifact never drifts on its
			// own)...
			sh("go", "test", "./...")
			// ...and a doctored artifact must fail with the refresh command.
			doc := filepath.Join(dir, "openapi.json")
			original, err := os.ReadFile(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(doc,
				bytes.Replace(original, []byte(`"openapi": "3.1.0"`), []byte(`"openapi": "3.0.0"`), 1),
				0o644); err != nil {
				t.Fatal(err)
			}
			drift := exec.Command("go", "test", "-run", "TestContractDrift", ".")
			drift.Dir = dir
			out, err := drift.CombinedOutput()
			if err == nil {
				t.Errorf("a doctored openapi.json must fail TestContractDrift:\n%s", out)
			}
			if !strings.Contains(string(out), "task contracts") {
				t.Errorf("the drift failure must name the refresh command:\n%s", out)
			}
			t.Logf("drift detected as designed:\n%s", out)
			if err := os.WriteFile(doc, original, 0o644); err != nil {
				t.Fatal(err)
			}

			if d.Web {
				// The prod half of the build-tag pair only compiles once the
				// frontend build exists — stand in for bun (the real build is
				// never on this path) and prove spa_embed.go itself is sound.
				if err := os.MkdirAll(filepath.Join(dir, "web", "build"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "web", "build", "index.html"),
					[]byte("<!doctype html><title>stand-in</title>"), 0o644); err != nil {
					t.Fatal(err)
				}
				sh("go", "build", "-tags", "embedspa", "-o", os.DevNull, ".")

				if d.DS == dsConnected {
					// The ONLY leg that needs depot auth: the design system is
					// a private library. A machine without it must not turn the
					// covenant red, so an unreachable registry skips with the
					// reason — and any OTHER install failure is a real one.
					if _, err := exec.LookPath("bun"); err != nil {
						t.Skipf("no bun, so the design system was never resolved: %v", err)
					}
					install := exec.Command("bun", "install")
					install.Dir = filepath.Join(dir, "web")
					bunOut, err := install.CombinedOutput()
					switch {
					case err == nil:
						t.Logf("bun install\n%s", bunOut)
					case depotOutOfReach(bunOut):
						t.Skipf("the depot registry is out of reach, so --ds connected stops here:\n%s", bunOut)
					default:
						t.Fatalf("bun install failed: %v\n%s", err, bunOut)
					}
					assertStyled(t, filepath.Join(dir, "web"))
					assertI18nCovered(t, filepath.Join(dir, "web"))
				}
			}
		})
	}
}

// offline reports whether a go command failed for want of a network rather
// than for want of correct code.
func offline(out []byte) bool {
	s := string(out)
	for _, sign := range []string{"dial tcp", "no such host", "connection refused",
		"proxy.golang.org", "i/o timeout", "TLS handshake timeout"} {
		if strings.Contains(s, sign) {
			return true
		}
	}
	return false
}

// depotOutOfReach reports whether a `bun install` failed because the private
// registry would not authenticate or could not be dialled, rather than because
// the manifest is wrong. Depot answers an unauthenticated fetch with 401, which
// bun reports as: error: GET <url> - 401. A bad version pin says "failed to
// resolve" with no such line, and must stay a failure.
func depotOutOfReach(out []byte) bool {
	s := string(out)
	for _, sign := range []string{" - 401", " - 403", "ConnectionRefused",
		"ConnectionClosed", "FailedToOpenSocket", "getaddrinfo", "ETIMEDOUT", "timed out"} {
		if strings.Contains(s, sign) {
			return true
		}
	}
	return false
}

// assertStyled is the unstyled-but-green gate, and the only check in this file
// that reads bytes a browser would. `bun install`, `bun run build`, `bun run
// check` and `task test` all stay green while the design system's own
// utilities are missing from the built css, because the classes that vanish
// are the ones written INSIDE DS components and never in product code — one
// wrong `../` in app.css's @source is enough. So the covenant greps the build:
// a utility only the DS writes internally must be in there.
func assertStyled(t *testing.T, web string) {
	t.Helper()
	build := exec.Command("bun", "run", "build")
	build.Dir = web
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("bun run build failed: %v\n%s", err, out)
	}
	var css []byte
	err := filepath.WalkDir(filepath.Join(web, "build"), func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, ".css") {
			return err
		}
		b, err := os.ReadFile(p)
		css = append(css, b...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(css) == 0 {
		t.Fatal("bun run build emitted no css at all")
	}
	for _, want := range []string{".rounded-Pill", ".bg-brand-solid"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("the built css (%d bytes) has no %s rule — @source is not reaching "+
				"the design system's dist, so every component renders unstyled while every check stays green",
				len(css), want)
		}
	}
	t.Logf("bun run build   %d bytes of css, design system utilities present", len(css))
}

// assertI18nCovered is the sibling gate, and the same shape of hole: catalog
// parity (I18N0202) only audits keys that already EXIST, so a screen of
// hardcoded English passes stack.Validate, passes svelte-check, and passes
// every other assertion in this file. The scaffolded checker is what closes
// that — and a gate nobody has watched fail is not a gate, so this runs it
// clean on the scaffold and then plants a literal and demands red.
func assertI18nCovered(t *testing.T, web string) {
	t.Helper()
	run := func() (string, error) {
		cmd := exec.Command("bun", "scripts/check-i18n.ts")
		cmd.Dir = web
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// The generated catalogs exist by now (the bootstrap wrote them), so the
	// gate is live rather than skipping.
	if _, err := os.Stat(filepath.Join(web, "src", "lib", "i18n")); err != nil {
		t.Fatalf("the i18n gate would skip: no generated catalogs to route through: %v", err)
	}
	// The scaffold ships clean — its own placeholder copy is allowlisted.
	if out, err := run(); err != nil {
		t.Fatalf("the scaffolded web/ must pass its own i18n gate: %v\n%s", err, out)
	}
	planted := filepath.Join(web, "src", "routes", "covenant")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(planted)
	if err := os.WriteFile(filepath.Join(planted, "+page.svelte"),
		[]byte("<h1>Untranslated heading</h1>\n<input placeholder=\"Type a name\" />\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err == nil {
		t.Fatalf("hardcoded UI English passed the i18n gate:\n%s", out)
	}
	// The failure has to be mechanical to act on: where, what, and the shape.
	for _, want := range []string{
		"covenant/+page.svelte", "Untranslated heading", "Type a name",
		"placeholder={t(", "i18n-allow.txt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the i18n failure must name %q so the fix needs no thinking:\n%s", want, out)
		}
	}
	t.Logf("bun scripts/check-i18n.ts   clean on the scaffold, red on a planted literal")
}

// treeOf lists every file under dir, slash-separated and sorted.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}
