package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeConfig puts one sqlc config in a fresh directory — the working
// directory sqlc hands the plugin.
func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const productConfig = `version: "2"
plugins:
  - name: ultra
    process:
      cmd: ultra
sql:
  - engine: "postgresql"
    schema: "migrations"   # scanned again for the constraints
    queries: "queries"
    codegen:
      - plugin: ultra
        out: "gen"
    gen:
      go:
        package: "gen"
        out: "gen"
        sql_package: "pgx/v5"
        emit_pointers_for_null_types: true
        overrides:
          - db_type: "uuid"
            go_type: "github.com/google/uuid.UUID"
          - column: "ducks.weight"
            go_type:
              import: "github.com/shopspring/decimal"
              type: "Decimal"
`

// TestReadTypeConfig is the channel that replaced the twin blocks: the plugin
// reads the SAME sqlc.yaml sqlc's Go codegen resolved its types from, so an
// override or a nullable rendering cannot be true on one side and false on the
// other.
func TestReadTypeConfig(t *testing.T) {
	dir := writeConfig(t, "sqlc.yaml", productConfig)
	cfg := readTypeConfig(dir, sqlcCodegen{Out: "gen", Plugin: "ultra"}, io.Discard)
	cfg.schema = "public"
	if !cfg.pointers {
		t.Error("emit_pointers_for_null_types was not read, so every nullable column resolves to the wrong half")
	}
	ducks := &sqlcIdent{Name: "ducks"}
	for _, c := range []struct {
		column sqlcColumn
		want   string
	}{
		{col("id", "uuid", true), "uuid.UUID"},          // the db_type override
		{col("owner_id", "uuid", false), "pgtype.UUID"}, // which does not reach a nullable column
		{col("weight", "numeric", true), "decimal.Decimal"},
		{col("name", "text", false), "*string"}, // the pointer rendering
		{col("name", "text", true), "string"},
	} {
		if got, _ := cfg.goTypeOf(ducks, c.column); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.column.Name, c.column.Type.Name, got, c.want)
		}
	}
}

// TestReadTypeConfigJSON covers the other spelling sqlc accepts for the same
// file — a product on sqlc.json must not silently lose its overrides.
func TestReadTypeConfigJSON(t *testing.T) {
	dir := writeConfig(t, "sqlc.json", `{"version":"2","sql":[{"engine":"postgresql",
		"codegen":[{"plugin":"ultra","out":"gen"}],
		"gen":{"go":{"emit_pointers_for_null_types":true,
			"overrides":[{"db_type":"timestamptz","go_type":"time.Time"}]}}}]}`)
	cfg := readTypeConfig(dir, sqlcCodegen{Out: "gen", Plugin: "ultra"}, io.Discard)
	if !cfg.pointers {
		t.Error("emit_pointers_for_null_types was not read out of sqlc.json")
	}
	if got, _ := cfg.goTypeOf(&sqlcIdent{Name: "ducks"}, col("created_at", "timestamptz", true)); got != "time.Time" {
		t.Errorf("created_at = %q, want time.Time", got)
	}
}

// TestReadTypeConfigPicksItsOwnBlock: a config may carry several sql blocks,
// and only the one whose codegen entry IS this plugin describes the package it
// is writing into.
func TestReadTypeConfigPicksItsOwnBlock(t *testing.T) {
	dir := writeConfig(t, "sqlc.yaml", `version: "2"
sql:
- engine: "postgresql"
  codegen:
  - plugin: other
    out: "elsewhere"
  gen:
    go:
      emit_pointers_for_null_types: true
- engine: "postgresql"
  codegen:
  - plugin: ultra
    out: "gen"
  gen:
    go:
      overrides:
      - db_type: "uuid"
        go_type: "github.com/google/uuid.UUID"
`)
	cfg := readTypeConfig(dir, sqlcCodegen{Out: "gen", Plugin: "ultra"}, io.Discard)
	if cfg.pointers {
		t.Error("the other block's emit_pointers_for_null_types was read for this one")
	}
	if got, _ := cfg.goTypeOf(&sqlcIdent{Name: "ducks"}, col("id", "uuid", true)); got != "uuid.UUID" {
		t.Errorf("id = %q, want uuid.UUID — the flush-list layout was not read", got)
	}
}

// TestScaffoldedConfigIsReadable closes the loop on the two halves this plugin
// ships: the sqlc.yaml `ultra new` writes must be one THIS reader can read, or
// every scaffolded product falls back to guessing.
func TestScaffoldedConfigIsReadable(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("templates", "db", "sqlc.yaml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	gen := goSettings(yamlTree(string(body)), sqlcCodegen{Out: "gen", Plugin: "ultra"})
	if gen == nil {
		t.Fatalf("the scaffolded sqlc.yaml has no gen: go: block this plugin can find:\n%s", body)
	}
	if !truthy(gen["emit_pointers_for_null_types"]) {
		t.Error("the scaffold emits pointers for null types; a factory that does not know that fills pgtype and stops compiling")
	}
	if strOf(gen["sql_package"]) != "pgx/v5" {
		t.Errorf("sql_package = %q — every rendering in pgTypes is pgx/v5's", gen["sql_package"])
	}
}

// TestYAMLSubset covers the shapes a hand-edited config arrives in, and the
// ones this reader refuses rather than misreads.
func TestYAMLSubset(t *testing.T) {
	tree := yamlTree(`# a leading comment
version: "2"
sql:
  - engine: postgresql   # trailing comment
    codegen:
      - plugin: ultra
        out: gen
    gen:
      go:
        emit_pointers_for_null_types: true
        overrides:
          - db_type: uuid
            go_type: github.com/google/uuid.UUID
        rename:
          id: ID
`)
	block := mapOf(seqOf(tree["sql"])[0])
	if strOf(block["engine"]) != "postgresql" {
		t.Errorf("engine = %v", block["engine"])
	}
	gen := mapOf(mapOf(block["gen"])["go"])
	over := mapOf(seqOf(gen["overrides"])[0])
	if strOf(over["go_type"]) != "github.com/google/uuid.UUID" {
		t.Errorf("go_type = %v", over["go_type"])
	}
	if strOf(mapOf(gen["rename"])["id"]) != "ID" {
		t.Errorf("a map after a list was misparsed: %v", gen["rename"])
	}
	if got := strOf(mapOf(seqOf(block["codegen"])[0])["plugin"]); got != "ultra" {
		t.Errorf("codegen plugin = %q", got)
	}
	// A tab indent is not YAML, and a reader that guessed at one would be
	// guessing at the types too.
	if yamlTree("sql:\n\t- engine: postgresql\n") != nil {
		t.Error("tab indentation must be refused, not interpreted")
	}
}
