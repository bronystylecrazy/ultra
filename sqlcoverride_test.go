package main

import (
	"strings"
	"testing"
)

// optionsConfig is the type policy a product delivers through the codegen
// entry's `options:` — the channel a process plugin is handed directly.
func optionsConfig(t *testing.T, opts string) typeConfig {
	t.Helper()
	return typeConfig{overrides: typeOverridesOf(jsonTree([]byte(opts))["overrides"]), schema: "public"}
}

func col(name, dbType string, notNull bool) sqlcColumn {
	return sqlcColumn{Name: name, NotNull: notNull, Type: sqlcIdent{Name: dbType}}
}

// TestResolvedTypes covers the resolution itself: the override spellings a
// product might use instead of the two migrate.md shows, and the answer for a
// column sqlc renders differently than the db type alone would suggest.
func TestResolvedTypes(t *testing.T) {
	notes := &sqlcIdent{Schema: "public", Name: "notes"}
	for _, c := range []struct {
		name, opts string
		column     sqlcColumn
		want       string
	}{
		{"no options at all", "", col("id", "uuid", true), "pgtype.UUID"},
		{"the documented string form",
			`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID"}]}`,
			col("id", "uuid", true), "uuid.UUID"},
		{"the object form sqlc also accepts",
			`{"overrides":[{"db_type":"uuid","go_type":{"import":"github.com/google/uuid","type":"UUID"}}]}`,
			col("id", "uuid", true), "uuid.UUID"},
		{"an explicit package name wins over the import path",
			`{"overrides":[{"db_type":"uuid","go_type":{"import":"example.com/x","package":"uuid","type":"UUID"}}]}`,
			col("id", "uuid", true), "uuid.UUID"},
		// sqlc matches a db_type override against ONE nullability, so the
		// documented NOT NULL placement leaves nullable columns alone.
		{"a not-null override does not reach a nullable column",
			`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID"}]}`,
			col("owner_id", "uuid", false), "pgtype.UUID"},
		{"a nullable override reaches only the nullable column",
			`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID","nullable":true}]}`,
			col("owner_id", "uuid", false), "uuid.UUID"},
		{"and leaves the not-null one",
			`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID","nullable":true}]}`,
			col("id", "uuid", true), "pgtype.UUID"},
		// A column override wins outright, whatever the column's nullability —
		// this is the one duckpond shipped and the factory ignored.
		{"a column override beats the db type",
			`{"overrides":[{"column":"notes.body","go_type":"github.com/google/uuid.UUID"}]}`,
			col("body", "text", true), "uuid.UUID"},
		{"a column override ignores nullability",
			`{"overrides":[{"column":"notes.body","go_type":"github.com/google/uuid.UUID"}]}`,
			col("body", "text", false), "uuid.UUID"},
		{"a column override on another table does not reach this one",
			`{"overrides":[{"column":"ducks.body","go_type":"github.com/google/uuid.UUID"}]}`,
			col("body", "text", true), "string"},
		{"the schema-qualified form",
			`{"overrides":[{"column":"public.notes.body","go_type":"time.Time"}]}`,
			col("body", "text", true), "time.Time"},
		{"a wildcard table",
			`{"overrides":[{"column":"*.body","go_type":"time.Time"}]}`,
			col("body", "text", true), "time.Time"},
		{"a type this plugin has no rendering for", "", col("area", "box", true), ""},
		{"unreadable options change nothing", `{`, col("id", "uuid", true), "pgtype.UUID"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := optionsConfig(t, c.opts).goTypeOf(notes, c.column)
			if got != c.want {
				t.Errorf("%s %s = %q, want %q", c.column.Name, c.column.Type.Name, got, c.want)
			}
		})
	}
}

// TestResolvedNullability pins the second half of sqlc's answer, which this
// generator was blind to: the rendering of a NULLABLE column, which is a
// pointer or a pgtype depending on one setting in the product's sqlc.yaml.
// The expectations are transcribed from real sqlc v1.31.1 output.
func TestResolvedNullability(t *testing.T) {
	for _, c := range []struct {
		dbType, notNull, pgtype, pointer string
	}{
		{"text", "string", "pgtype.Text", "*string"},
		{"varchar", "string", "pgtype.Text", "*string"},
		{"bool", "bool", "pgtype.Bool", "*bool"},
		{"int2", "int16", "pgtype.Int2", "*int16"},
		{"int4", "int32", "pgtype.Int4", "*int32"},
		{"int8", "int64", "pgtype.Int8", "*int64"},
		{"float4", "float32", "pgtype.Float4", "*float32"},
		{"float8", "float64", "pgtype.Float8", "*float64"},
		// pgx/v5 keeps these on their pgtype spelling either way — a pointer
		// here would be exactly as wrong as the string was.
		{"uuid", "pgtype.UUID", "pgtype.UUID", "pgtype.UUID"},
		{"timestamptz", "pgtype.Timestamptz", "pgtype.Timestamptz", "pgtype.Timestamptz"},
		{"timestamp", "pgtype.Timestamp", "pgtype.Timestamp", "pgtype.Timestamp"},
		{"date", "pgtype.Date", "pgtype.Date", "pgtype.Date"},
		{"jsonb", "[]byte", "[]byte", "[]byte"},
		{"bytea", "[]byte", "[]byte", "[]byte"},
	} {
		t.Run(c.dbType, func(t *testing.T) {
			plain := typeConfig{schema: "public"}
			pointers := typeConfig{schema: "public", pointers: true}
			ident := &sqlcIdent{Name: "wide"}
			if got, _ := plain.goTypeOf(ident, col("c", c.dbType, true)); got != c.notNull {
				t.Errorf("not null = %q, want %q", got, c.notNull)
			}
			if got, _ := plain.goTypeOf(ident, col("c", c.dbType, false)); got != c.pgtype {
				t.Errorf("nullable = %q, want %q", got, c.pgtype)
			}
			if got, _ := pointers.goTypeOf(ident, col("c", c.dbType, false)); got != c.pointer {
				t.Errorf("nullable with emit_pointers_for_null_types = %q, want %q", got, c.pointer)
			}
			// Every rendering this table can produce must have a factory value,
			// or a product hits "no factory" for an ordinary column.
			for _, goType := range []string{c.notNull, c.pgtype, c.pointer} {
				if _, _, ok := factoryValue(goType, "", c.dbType, "wide", "c", "n"); !ok {
					t.Errorf("%s has no factory value", goType)
				}
			}
		})
	}
}

