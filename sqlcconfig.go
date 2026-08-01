package main

// The product's sqlc.yaml, read the way the migrations are read.
//
// sqlc hands a plugin its OWN codegen entry and nothing else about types: the
// `gen: go:` block that decides every Go type sqlc emits — `overrides:` and
// `emit_pointers_for_null_types` — never crosses the wire. That is not a
// guess; a real request dumped from sqlc v1.31.1 carries Settings fields
// 1,2,3,4 and 12 and no more.
//
// The block is on disk, though, in the working directory sqlc runs the plugin
// in, and it is the very file sqlc's Go codegen resolved its types from. So
// reading it is the only way the factory's types and the params struct's types
// cannot drift — the same bargain sqlcschema.go strikes for the constraints
// the proto drops.
//
// What follows is a scanner for the block-style subset a sqlc config is
// written in — nested maps, lists, scalars — not a YAML implementation: no
// flow collections, no anchors, no multi-line scalars. A config it cannot read
// costs the gen.go facts and SAYS SO on stderr; it never yields a wrong one.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// readTypeConfig resolves the product's type policy. The config file wins
// outright when it can be read: the options block is a copy a product has to
// keep in sync by hand, and the copy is exactly what goes stale.
func readTypeConfig(dir string, codegen sqlcCodegen, errW io.Writer) typeConfig {
	options := typeOverridesOf(jsonTree(codegen.Options)["overrides"])
	gen := goSettings(sqlcConfigTree(dir), codegen)
	if gen == nil {
		fmt.Fprintf(errW, "ultra plugin: no readable `gen: go:` block in %s — gen/factory falls back to this codegen entry's own options: and assumes nullable columns are pgtype\n", dir)
		return typeConfig{overrides: options}
	}
	cfg := typeConfig{
		overrides: typeOverridesOf(gen["overrides"]),
		pointers:  truthy(gen["emit_pointers_for_null_types"]),
	}
	// The twin `options:` block is no longer load-bearing, but a product that
	// edited one list and not the other is a product whose two halves disagree
	// about a type — and sqlc emits the gen.go one.
	if len(options) > 0 && !slices.Equal(options, cfg.overrides) {
		fmt.Fprintln(errW, "ultra plugin: the codegen options: overrides differ from gen.go's — sqlc emits the gen.go types, so gen/factory follows those")
	}
	return cfg
}

// sqlcConfigTree reads the config sqlc itself was run with: the plugin's
// working directory IS the directory that file sits in.
func sqlcConfigTree(dir string) map[string]any {
	for _, name := range []string{"sqlc.yaml", "sqlc.yml", "sqlc.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if strings.HasSuffix(name, ".json") {
			return jsonTree(b)
		}
		return yamlTree(string(b))
	}
	return nil
}

// goSettings returns the `gen: go:` map of the sql block THIS codegen entry
// belongs to — a config may hold several, and only one of them describes the
// package this plugin is writing into.
func goSettings(tree map[string]any, codegen sqlcCodegen) map[string]any {
	var fallback map[string]any
	for _, block := range seqOf(tree["sql"]) {
		m := mapOf(block)
		gen := mapOf(mapOf(m["gen"])["go"])
		if gen == nil {
			continue
		}
		for _, c := range seqOf(m["codegen"]) {
			if cm := mapOf(c); strOf(cm["plugin"]) == codegen.Plugin && strOf(cm["out"]) == codegen.Out {
				return gen
			}
		}
		if fallback == nil {
			fallback = gen
		}
	}
	return fallback
}

func jsonTree(b []byte) map[string]any {
	var m map[string]any
	if len(b) == 0 || json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// ---- the YAML subset ----

type yamlLine struct {
	indent int
	text   string
}

// yamlTree parses one config document. nil means "outside the subset", which
// the caller reports rather than works around.
func yamlTree(src string) map[string]any {
	lines := yamlLines(src)
	if len(lines) == 0 {
		return nil
	}
	v, _ := yamlNode(lines, 0, lines[0].indent)
	return mapOf(v)
}

// yamlLines drops comments and blank lines, and expands "- item" into a marker
// line plus the item's own line at the column it actually starts in — so a
// sequence entry parses as exactly the map or scalar it is.
func yamlLines(src string) []yamlLine {
	var out []yamlLine
	for _, raw := range strings.Split(src, "\n") {
		text := strings.TrimRight(stripYAMLComment(raw), " \t")
		if strings.TrimSpace(text) == "" {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " \t"))
		if strings.Contains(text[:indent], "\t") {
			return nil // YAML forbids tab indentation, and so does this reader
		}
		text = text[indent:]
		for text == "-" || strings.HasPrefix(text, "- ") {
			out = append(out, yamlLine{indent, "-"})
			indent += 2
			text = strings.TrimPrefix(strings.TrimPrefix(text, "-"), " ")
		}
		if text != "" {
			out = append(out, yamlLine{indent, text})
		}
	}
	return out
}

// yamlNode parses one node at the given indent and returns the line after it.
func yamlNode(lines []yamlLine, i, indent int) (any, int) {
	if lines[i].text == "-" {
		var seq []any
		for i < len(lines) && lines[i].indent == indent && lines[i].text == "-" {
			i++
			if i < len(lines) && lines[i].indent > indent {
				var v any
				v, i = yamlNode(lines, i, lines[i].indent)
				seq = append(seq, v)
				continue
			}
			seq = append(seq, "")
		}
		return seq, i
	}
	m := map[string]any{}
	for i < len(lines) && lines[i].indent == indent && lines[i].text != "-" {
		key, rest, ok := strings.Cut(lines[i].text, ":")
		if !ok {
			if len(m) == 0 {
				return yamlScalar(lines[i].text), i + 1 // a scalar, e.g. a list item
			}
			return m, i
		}
		key, rest = yamlScalar(key), strings.TrimSpace(rest)
		i++
		switch {
		case rest != "":
			m[key] = yamlScalar(rest)
		case i < len(lines) && lines[i].indent > indent:
			m[key], i = yamlNode(lines, i, lines[i].indent)
		case i < len(lines) && lines[i].indent == indent && lines[i].text == "-":
			m[key], i = yamlNode(lines, i, indent) // a sequence written flush with its key
		default:
			m[key] = ""
		}
	}
	return m, i
}

// stripYAMLComment cuts at the first # that is neither quoted nor part of a
// word (a go_type of "pkg#x" is not a comment, and neither is a colour).
func stripYAMLComment(line string) string {
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

func yamlScalar(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ---- reading one decoded tree, whichever parser produced it ----

func mapOf(v any) map[string]any { m, _ := v.(map[string]any); return m }
func seqOf(v any) []any          { s, _ := v.([]any); return s }
func strOf(v any) string         { s, _ := v.(string); return s }

// truthy accepts both spellings a value arrives in: JSON decodes `true` to a
// bool, the YAML subset keeps it a string.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "yes" || t == "on"
	}
	return false
}
