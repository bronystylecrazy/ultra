package main

import (
	"io"
	"strings"
	"testing"
)

// TestTypeTable covers what the golden cannot: the override spellings a
// product might use instead of the two migrate.md shows, and the refusals.
func TestTypeTable(t *testing.T) {
	for _, c := range []struct {
		name, opts string
		// want is the Go type uuid resolves to, or "" for "no factory value".
		want string
	}{
		{"no options at all", "", "pgtype.UUID"},
		{"the documented string form",
			`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID"}]}`, "uuid.UUID"},
		{"the object form sqlc also accepts",
			`{"overrides":[{"db_type":"uuid","go_type":{"import":"github.com/google/uuid","type":"UUID"}}]}`, "uuid.UUID"},
		{"an explicit package name wins over the import path",
			`{"overrides":[{"db_type":"uuid","go_type":{"import":"example.com/x","package":"uuid","type":"UUID"}}]}`, "uuid.UUID"},
		// A nullable override describes the pointer/Null rendering, which no
		// factory fills — and migrate.md sanctions the NOT NULL placement only.
		{"nullable is not ours",
			`{"overrides":[{"db_type":"uuid","go_type":"github.com/google/uuid.UUID","nullable":true}]}`, "pgtype.UUID"},
		// Anything we cannot produce a value for leaves the table, so the
		// affected tables lose their factory and say which column and why.
		{"a target with no deterministic value",
			`{"overrides":[{"db_type":"uuid","go_type":"example.com/ksuid.KSUID"}]}`, ""},
		{"a pointer target is not a value either",
			`{"overrides":[{"db_type":"uuid","go_type":{"import":"github.com/google/uuid","type":"UUID","pointer":true}}]}`, ""},
		{"unreadable options change nothing", `{`, "pgtype.UUID"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var errW strings.Builder
			got, ok := typeTable([]byte(c.opts), &errW)["uuid"]
			switch {
			case c.want == "" && ok:
				t.Errorf("uuid resolved to %s; a type with no factory value must leave the table", got.goType)
			case c.want == "" && errW.Len() == 0:
				t.Error("dropping a type must say so — silence is the failure mode this generator is against")
			case c.want != "" && got.goType != c.want:
				t.Errorf("uuid = %q, want %q", got.goType, c.want)
			}
		})
	}

	// timestamptz is the one the playground actually hit.
	table := typeTable([]byte(`{"overrides":[{"db_type":"timestamptz","go_type":"time.Time"}]}`), io.Discard)
	ts := table["timestamptz"]
	if ts.goType != "time.Time" {
		t.Errorf("timestamptz = %q, want time.Time", ts.goType)
	}
	if lit := ts.lit("ducks", "created_at", "n"); lit != "at(n)" {
		t.Errorf("the fill is %q, want at(n)", lit)
	}
	if len(ts.needs) != 0 {
		t.Errorf("at() is factory-local and needs no import, got %v", ts.needs)
	}
	// An un-overridden type is untouched.
	if table["text"].goType != "string" {
		t.Errorf("text = %q, want string", table["text"].goType)
	}
}
