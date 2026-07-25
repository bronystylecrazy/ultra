package main

import (
	"bytes"
	"embed"
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
const scaffoldVersion = "v0.9.16"

// scaffoldGoVersion is the go directive for generated products. It must be
// at least the framework's own, or the toolchain refuses the dependency.
const scaffoldGoVersion = "1.26.3"

// modulePath is the framework's root module; the contrib module hangs off it.
const modulePath = "github.com/bronystylecrazy/ultrastack"

// privateGlob routes module resolution for the platform's private repos
// straight to git (which carries your GitHub credentials), root module
// included — GOPRIVATE globs do not descend into the module they name.
const privateGlob = "github.com/bronystylecrazy/*"

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
		// The product itself: the ONLY file that knows the feature list.
		"app.go.tmpl": "internal/app/app.go",
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
		files["web/src/routes/page.svelte.tmpl"] = "web/src/routes/+page.svelte"
		files["web/src/lib/api/gitkeep.tmpl"] = "web/src/lib/api/.gitkeep"
		// A seed of the generated dev proxy so `task dev:web` forwards
		// correctly before the first `task gen`; that command rewrites it.
		files["web/src/lib/api/vite.proxy.json.tmpl"] = "web/src/lib/api/vite.proxy.json"
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return renderAll(dir, scaffoldFiles(d), d)
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

const newUsage = `usage: ultra new <name> [--module github.com/org/name] [--version vX.Y.Z]
                      [--bare] [--db|--no-db] [--web|--no-web] [--auth|--no-auth]
       ultra new feature <name>

  --db --web --auth are ON by default; --bare turns all three off, and a
  later --db/--web/--auth turns one back on.
`

// cmdNew implements `ultra new <name> [flags]` and `ultra new feature <name>`.
func cmdNew(args []string, out, errW io.Writer) int {
	if len(args) > 0 && args[0] == "feature" {
		return cmdNewFeature(args[1:], out, errW)
	}

	var name, module, version string
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
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(errW, "unknown flag %q\n%s", a, newUsage)
			return 2
		default:
			if name != "" {
				fmt.Fprint(errW, newUsage)
				return 2
			}
			name, rest = a, rest[1:]
		}
	}
	if name == "" {
		fmt.Fprint(errW, newUsage)
		return 2
	}
	if module == "" {
		module = "example.com/" + name
	}
	d.Name, d.Module = name, module
	d.Version, d.GoVersion = resolveVersion(version), scaffoldGoVersion

	if err := scaffold(name, d); err != nil {
		fmt.Fprintln(errW, err)
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
		return 1
	}

	fmt.Fprintf(out, "\n%s is ready:\n  cd %s\n  task test        # the covenant: wiring + boot\n  task dev:api     # serve on :8080 (a dev build serves NO frontend — by design)\n",
		name, name)
	if d.Web {
		fmt.Fprint(out, "  task dev:web     # the SvelteKit dev server (bun install first)\n")
	}
	fmt.Fprint(out, "\nYour first feature:\n  ultra new feature <name>   # internal/app/<name>/<name>.go, then one line in app.Modules()\n")
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
		fmt.Fprintln(errW, "usage: ultra new feature <name>   (run from the product root)")
		return 2
	}
	name := args[0]
	if !pkgNameRe.MatchString(name) {
		fmt.Fprintf(errW, "feature name %q must be a Go package name: lowercase letters and digits, starting with a letter\n", name)
		return 1
	}
	if _, err := os.Stat("internal/app"); err != nil {
		fmt.Fprintln(errW, "no internal/app directory here — run `ultra new feature` from the product root")
		return 1
	}
	dir := filepath.Join("internal", "app", name)
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(errW, "%s already exists — refusing to overwrite\n", dir)
		return 1
	}
	if err := renderAll(".", map[string]string{
		"feature.go.tmpl": filepath.Join(dir, name+".go"),
	}, scaffoldData{Name: name}); err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	fmt.Fprintf(out, "created %s/%s.go\n\nOne line left — in internal/app/app.go:\n\t%s.Use(),\n", dir, name, name)
	return 0
}
