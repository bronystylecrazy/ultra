package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden rewrites the expected plugin output instead of comparing it.
//
//	go test ./cmd/ultra -run TestPluginGolden -update
var updateGolden = flag.Bool("update", false, "rewrite the sqlc plugin's golden files")

// fixtureRequest is a REAL CodeGenRequest, captured by running sqlc v1.31.1
// against testdata/sqlc/product with a byte-dumping process plugin in place of
// this one. That provenance is the point: the field numbers in sqlcproto.go
// are transcribed from a generated .pb.go, and only a genuine sqlc payload can
// prove the transcription. It carries the whole pg_catalog too, exactly as a
// real request does — the plugin must ignore it.
const fixtureRequest = "codegen_request.bin"

func loadFixture(t *testing.T) (dir string, req []byte) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "sqlc", "product"))
	if err != nil {
		t.Fatal(err)
	}
	req, err = os.ReadFile(filepath.Join(dir, fixtureRequest))
	if err != nil {
		t.Fatal(err)
	}
	return dir, req
}

// TestDecodeRequest is the field-number gate. It asserts the facts the
// emitters read straight off the wire, so a renumbering in a future
// plugin-sdk-go fails HERE with a readable diff rather than downstream as
// mysteriously empty generated code.
func TestDecodeRequest(t *testing.T) {
	_, raw := loadFixture(t)
	req, err := decodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.Version == "" || !strings.HasPrefix(req.Version, "v1.") {
		t.Errorf("sqlc_version = %q, want a v1.x string", req.Version)
	}
	if req.Settings.Engine != "postgresql" {
		t.Errorf("engine = %q, want postgresql", req.Settings.Engine)
	}
	if got := req.Settings.Schema; len(got) != 1 || got[0] != "migrations" {
		t.Errorf("schema = %v, want [migrations]", got)
	}
	if req.Settings.Codegen.Out != "gen" || req.Settings.Codegen.Plugin != "ultra" {
		t.Errorf("codegen = %+v, want out=gen plugin=ultra", req.Settings.Codegen)
	}
	if req.Catalog.DefaultSchema != "public" {
		t.Errorf("default schema = %q, want public", req.Catalog.DefaultSchema)
	}

	var notes *sqlcTable
	for i, s := range req.Catalog.Schemas {
		if s.Name != "public" {
			continue
		}
		for j := range s.Tables {
			if s.Tables[j].Rel.Name == "notes" {
				notes = &req.Catalog.Schemas[i].Tables[j]
			}
		}
	}
	if notes == nil {
		t.Fatal("the public schema carries no notes table")
	}
	want := []struct {
		name, typ string
		notNull   bool
	}{
		{"id", "uuid", true}, {"subject", "text", true}, {"title", "text", true},
		{"body", "text", true}, {"weight", "int4", true}, {"pinned", "bool", true},
		{"created_at", "timestamptz", true},
	}
	if len(notes.Columns) != len(want) {
		t.Fatalf("notes has %d columns, want %d", len(notes.Columns), len(want))
	}
	for i, w := range want {
		c := notes.Columns[i]
		if c.Name != w.name || baseType(c.Type.Name) != w.typ || c.NotNull != w.notNull {
			t.Errorf("column %d = %s/%s/notnull=%v, want %s/%s/notnull=%v",
				i, c.Name, c.Type.Name, c.NotNull, w.name, w.typ, w.notNull)
		}
	}

	// The two query facts every wrapper depends on: the narg pair is flagged
	// as named and nullable, and the insert names its table.
	var list, create *sqlcQuery
	for i := range req.Queries {
		switch req.Queries[i].Name {
		case "ListNotes":
			list = &req.Queries[i]
		case "CreateNote":
			create = &req.Queries[i]
		}
	}
	if list == nil || create == nil {
		t.Fatal("the fixture lost ListNotes or CreateNote")
	}
	if list.Cmd != ":many" {
		t.Errorf("ListNotes cmd = %q, want :many", list.Cmd)
	}
	var nargs int
	for _, p := range list.Params {
		if p.Column.IsNamedParam && !p.Column.NotNull {
			nargs++
		}
	}
	if nargs != 2 {
		t.Errorf("ListNotes has %d nullable named params, want the after_at/after_id pair", nargs)
	}
	if create.InsertIntoTable.GetName() != "notes" {
		t.Errorf("CreateNote insert_into_table = %q, want notes", create.InsertIntoTable.GetName())
	}
}

