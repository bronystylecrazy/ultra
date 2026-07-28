package main

// `ultra` as a sqlc process plugin.
//
// sqlc execs the plugin binary with the CodeGenRequest protobuf on stdin, the
// CodeGenResponse expected on stdout, the sqlc.yaml directory as the working
// directory, and a DELIBERATELY EMPTY environment except SQLC_VERSION and
// whatever the plugin's `env:` list asks for. That last fact is the detection:
// SQLC_VERSION set plus a stdin that is not a terminal cannot be a human at a
// shell, and the check runs before argument parsing because sqlc passes
// "/plugin.CodegenService/Generate" as argv[1] — a word this CLI must never
// try to route.
//
// One binary, two personalities, no second install: the scaffolded sqlc.yaml
// names `ultra` and the product's `sqlc generate` picks up whatever version
// the developer already has on PATH.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// pluginMode reports whether sqlc exec'd us. stdin is a parameter so the test
// can hand it a real pipe: a terminal and /dev/null are both character
// devices, and only the pipe case is sqlc.
func pluginMode(stdin *os.File) bool {
	if os.Getenv("SQLC_VERSION") == "" {
		return false
	}
	st, err := stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

// runPlugin reads one CodeGenRequest and writes one CodeGenResponse. Failures
// go to stderr and a non-zero exit — sqlc surfaces both, and a plugin that
// answered with half a package would be worse than one that refused.
func runPlugin(in io.Reader, out, errW io.Writer) int {
	body, err := io.ReadAll(in)
	if err != nil {
		fmt.Fprintln(errW, "ultra plugin: reading the request:", err)
		return 1
	}
	req, err := decodeRequest(body)
	if err != nil {
		fmt.Fprintln(errW, "ultra plugin:", err)
		return 1
	}
	if req.Settings.Engine != "" && req.Settings.Engine != "postgresql" {
		fmt.Fprintf(errW, "ultra plugin: engine %q is not supported — this plugin generates pgx/v5 code\n",
			req.Settings.Engine)
		return 1
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errW, "ultra plugin:", err)
		return 1
	}
	genImport, err := genImportPath(dir, req.Settings.Codegen.Out)
	if err != nil {
		fmt.Fprintln(errW, "ultra plugin:", err)
		return 1
	}
	tables := analyze(req, readSchemaFacts(dir, req.Settings.Schema),
		typeTable(req.Settings.Codegen.Options, errW))
	if len(tables) == 0 {
		fmt.Fprintln(errW, "ultra plugin: the catalog has no tables yet — nothing to generate")
		out.Write(encodeResponse(nil))
		return 0
	}
	files, err := emitAll(tables, genImport)
	if err != nil {
		fmt.Fprintln(errW, "ultra plugin:", err)
		return 1
	}
	if _, err := out.Write(encodeResponse(files)); err != nil {
		fmt.Fprintln(errW, "ultra plugin: writing the response:", err)
		return 1
	}
	return 0
}

// genImportPath is the import path of the sqlc output package — the factory
// package is a sibling directory, so it must import gen by path. Derived from
// the nearest go.mod above the sqlc.yaml, which is the product's root.
func genImportPath(dir, out string) (string, error) {
	if out == "" {
		out = "gen"
	}
	target := filepath.Join(dir, out)
	root := dir
	for {
		if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
			mod := modulePathOf(string(b))
			if mod == "" {
				return "", fmt.Errorf("%s declares no module path", filepath.Join(root, "go.mod"))
			}
			rel, err := filepath.Rel(root, target)
			if err != nil {
				return "", err
			}
			return mod + "/" + filepath.ToSlash(rel), nil
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("no go.mod above %s — run sqlc from inside the product", dir)
		}
		root = parent
	}
}

func modulePathOf(gomod string) string {
	for _, line := range strings.Split(gomod, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}
