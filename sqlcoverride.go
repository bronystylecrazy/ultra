package main

// The Go type sqlc actually emitted, column by column.
//
// A factory assigns into the params struct sqlc generated beside it, so every
// value it fills must have the type SQLC chose — never the type this plugin
// would have picked from the db type alone. Three things move that type, and
// two of them used to be invisible here:
//
//   - an `overrides:` entry keyed by db_type (the one migrate.md documents),
//   - an `overrides:` entry keyed by COLUMN, which wins outright and ignores
//     nullability — a real product shipped one and got the driver default,
//   - NULLABILITY itself: a nullable text is *string or pgtype.Text, never
//     string, and a factory that filled string did not compile.
//
// What follows is a port of sqlc's own goType/goInnerType at v1.31.1 — same
// precedence, same order, same nullability rule — because the params struct
// and this file must reach the same answer or the generated package does not
// build. The renderings themselves are captured from real sqlc output, both
// with and without emit_pointers_for_null_types, and checked against it column
// by column in TestResolvedTypesMatchSQLC.

import (
	"regexp"
	"strings"
)

// typeConfig is the type policy this product's sqlc.yaml resolved: the
// overrides in sqlc's precedence order, whether nullable columns are pointers,
// whether :one/:many results are pointers (emit_result_struct_pointers), and
// the catalog's default schema (a column override names its table).
type typeConfig struct {
	overrides      []typeOverride
	pointers       bool
	resultPointers bool
	schema         string
}

// typeOverride is one `overrides:` entry, from whichever channel carried it.
type typeOverride struct {
	dbType   string
	column   string // table.column, schema.table.column, catalog.schema.table.column
	nullable bool
	goType   string // spelled the way the generated file must write it
	imp      string // the import that spells it, when the entry named one
}

// typeOverridesOf reads an `overrides:` list out of a decoded config. The YAML
// tree and the options JSON arrive as the same shape, so one reader serves
// both channels.
func typeOverridesOf(v any) []typeOverride {
	var out []typeOverride
	for _, item := range seqOf(v) {
		m := mapOf(item)
		o := typeOverride{
			dbType:   strOf(m["db_type"]),
			column:   strOf(m["column"]),
			nullable: truthy(m["nullable"]),
		}
		o.goType, o.imp = renderGoType(m["go_type"])
		if o.goType == "" || (o.dbType == "" && o.column == "") {
			continue // not an override this plugin can act on
		}
		out = append(out, o)
	}
	return out
}

// goTypeOf is the type sqlc put in this column's struct field: a column
// override outright, then a db_type override whose nullability matches, then
// the driver's own rendering. An empty string means sqlc's answer is not one
// this plugin can reproduce — the caller emits nothing rather than guess.
func (c typeConfig) goTypeOf(table *sqlcIdent, col sqlcColumn) (goType, imp string) {
	notNull := col.NotNull || col.IsArray
	for _, o := range c.overrides {
		if o.column != "" && o.matchesColumn(c.schema, table, col.Name) {
			return o.goType, o.imp
		}
	}
	db := baseType(col.Type.Name)
	for _, o := range c.overrides {
		if o.dbType != "" && baseType(o.dbType) == db && o.nullable != notNull {
			return o.goType, o.imp
		}
	}
	t, ok := pgTypes[db]
	switch {
	case !ok:
		return "", ""
	case notNull:
		return t.notNull, ""
	case c.pointers && t.pointer:
		return "*" + t.notNull, ""
	default:
		return t.null, ""
	}
}

// matchesColumn is sqlc's column matching in the subset a product writes: two
// to four dotted parts, with * as a wildcard for any of them.
func (o typeOverride) matchesColumn(defaultSchema string, table *sqlcIdent, col string) bool {
	parts := strings.Split(o.column, ".")
	if table == nil || len(parts) < 2 || len(parts) > 4 {
		return false
	}
	want := sqlcIdent{Schema: defaultSchema, Name: parts[len(parts)-2]}
	switch len(parts) {
	case 3:
		want.Schema = parts[0]
	case 4:
		want.Catalog, want.Schema = parts[0], parts[1]
	}
	schema := table.Schema
	if schema == "" {
		schema = defaultSchema
	}
	if want.Catalog != "" && !globMatch(want.Catalog, table.Catalog) {
		return false
	}
	return globMatch(want.Schema, schema) &&
		globMatch(want.Name, table.Name) &&
		globMatch(parts[len(parts)-1], col)
}

func globMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	re, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$")
	return err == nil && re.MatchString(s)
}

// renderGoType spells an override's `go_type` the way the generated file must
// write it, and names the import that supplies it. sqlc accepts both forms: a
// fully qualified string ("github.com/google/uuid.UUID", "time.Time") and an
// object naming the import and the type separately.
func renderGoType(v any) (goType, imp string) {
	if s := strOf(v); s != "" {
		dir, base := "", s
		if i := strings.LastIndex(s, "/"); i >= 0 {
			dir, base = s[:i+1], s[i+1:]
		}
		pkg, _, ok := strings.Cut(base, ".")
		if !ok {
			return base, "" // a bare type name names no package
		}
		return base, dir + pkg
	}
	m := mapOf(v)
	typ := strOf(m["type"])
	if typ == "" {
		return "", ""
	}
	imp = strOf(m["import"])
	pkg := strOf(m["package"])
	if pkg == "" && imp != "" {
		pkg = imp[strings.LastIndex(imp, "/")+1:]
	}
	if pkg != "" {
		typ = pkg + "." + typ
	}
	if truthy(m["slice"]) {
		typ = "[]" + typ
	}
	if truthy(m["pointer"]) {
		typ = "*" + typ
	}
	return typ, imp
}