// TestPluginGolden runs the whole plugin over the captured request and
// compares every emitted byte.
func TestPluginGolden(t *testing.T) {
	dir, req := loadFixture(t)
	golden, err := filepath.Abs(filepath.Join("testdata", "sqlc", "golden"))
	if err != nil {
		t.Fatal(err)
	}
	// The plugin reads its working directory: that is where sqlc puts it, and
	// how it finds both the migrations and the product's go.mod.
	t.Chdir(dir)

	var out, errW bytes.Buffer
	if code := runPlugin(bytes.NewReader(req), &out, &errW); code != 0 {
		t.Fatalf("the plugin refused the request (%d): %s", code, errW.String())
	}
	files := decodeGeneratedFiles(t, out.Bytes())

	if *updateGolden {
		os.RemoveAll(golden)
		if err := os.MkdirAll(golden, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for _, f := range files {
		names = append(names, f.name)
		path := filepath.Join(golden, strings.ReplaceAll(f.name, "/", "__"))
		if *updateGolden {
			if err := os.WriteFile(path, f.body, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("no golden for %s (run with -update): %v", f.name, err)
			continue
		}
		if !bytes.Equal(want, f.body) {
			t.Errorf("%s differs from its golden:\n--- got ---\n%s", f.name, f.body)
		}
	}
	if *updateGolden {
		t.Log("golden files rewritten:", strings.Join(names, ", "))
		return
	}

	// The file SET is part of the contract: shapes has no factory (a box
	// column cannot be defaulted) and comments' factory must exist, because
	// its foreign key resolves through notes'.
	want := []string{
		"ultra.go", "ultra_comments.go", "ultra_notes.go", "ultra_shapes.go",
		"factory/ultra.go", "factory/comments.go", "factory/notes.go",
	}
	got := strings.Join(names, " ")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("the plugin emitted %v, missing %s", names, w)
		}
	}
	if strings.Contains(got, "factory/shapes.go") {
		t.Error("shapes has a box column and must get NO factory")
	}
}

// TestPluginRefusesGarbage keeps a malformed request from being read as an
// empty one — silence is the failure mode this whole design is against.
func TestPluginRefusesGarbage(t *testing.T) {
	var out, errW bytes.Buffer
	// Wire type 7 does not exist.
	if code := runPlugin(bytes.NewReader([]byte{0x0f, 0x01, 0x02}), &out, &errW); code == 0 {
		t.Error("a malformed CodeGenRequest must not exit 0")
	}
	if !strings.Contains(errW.String(), "cannot read") {
		t.Errorf("the refusal must say what it could not read, got: %s", errW.String())
	}
}

// TestPluginMode covers the detection, which runs before any argument
// parsing: sqlc sets SQLC_VERSION, and a test's stdin is never a terminal.
func TestPluginMode(t *testing.T) {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	tty, err := os.Open(os.DevNull) // a character device, like a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()

	t.Setenv("SQLC_VERSION", "")
	os.Unsetenv("SQLC_VERSION")
	if pluginMode(pipeR) {
		t.Error("without SQLC_VERSION this is a human at a shell")
	}
	t.Setenv("SQLC_VERSION", "v1.31.1")
	if !pluginMode(pipeR) {
		t.Error("SQLC_VERSION plus a piped stdin is sqlc calling")
	}
	if pluginMode(tty) {
		t.Error("a character-device stdin is a terminal, not sqlc's pipe")
	}
}

// TestSchemaFacts covers the constraint scraping the proto cannot supply.
func TestSchemaFacts(t *testing.T) {
	facts := schemaFacts{uniques: map[string][]uniqueFact{}, fks: map[string]map[string]string{}}
	scanDDL(`
-- +goose Up
create table notes (
    id uuid primary key,
    subject text not null,
    slug text not null unique,
    price numeric(10,2) not null,
    unique (subject, slug),
    constraint notes_alt_key unique (slug, price)
);
create unique index notes_lower_uq on notes (subject, price);
alter table notes add constraint notes_added_key unique (price);

create table comments (
    id uuid primary key,
    note_id uuid not null references notes (id),
    author_id uuid not null,
    foreign key (author_id) references users (id)
);

-- +goose Down
drop table comments;
`, &facts)

	var names []string
	for _, u := range facts.uniques["notes"] {
		names = append(names, u.name)
	}
	for _, want := range []string{
		"notes_slug_key",         // a column-level UNIQUE, named the way Postgres names it
		"notes_subject_slug_key", // a table-level UNIQUE, same rule
		"notes_alt_key",          // named explicitly
		"notes_lower_uq",         // a separate CREATE UNIQUE INDEX
		"notes_added_key",        // added by ALTER TABLE
	} {
		if !contains(names, want) {
			t.Errorf("missed unique constraint %s, found %v", want, names)
		}
	}
	// numeric(10,2) must not split into two items on its own comma.
	if len(facts.uniques["notes"]) != 5 {
		t.Errorf("notes has %d uniques, want 5: %v", len(facts.uniques["notes"]), names)
	}
	if got := facts.fks["comments"]["note_id"]; got != "notes" {
		t.Errorf("comments.note_id references %q, want notes", got)
	}
	if got := facts.fks["comments"]["author_id"]; got != "users" {
		t.Errorf("comments.author_id references %q, want users", got)
	}
}

// decodeGeneratedFiles reads a CodeGenResponse — the test's half of the wire
// format, so encodeResponse is proven against a reader that is not itself.
func decodeGeneratedFiles(t *testing.T, b []byte) []outFile {
	t.Helper()
	var files []outFile
	err := scanProto(b, func(num int, _ uint64, val []byte) error {
		if num != 1 {
			return nil
		}
		var f outFile
		if err := scanProto(val, func(num int, _ uint64, val []byte) error {
			switch num {
			case 1:
				f.name = string(val)
			case 2:
				f.body = val
			}
			return nil
		}); err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
