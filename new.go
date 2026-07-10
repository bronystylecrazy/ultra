package main

import (
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/*
var templates embed.FS

// scaffoldVersion pins the framework version generated products require.
const scaffoldVersion = "v0.2.0"

// scaffoldGoVersion is the Go directive for generated products.
const scaffoldGoVersion = "1.26"

type scaffoldData struct {
	Name      string
	Module    string
	Version   string
	GoVersion string
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// scaffold renders the product skeleton into dir. Pure generation — no
// network, no exec — so it is fully testable.
func scaffold(dir, name, module string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("product name %q must be lowercase letters, digits, and dashes, starting with a letter", name)
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", dir)
	}
	data := scaffoldData{
		Name: name, Module: module,
		Version: scaffoldVersion, GoVersion: scaffoldGoVersion,
	}
	files := map[string]string{ // template → output
		"main.go.tmpl":      "main.go",
		"modules.go.tmpl":   "modules.go",
		"config.toml.tmpl":  "config.toml",
		"main_test.go.tmpl": "main_test.go",
		"go.mod.tmpl":       "go.mod",
		"SKILL.md.tmpl":     "SKILL.md",
		"gitignore.tmpl":    ".gitignore",
		// The testing tier: smoke (does it turn on), one sanity file per wired
		// capability area (auth, api), and a binary-mode e2e story. Plus the CI
		// ladder. See references/presets/testkit.md.
		"smoke_test.go.tmpl":       "smoke_test.go",
		"sanity_auth_test.go.tmpl": "sanity_auth_test.go",
		"sanity_api_test.go.tmpl":  "sanity_api_test.go",
		"e2e/doc.go.tmpl":          "e2e/doc.go",
		"e2e/story_test.go.tmpl":   "e2e/story_test.go",
		"github-ci.yml.tmpl":       ".github/workflows/ci.yml",
		// The embedded-frontend seam: an empty dist committed so
		// `//go:embed all:dist` always compiles, and stack.SPA (wired,
		// commented, in modules.go) serves a placeholder until a build lands.
		"internal/ui/ui.go.tmpl":     "internal/ui/ui.go",
		"internal/ui/dist/keep.tmpl": "internal/ui/dist/.keep",
		// The routing core (SKILL.md) plus one reference per capability this
		// scaffold actually wires — conf, http, auth, otel, api, cli — and the
		// kernel/errors/graph pointers. They route to the framework's generated
		// references; growing the product means adding the preset's reference.
		"references/kernel.md.tmpl":       "references/kernel.md",
		"references/errors.md.tmpl":       "references/errors.md",
		"references/graph.md.tmpl":        "references/graph.md",
		"references/presets/conf.md.tmpl": "references/presets/conf.md",
		"references/presets/http.md.tmpl": "references/presets/http.md",
		"references/presets/auth.md.tmpl": "references/presets/auth.md",
		"references/presets/otel.md.tmpl": "references/presets/otel.md",
		"references/presets/api.md.tmpl":     "references/presets/api.md",
		"references/presets/cli.md.tmpl":     "references/presets/cli.md",
		"references/presets/testkit.md.tmpl": "references/presets/testkit.md",
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for tmpl, out := range files {
		t, err := template.ParseFS(templates, "templates/"+tmpl)
		if err != nil {
			return fmt.Errorf("template %s: %w", tmpl, err)
		}
		target := filepath.Join(dir, out)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		if err := t.Execute(f, data); err != nil {
			f.Close()
			return fmt.Errorf("render %s: %w", out, err)
		}
		f.Close()
	}
	return nil
}

// cmdNew implements `ultra new <name> [--module path]`.
func cmdNew(args []string, out, errW io.Writer) int {
	var name, module string
	rest := args
	for len(rest) > 0 {
		switch {
		case rest[0] == "--module" && len(rest) > 1:
			module = rest[1]
			rest = rest[2:]
		case strings.HasPrefix(rest[0], "-"):
			fmt.Fprintf(errW, "unknown flag %q\nusage: ultra new <name> [--module github.com/org/name]\n", rest[0])
			return 2
		default:
			if name != "" {
				fmt.Fprintln(errW, "usage: ultra new <name> [--module github.com/org/name]")
				return 2
			}
			name = rest[0]
			rest = rest[1:]
		}
	}
	if name == "" {
		fmt.Fprintln(errW, "usage: ultra new <name> [--module github.com/org/name]")
		return 2
	}
	if module == "" {
		module = "example.com/" + name
	}

	if err := scaffold(name, name, module); err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	fmt.Fprintf(out, "created %s/ (module %s)\n", name, module)

	// Resolve dependencies. The platform repo is private: route module
	// resolution straight to git (which carries your GitHub credentials).
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = name
	tidy.Env = append(os.Environ(), "GOPRIVATE=github.com/bronystylecrazy/*")
	tidy.Stdout = out
	tidy.Stderr = errW
	if err := tidy.Run(); err != nil {
		fmt.Fprintf(errW, `
go mod tidy did not finish (%v). To resolve manually:
  cd %s
  GOPRIVATE=github.com/bronystylecrazy/* go mod tidy
(private repos need git auth: gh auth setup-git, or SSH)
`, err, name)
		return 1
	}

	fmt.Fprintf(out, `
%s is ready:
  cd %s
  go test ./...          # the covenant: wiring + boot
  go run .               # serve on :8080
  go run . help          # the toolbox
`, name, name)
	return 0
}
