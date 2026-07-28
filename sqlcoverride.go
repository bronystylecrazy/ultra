package main

// Type overrides, as a process plugin can actually see them.
//
// migrate.md documents the two overrides a product reaches for first — uuid →
// uuid.UUID, timestamptz → time.Time. sqlc honours them in gen/<table>.sql.go,
// and this plugin must honour the SAME ones in gen/factory: the moment a
// product follows the doc, a factory that still fills pgtype.Timestamptz stops
// compiling against the params struct sqlc just emitted beside it.
//
// Where they come from is forced by the wire. A CodeGenRequest carries
// Settings.Codegen.Options — the ultra codegen entry's own `options:` block,
// handed over as JSON — and nothing else about types. `sql.gen.go.overrides`
// is resolved inside sqlc's Go codegen and never sent. So the product states
// them twice, and the scaffolded sqlc.yaml carries both blocks side by side
// with a comment saying why.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// codegenOptions is the subset of `options:` this plugin reads. The key and
// value spellings are sqlc's own, so one block copies between the two halves
// of sqlc.yaml unchanged.
type codegenOptions struct {
	Overrides []struct {
		DBType   string          `json:"db_type"`
		Column   string          `json:"column"`
		Nullable bool            `json:"nullable"`
		GoType   json.RawMessage `json:"go_type"`
	} `json:"overrides"`
}

// overrideTargets is what a factory can fill for an overridden column, keyed
// by the rendered Go type. A target absent from this table is not guessed:
// its db type leaves the type table, and every affected table loses its
// factory with a named reason — the same bargain pgTypes strikes.
var overrideTargets = map[string]pgType{
	// at() is factory-local, so a time.Time fill needs no import of its own.
	"time.Time": {"time.Time", func(_, _, seq string) string { return "at(" + seq + ")" }, nil},
	// uuid.UUID is [16]byte, so the deterministic value already on hand
	// converts without a second generator.
	"uuid.UUID": {"uuid.UUID", func(table, _, seq string) string {
		return fmt.Sprintf("uuid.UUID(uuidAt(%q, %s).Bytes)", table, seq)
	}, []string{"github.com/google/uuid"}},
}

// typeTable is the pg → Go mapping THIS product resolved: pgTypes with the
// codegen entry's overrides folded over it. Overrides marked `nullable: true`
// are ignored, because a factory only ever fills the NOT NULL rendering —
// which is also the only placement migrate.md sanctions.
func typeTable(opts []byte, errW io.Writer) map[string]pgType {
	table := make(map[string]pgType, len(pgTypes))
	for k, v := range pgTypes {
		table[k] = v
	}
	if len(opts) == 0 {
		return table
	}
	var o codegenOptions
	if err := json.Unmarshal(opts, &o); err != nil {
		fmt.Fprintf(errW, "ultra plugin: the codegen options are not readable JSON (%v) — factories fall back to the driver defaults\n", err)
		return table
	}
	for _, ov := range o.Overrides {
		if ov.Nullable {
			continue
		}
		if ov.DBType == "" {
			fmt.Fprintf(errW, "ultra plugin: override for column %q is not db_type — gen/factory is override-aware by db type only, so that column keeps the driver default\n", ov.Column)
			continue
		}
		goType := renderGoType(ov.GoType)
		target, ok := overrideTargets[goType]
		if !ok {
			// Removing it is the honest answer: sqlc's params struct now
			// wants a type this generator cannot produce a value for.
			delete(table, baseType(ov.DBType))
			fmt.Fprintf(errW, "ultra plugin: %s is overridden to %s, which has no deterministic factory value — tables using it get no factory\n", ov.DBType, goType)
			continue
		}
		table[baseType(ov.DBType)] = target
	}
	return table
}

// renderGoType spells an override's `go_type` the way the generated file must
// write it. sqlc accepts both forms: a fully qualified string
// ("github.com/google/uuid.UUID", "time.Time") and an object naming the import
// and the type separately.
func renderGoType(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if i := strings.LastIndex(s, "/"); i >= 0 {
			s = s[i+1:] // github.com/google/uuid.UUID -> uuid.UUID
		}
		return s
	}
	var obj struct {
		Import  string `json:"import"`
		Package string `json:"package"`
		Type    string `json:"type"`
		Pointer bool   `json:"pointer"`
		Slice   bool   `json:"slice"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type == "" || obj.Pointer || obj.Slice {
		return ""
	}
	pkg := obj.Package
	if pkg == "" {
		pkg = obj.Import
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			pkg = pkg[i+1:]
		}
	}
	if pkg == "" {
		return obj.Type
	}
	return pkg + "." + obj.Type
}