// TestFactoryValueRefuses is the other half of the bargain: a type this
// generator cannot produce a deterministic value for gets NO value, so the
// table loses its factory with a named reason instead of emitting a fill that
// does not compile.
func TestFactoryValueRefuses(t *testing.T) {
	for _, goType := range []string{"ksuid.KSUID", "*ksuid.KSUID", "pgtype.Numeric", "interface{}"} {
		if expr, _, ok := factoryValue(goType, "", "uuid", "ducks", "id", "n"); ok {
			t.Errorf("%s must have no factory value, got %s", goType, expr)
		}
	}
	// An override that names its own import must be spelled with THAT import,
	// not the google one this generator would otherwise reach for.
	cfg := optionsConfig(t, `{"overrides":[{"db_type":"uuid","go_type":"example.com/x/uuid.UUID"}]}`)
	goType, imp := cfg.goTypeOf(&sqlcIdent{Name: "ducks"}, col("id", "uuid", true))
	_, needs, ok := factoryValue(goType, imp, "uuid", "ducks", "id", "n")
	if !ok || len(needs) != 1 || needs[0] != "example.com/x/uuid" {
		t.Errorf("needs = %v, want [example.com/x/uuid]", needs)
	}
}

// TestRenderGoType covers the two spellings sqlc accepts, and the import each
// one implies — an import guessed wrong is a generated file that does not
// compile just as surely as a type guessed wrong.
func TestRenderGoType(t *testing.T) {
	for _, c := range []struct {
		name             string
		in               any
		wantType, wantIm string
	}{
		{"a qualified string", "github.com/google/uuid.UUID", "uuid.UUID", "github.com/google/uuid"},
		{"a stdlib string", "time.Time", "time.Time", "time"},
		{"a bare name", "int64", "int64", ""},
		{"the object form", map[string]any{"import": "github.com/shopspring/decimal", "type": "Decimal"},
			"decimal.Decimal", "github.com/shopspring/decimal"},
		{"a pointer target", map[string]any{"import": "github.com/google/uuid", "type": "UUID", "pointer": true},
			"*uuid.UUID", "github.com/google/uuid"},
		{"a slice target", map[string]any{"package": "x", "type": "T", "slice": true}, "[]x.T", ""},
		{"nothing at all", map[string]any{"import": "github.com/google/uuid"}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			goType, imp := renderGoType(c.in)
			if goType != c.wantType || imp != c.wantIm {
				t.Errorf("= %q/%q, want %q/%q", goType, imp, c.wantType, c.wantIm)
			}
		})
	}
}

// TestOptionsFallbackSaysSo keeps the options channel working where the config
// file cannot be read — and keeps that fallback from being silent, because a
// product whose nullable columns quietly resolve to the wrong half is the bug
// this whole file exists to close.
func TestOptionsFallbackSaysSo(t *testing.T) {
	var errW strings.Builder
	cfg := readTypeConfig(t.TempDir(), sqlcCodegen{Out: "gen", Plugin: "ultra",
		Options: []byte(`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID"},
			{"column":"ducks.created_at","go_type":"time.Time"}]}`)}, &errW)
	cfg.schema = "public"
	ducks := &sqlcIdent{Name: "ducks"}
	if got, _ := cfg.goTypeOf(ducks, col("id", "uuid", true)); got != "uuid.UUID" {
		t.Errorf("the options override was dropped: %q", got)
	}
	if got, _ := cfg.goTypeOf(ducks, col("created_at", "timestamptz", true)); got != "time.Time" {
		t.Errorf("a COLUMN override through the options channel was dropped: %q", got)
	}
	if !strings.Contains(errW.String(), "gen: go:") {
		t.Errorf("the fallback must name what it could not read: %q", errW.String())
	}
}
