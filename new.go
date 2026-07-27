package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"go/format"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"text/template"
)

//go:embed all:templates
var templates embed.FS

// scaffoldVersion pins the framework version generated products require. It
// rides the release train: bump it in the `release: vX.Y.Z` commit (the
// version-drift test below keeps it honest against contrib/go.mod). An
// `ultra` installed from a tag reports the same version through
// debug.ReadBuildInfo, which wins over this constant; `--version` wins over
// both.
const scaffoldVersion = "v0.9.24"

// scaffoldGoVersion is the go directive for generated products. It must be
// at least the framework's own, or the toolchain refuses the dependency.
const scaffoldGoVersion = "1.26.3"

// modulePath is the framework's root module; the contrib module hangs off it.
const modulePath = "github.com/bronystylecrazy/ultrastack"

// privateGlob routes module resolution for the platform's private repos
// straight to git (which carries your GitHub credentials), root module
// included — GOPRIVATE globs do not descend into the module they name.
const privateGlob = "github.com/bronystylecrazy/*"

// The --ds values. The company design system is a PRIVATE library on the
// depot registry, so `bare` exists for a machine (or a product) without it:
// the same Tailwind foundation, no DS.
const (
	dsConnected = "connected"
	dsBare      = "bare"
)

// scaffoldData is what every template sees. The three capability booleans
// are the whole story: a file exists only when a flag asks for it, and the
// templates never emit a placeholder for a capability that is off.
type scaffoldData struct {
	Name      string // the product (also the binary and the log/module name)
	Module    string // the Go module path
	Version   string // framework version the product requires
	GoVersion string // the go directive
	DB        bool   // --db:   pg + migrate + internal/db
	Web       bool   // --web:  SPA seam + web/ SvelteKit skeleton
	Auth      bool   // --auth: contrib/auth wired and enforcing
	DS        string // --ds:   the frontend's design system (--web only)

	// The --auth dev seed. A scaffold that mounts login routes over an empty
	// user store and a commented-out signing key is un-loginnable on arrival,
	// silently — POST /auth/token can only answer auth.jwt_unconfigured or bad
	// credentials. These two make the generated product actually work the
	// minute it boots, and both are freshly random per scaffold so no two
	// products ever share one (and no default can leak into production by
	// being the value everybody has). Filled by fillDevSeed.
	JWTSecret   string // [auth.jwt] secret, hex, 32 bytes
	DevUser     string // the seeded username
	DevPassword string // the seeded password, printed once by `ultra new`

	// --from: the legacy-service on-ramp. Features is the package list app.go
	// renders (empty for a forward scaffold, so that file is byte-identical
	// either way); plan carries everything the feature files need. Reverse
	// scaffolding ADDS to the normal tree — it never replaces it, so a product
	// born from an OpenAPI document is the same product as any other.
	Features []string
	From     string // the source document's name, for the generated prose
	plan     *fromPlan
}

