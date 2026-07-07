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
const scaffoldVersion = "v0.1.0"

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
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for tmpl, out := range files {
		t, err := template.ParseFS(templates, "templates/"+tmpl)
		if err != nil {
			return fmt.Errorf("template %s: %w", tmpl, err)
		}
		f, err := os.Create(filepath.Join(dir, out))
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
