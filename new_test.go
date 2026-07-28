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
		"web/.gitignore", "web/e2e/golden.spec.ts", "web/package.json",
		"web/playwright.config.ts", "web/src/app.css", "web/src/app.html",
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
	if err := scaffold(bare, testData("speedcheck", scaffoldData{})); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(bare, "config.toml"))
	if strings.Contains(string(b), "[auth.jwt]") {
		t.Errorf("a product without --auth gets no jwt secret:\n%s", b)
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