// fillDevSeed generates the --auth dev credentials if they are not already
// set. Called by cmdNew (which prints them) and by scaffold (so a direct
// caller — a test, an embedder — never renders a product with a blank signing
// key). Idempotent: already-set values are kept.
func (d *scaffoldData) fillDevSeed() error {
	if !d.Auth {
		return nil
	}
	if d.DevUser == "" {
		d.DevUser = "dev"
	}
	if d.JWTSecret == "" {
		// 32 bytes → 64 hex chars, comfortably over the HS256 minimum that
		// auth.Config.Validate enforces.
		s, err := randomHex(32)
		if err != nil {
			return err
		}
		d.JWTSecret = s
	}
	if d.DevPassword == "" {
		p, err := randomHex(9) // 18 hex chars: typeable, not guessable
		if err != nil {
			return err
		}
		d.DevPassword = p
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a dev secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// pkgNameRe is stricter than nameRe: a feature becomes a Go package, and Go
// package names carry no dashes.
var pkgNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// scaffoldFiles maps template path → output path for one flag combination.
// Ordering is irrelevant (each render is independent); grouping is by the
// flag that earns the file.
func scaffoldFiles(d scaffoldData) map[string]string {
	files := map[string]string{
		"go.mod.tmpl":       "go.mod",
		"main.go.tmpl":      "main.go",
		"main_test.go.tmpl": "main_test.go",
		"config.toml.tmpl":  "config.toml",
		"Taskfile.yml.tmpl": "Taskfile.yml",
		"gitignore.tmpl":    ".gitignore",
		// The cross-tool agent entry point. A ROUTER, not the manual: the
		// loop, the verbs, the laws, and where the vendored doctrine lives.
		"AGENTS.md.tmpl": "AGENTS.md",
		// The drift gate: the committed contract artifacts are regenerated
		// and diffed on every `go test`. The artifacts themselves are NOT
		// scaffolded — nothing here can run the binary it just wrote, so the
		// test bootstraps them on its first run and then guards them.
		"contract_test.go.tmpl": "contract_test.go",
		// The product itself: the ONLY file that knows the feature list.
		"app.go.tmpl": "internal/app/app.go",
		// The committed message catalogs [i18n] enables — codes on the wire,
		// words here; completeness is gated by TestWiring.
		"messages/en.toml.tmpl": "messages/en.toml",
		"messages/th.toml.tmpl": "messages/th.toml",
	}
	if d.Web {
		// The embedspa build-tag pair: dev serves no frontend by design.
		files["spa.go.tmpl"] = "spa.go"
		files["spa_embed.go.tmpl"] = "spa_embed.go"
		// A minimal SvelteKit SPA, inside the module (go:embed cannot reach
		// a sibling directory).
		files["web/package.json.tmpl"] = "web/package.json"
		files["web/svelte.config.js.tmpl"] = "web/svelte.config.js"
		files["web/vite.config.ts.tmpl"] = "web/vite.config.ts"
		files["web/tsconfig.json.tmpl"] = "web/tsconfig.json"
		files["web/gitignore.tmpl"] = "web/.gitignore"
		files["web/src/app.html.tmpl"] = "web/src/app.html"
		files["web/src/routes/layout.ts.tmpl"] = "web/src/routes/+layout.ts"
		files["web/src/routes/layout.svelte.tmpl"] = "web/src/routes/+layout.svelte"
		files["web/src/routes/page.svelte.tmpl"] = "web/src/routes/+page.svelte"
		// The stylesheet the root shell imports — the design system under
		// --ds connected, Tailwind and an empty @theme under --ds bare.
		files["web/src/app.css.tmpl"] = "web/src/app.css"
		if d.DS == dsConnected {
			// The @connected scope resolves through depot, not npm.
			files["web/npmrc.tmpl"] = "web/.npmrc"
		}
		files["web/src/lib/api/gitkeep.tmpl"] = "web/src/lib/api/.gitkeep"
		// A seed of the generated dev proxy so `task dev:web` forwards
		// correctly before the first `task gen`; that command rewrites it.
		files["web/src/lib/api/vite.proxy.json.tmpl"] = "web/src/lib/api/vite.proxy.json"
		// The golden path: ONE browser spec, inside web/ so it resolves
		// through the install the frontend already needs. `task e2e` builds,
		// boots, runs, and kills — and skips with a note without bun/npx.
		files["web/playwright.config.ts.tmpl"] = "web/playwright.config.ts"
		files["web/e2e/golden.spec.ts.tmpl"] = "web/e2e/golden.spec.ts"
	}
	if d.DB {
		// Central persistence: ONE migration line, per-domain query files,
		// ONE generated package (internal/db/gen).
		files["db/migrations/00001_init.sql.tmpl"] = "internal/db/migrations/00001_init.sql"
		files["db/queries/gitkeep.tmpl"] = "internal/db/queries/.gitkeep"
		files["db/sqlc.yaml.tmpl"] = "internal/db/sqlc.yaml"
	}
	return files
}

// scaffold renders the product skeleton into dir. Pure generation — no
// network, no exec — so it is fully testable.
func scaffold(dir string, d scaffoldData) error {
	if !nameRe.MatchString(d.Name) {
		return fmt.Errorf("product name %q must be lowercase letters, digits, and dashes, starting with a letter", d.Name)
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", dir)
	}
	if err := d.fillDevSeed(); err != nil {
		return err
	}
	// Defaulted here as well as in cmdNew so a direct caller — a test, an
	// embedder — never renders a web/ whose app.css has no design system.
	if d.Web && d.DS == "" {
		d.DS = dsConnected
	}
	if d.plan != nil && d.From == "" {
		d.From = d.plan.Source
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := renderAll(dir, scaffoldFiles(d), d); err != nil {
		return err
	}
	return renderFeatures(dir, d)
}

// renderFeatures writes the --from feature packages. Each feature is rendered
// on its own, against its own data, through the same renderAll — so generated
// Go still goes through go/format and a template that emits garbage is an
// error here rather than a broken product.
func renderFeatures(dir string, d scaffoldData) error {
	if d.plan == nil {
		return nil
	}
	for _, f := range d.plan.Features {
		f.Source = d.plan.Source
		base := filepath.Join("internal", "app", f.Pkg)
		files := map[string]string{
			"from/feature.go.tmpl": filepath.Join(base, f.Pkg+".go"),
			"from/handler.go.tmpl": filepath.Join(base, "handler.go"),
			"from/errors.go.tmpl":  filepath.Join(base, "errors.go"),
		}
		if f.HasTypes {
			files["from/types.go.tmpl"] = filepath.Join(base, "types.go")
		}
		if err := renderAll(dir, files, f); err != nil {
			return fmt.Errorf("feature %s: %w", f.Pkg, err)
		}
	}
	return nil
}

// renderAll executes each template against data and writes it to its output
// path under dir, creating parent directories on the way. Generated Go is
// run through go/format, so a template may write conditional blocks without
// hand-aligning them — and a template that renders unparseable Go is an
// error here rather than a broken scaffold.
func renderAll(dir string, files map[string]string, data any) error {
	for tmpl, out := range files {
		t, err := template.ParseFS(templates, "templates/"+tmpl)
		if err != nil {
			return fmt.Errorf("template %s: %w", tmpl, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return fmt.Errorf("render %s: %w", out, err)
		}
		body := buf.Bytes()
		if strings.HasSuffix(out, ".go") {
			formatted, err := format.Source(body)
			if err != nil {
				return fmt.Errorf("template %s renders invalid Go: %w", tmpl, err)
			}
			body = formatted
		}
		target := filepath.Join(dir, out)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// releaseTagRe matches a plain release version and nothing else — not
// "(devel)", and not a pseudo-version like
// v0.9.15-0.20260725102641-053d4689b9a1+dirty, which a `go build` from a
// working tree stamps and which no product could ever resolve.
var releaseTagRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// resolveVersion answers "which framework version should the new product
// require": the explicit flag, else the release this `ultra` binary was
// installed from (`go install ...@v0.9.14` reports it), else the pin.
func resolveVersion(flag string) string {
	if flag != "" {
		return flag
	}
	if bi, ok := debug.ReadBuildInfo(); ok && releaseTagRe.MatchString(bi.Main.Version) {
		return bi.Main.Version
	}
	return scaffoldVersion
}

// cmdNew implements `ultra new <name> [flags]` and `ultra new feature <name>`.
func cmdNew(args []string, out, errW io.Writer) int {
	if len(args) > 0 && args[0] == "feature" {
		return cmdNewFeature(args[1:], out, errW)
	}

	var name, module, version, from string
	// Defaults on; --bare is the subtractive switch, and it is applied
	// before the additive ones so `--bare --db` means "core plus db"
	// regardless of the order they were typed in.
	d := scaffoldData{DB: true, Web: true, Auth: true}
	for _, a := range args {
		if a == "--bare" {
			d.DB, d.Web, d.Auth = false, false, false
		}
	}
	rest := args
	for len(rest) > 0 {
		a := rest[0]
		switch {
		case a == "--bare":
			rest = rest[1:]
		case a == "--db", a == "--web", a == "--auth",
			a == "--no-db", a == "--no-web", a == "--no-auth":
			on := !strings.HasPrefix(a, "--no-")
			switch strings.TrimPrefix(strings.TrimPrefix(a, "--no-"), "--") {
			case "db":
				d.DB = on
			case "web":
				d.Web = on
			case "auth":
				d.Auth = on
			}
			rest = rest[1:]
		case a == "--module" && len(rest) > 1:
			module, rest = rest[1], rest[2:]
		case a == "--version" && len(rest) > 1:
			version, rest = rest[1], rest[2:]
		case a == "--from" && len(rest) > 1:
			from, rest = rest[1], rest[2:]
		case a == "--ds" && len(rest) > 1:
			d.DS, rest = rest[1], rest[2:]
		case strings.HasPrefix(a, "-"):
			node := ultraTree().find("new")
			fmt.Fprintf(errW, "Error: unknown flag %q for %q\n\n", a, node.path())
			node.help(errW)
			return 2
		default:
			if name != "" {
				ultraTree().find("new").help(errW)
				return 2
			}
			name, rest = a, rest[1:]
		}
	}
	if name == "" {
		ultraTree().find("new").help(errW)
		return 2
	}
	switch {
	case d.DS != "" && d.DS != dsConnected && d.DS != dsBare:
		fmt.Fprintf(errW, "Error: unknown --ds value %q — use %s (the company design system) or %s (Tailwind alone)\n\n",
			d.DS, dsConnected, dsBare)
		ultraTree().find("new").help(errW)
		return 2
	case d.DS != "" && !d.Web:
		fmt.Fprintf(errW, "Error: --ds needs --web — a design system with no frontend to style\n\n")
		ultraTree().find("new").help(errW)
		return 2
	case d.Web && d.DS == "":
		d.DS = dsConnected
	}
	if module == "" {
		module = "example.com/" + name
	}
	d.Name, d.Module = name, module
	d.Version, d.GoVersion = resolveVersion(version), scaffoldGoVersion
	// Generated here rather than inside scaffold so the credentials can be
	// printed below — a dev login nobody is told about is the silence this
	// exists to end.
	if err := d.fillDevSeed(); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new "+name, err.Error())
		return 1
	}
	// --from is parsed BEFORE anything is written: a document that cannot be
	// read must not leave half a product on disk.
	if from != "" {
		plan, err := planFrom(from)
		if err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "new "+name, err.Error())
			return 1
		}
		d.plan, d.Features = plan, plan.PkgNames()
	}

	if err := scaffold(name, d); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new "+name, err.Error())
		return 1
	}
	fmt.Fprintf(out, "created %s/ (module %s, ultrastack %s%s)\n",
		name, module, d.Version, capsSuffix(d))

	// Resolve dependencies. The platform repo is private: route module
	// resolution straight to git (which carries your GitHub credentials).
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = name
	tidy.Env = append(os.Environ(), "GOPRIVATE="+privateGlob)
	tidy.Stdout = out
	tidy.Stderr = errW
	if err := tidy.Run(); err != nil {
		fmt.Fprintf(errW, `
go mod tidy did not finish (%v). To resolve manually:
  cd %s
  GOPRIVATE=%s go mod tidy
(private repos need git auth: gh auth setup-git, or SSH; or uncomment the
replace directives in go.mod to build against a local checkout)
`, err, name, privateGlob)
		failVerdict(errW, "new "+name, "scaffolded, but go mod tidy did not finish")
		return 1
	}

	fmt.Fprintf(out, "\n%s is ready:\n  cd %s\n  task test        # the covenant: wiring + boot, then the contract gate\n  task dev:api     # serve on :8080 (a dev build serves NO frontend — by design)\n",
		name, name)
	if d.Web {
		fmt.Fprint(out, "  task dev:web     # the SvelteKit dev server (bun install first)\n")
		fmt.Fprint(out, "  task e2e         # the golden path in a browser (bun + npx playwright)\n")
		if d.DS == dsConnected {
			fmt.Fprint(out, `
web/ is wired to @connected/svelte-connected-design, and web/.npmrc points the
@connected scope at depot — `+"`bun install`"+` needs depot auth. Without it,
scaffold with --ds bare: the same Tailwind foundation, no design system.
`)
		}
	}
	fmt.Fprintf(out, "\nThe first `task test` writes the committed contract artifacts —\nopenapi.json%s. Add them to the first commit:\nfrom then on a contract change that forgets `task contracts` fails the test.\n",
		map[bool]string{true: " and web/src/lib/api", false: ""}[d.Web])
	if d.Auth {
		fmt.Fprintf(out, `
  dev login: %s / %s

That user is seeded in main.go and the [auth.jwt] secret in config.toml is
freshly random — the product is loginnable the minute it boots. BOTH are
DEV ONLY: delete the seed and wire authpg.Stores() for real users, and move
the secret to ULTRA_AUTH_JWT_SECRET, before this serves anyone but you.
`, d.DevUser, d.DevPassword)
	}
	if d.plan != nil {
		// The migration report is the LAST thing printed and the first thing
		// read: what came across, what was guessed, and what did not — with
		// the paths that carry it.
		fmt.Fprint(out, d.plan.report.String())
		verdict(errW, "new "+name, fmt.Sprintf("scaffolded from %s — %s, %s",
			filepath.Base(from), count(len(d.Features), "feature"), count(d.plan.report.Stubs, "stub")))
		return 0
	}
	fmt.Fprint(out, "\nYour first feature:\n  ultra new feature <name>   # internal/app/<name>/<name>.go, then one line in app.Modules()\n")
	verdict(errW, "new "+name, fmt.Sprintf("scaffolded %s (%s)", module, strings.TrimPrefix(capsSuffix(d), ", ")))
	return 0
}

// capsSuffix renders the capability set for the "created" line — the flags
// are the product's shape, so echo them back.
func capsSuffix(d scaffoldData) string {
	var on []string
	for _, c := range []struct {
		name string
		on   bool
	}{{"db", d.DB}, {"web", d.Web}, {"auth", d.Auth}} {
		if c.on {
			on = append(on, c.name)
		}
	}
	if len(on) == 0 {
		return ", bare"
	}
	return ", " + strings.Join(on, "+")
}

// cmdNewFeature implements `ultra new feature <name>`: the single-file
// collapse form of a feature package. Manifest files (handler.go,
// service.go, types.go, ...) are NOT scaffolded — they appear when content
// demands them, which is the doctrine's growth rule.
func cmdNewFeature(args []string, out, errW io.Writer) int {
	if len(args) != 1 {
		ultraTree().find("new").find("feature").help(errW)
		return 2
	}
	name := args[0]
	if !pkgNameRe.MatchString(name) {
		fmt.Fprintf(errW, "feature name %q must be a Go package name: lowercase letters and digits, starting with a letter\n", name)
		failVerdict(errW, "new feature "+name, "not a Go package name")
		return 1
	}
	if _, err := os.Stat("internal/app"); err != nil {
		fmt.Fprintln(errW, "no internal/app directory here — run `ultra new feature` from the product root")
		failVerdict(errW, "new feature "+name, "not a product root")
		return 1
	}
	dir := filepath.Join("internal", "app", name)
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(errW, "%s already exists — refusing to overwrite\n", dir)
		failVerdict(errW, "new feature "+name, dir+" already exists")
		return 1
	}
	if err := renderAll(".", map[string]string{
		"feature.go.tmpl":        filepath.Join(dir, name+".go"),
		"feature_errors.go.tmpl": filepath.Join(dir, "errors.go"),
	}, scaffoldData{Name: name}); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new feature "+name, err.Error())
		return 1
	}
	fmt.Fprintf(out, "created %s/%s.go\n\nOne line left — in internal/app/app.go:\n\t%s.Use(),\n", dir, name, name)
	verdict(errW, "new feature "+name, "created "+dir+" — one line left in app.go")
	return 0
}
