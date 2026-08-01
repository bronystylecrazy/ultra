package main

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// loadFixture returns one captured product: testdata/sqlc/product is the plain
// one, testdata/sqlc/overrides the same capture from a sqlc.yaml carrying
// migrate.md's uuid/timestamptz overrides, and testdata/sqlc/nullable a
// product whose columns are NULLABLE and whose overrides are keyed by COLUMN.
// Each dir carries the sqlc.yaml it was captured with, because the plugin
// reads that file back for the `gen: go:` facts sqlc never sends.
func loadFixture(t *testing.T, name string) (dir string, req []byte) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "sqlc", name))
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
	_, raw := loadFixture(t, "product")
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
	files, names := goldenRun(t, "product", "golden")
	if files == nil {
		return // -update
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

// TestPluginGoldenOverrides is the bug the playground shipped into: a product
// takes migrate.md's advice, sqlc emits uuid.UUID and time.Time, and a factory
// that still filled pgtype.Timestamptz would not compile against the params
// struct sitting beside it. The overrides ride the codegen entry's `options:`,
// which is the only thing about types a process plugin is sent.
func TestPluginGoldenOverrides(t *testing.T) {
	files, _ := goldenRun(t, "overrides", "golden-overrides")
	if files == nil {
		return // -update
	}
	var factory string
	for _, f := range files {
		if f.name == "factory/ducks.go" {
			factory = string(f.body)
		}
	}
	if factory == "" {
		t.Fatal("the overridden product got no ducks factory")
	}
	for _, want := range []string{
		"CreatedAt: at(n),", // time.Time, not pgtype.Timestamptz
		`OwnerID:   uuid.UUID(uuidAt("ducks", n).Bytes)`, // uuid.UUID, not pgtype.UUID
		`"github.com/google/uuid"`,                       // and the import that spells it
		"Subject:   fmt.Sprintf(",                        // an un-overridden column is untouched
	} {
		if !strings.Contains(factory, want) {
			t.Errorf("factory/ducks.go is missing %q:\n%s", want, factory)
		}
	}
	if strings.Contains(factory, "pgtype") {
		t.Errorf("nothing in this product is a pgtype any more:\n%s", factory)
	}
	// The foreign key is a uuid.UUID now, so the child factory must convert
	// the deterministic value rather than hand over a pgtype.UUID.
	for _, f := range files {
		if f.name != "factory/quacks.go" {
			continue
		}
		if !strings.Contains(string(f.body), "DuckID:    Duck(t, db).ID,") {
			t.Errorf("the fk fill did not survive the override:\n%s", f.body)
		}
	}
	// The cursor half must NOT move: sqlc.narg params are nullable, migrate.md
	// puts overrides on the NOT NULL types only, and ListDucksParams still
	// takes pgtype there.
	for _, f := range files {
		if f.name != "ultra_ducks.go" {
			continue
		}
		if !strings.Contains(string(f.body), "arg.AfterAt = pgtype.Timestamptz{Time: after.At, Valid: true}") {
			t.Errorf("the keyset cursor must stay pgtype:\n%s", f.body)
		}
	}
}

// TestPluginGoldenNullable is the two bugs two real products hit, side by
// side: a NULLABLE column, which sqlc renders *string or pgtype.Timestamptz
// and never string (firefly), and an override keyed by COLUMN rather than
// db_type, which the factory used to ignore outright (duckpond). Both end the
// same way — a fill of the wrong type against the params struct sitting beside
// it — so both are asserted on the fills themselves.
func TestPluginGoldenNullable(t *testing.T) {
	files, _ := goldenRun(t, "nullable", "golden-nullable")
	if files == nil {
		return // -update
	}
	var factory string
	for _, f := range files {
		if f.name == "factory/gizmos.go" {
			factory = string(f.body)
		}
	}
	if factory == "" {
		t.Fatal("the nullable product got no gizmos factory")
	}
	for _, want := range []string{
		"Subject:   fmt.Sprintf(",           // a NOT NULL text is still a plain string
		"Label:     ptr(fmt.Sprintf(",       // a nullable text is *string
		"Tally:     ptr(int32(n)),",         // ... and a nullable int4 is *int32
		"Ratio:     ptr(float64(n)),",       //
		"Active:    ptr(false),",            //
		"SeenAt:    pgtype.Timestamptz{",    // but pgx/v5 leaves timestamptz alone
		`Payload:   []byte("{}"),`,          // and jsonb alone
		`Tag:       uuid.UUID(uuidAt("gizmos", n).Bytes)`, // the column override, on a NULLABLE column
		"CreatedAt: at(n),",                               // and one written with a wildcard table
		`"github.com/google/uuid"`,
	} {
		if !strings.Contains(factory, want) {
			t.Errorf("factory/gizmos.go is missing %q:\n%s", want, factory)
		}
	}
	// The child's fk is a pgtype.UUID and so is the parent's id, so the parent
	// row is taken whole — and its own nullable column still gets a pointer.
	for _, f := range files {
		if f.name != "factory/gadgets.go" {
			continue
		}
		for _, want := range []string{"GizmoID:   Gizmo(t, db).ID,", "Note:      ptr(fmt.Sprintf("} {
			if !strings.Contains(string(f.body), want) {
				t.Errorf("factory/gadgets.go is missing %q:\n%s", want, f.body)
			}
		}
	}
}

// TestParentFill covers the foreign-key path an override can also break: the
// child's fk and the parent's id are two columns, resolved separately, and a
// factory that hands one to the other has to have the SAME type in hand.
func TestParentFill(t *testing.T) {
	parent := func(idType string, create bool) *tableGen {
		g := &tableGen{
			name:  "ducks",
			ident: sqlcIdent{Name: "ducks"},
			cols:  []sqlcColumn{col("id", idType, true)},
			types: typeConfig{schema: "public"},
		}
		if create {
			g.create = &sqlcQuery{Name: "CreateDuck"}
		}
		return g
	}
	child := &tableGen{name: "quacks", ident: sqlcIdent{Name: "quacks"}}

	for _, c := range []struct {
		name, idType, fkType string
		parent               *tableGen
		wantExpr, wantReason string
	}{
		{name: "the same spelling on both sides", idType: "uuid", fkType: "pgtype.UUID",
			parent: parent("uuid", true), wantExpr: "Duck(t, db).ID"},
		{name: "a nullable fk over a pointerable id", idType: "int8", fkType: "*int64",
			parent: parent("int8", true), wantExpr: "ptr(Duck(t, db).ID)"},
		{name: "an override on one side only", idType: "uuid", fkType: "uuid.UUID",
			parent: parent("uuid", true), wantReason: "column duck_id is uuid.UUID but ducks.id is pgtype.UUID"},
		{name: "a parent with no factory", idType: "uuid", fkType: "pgtype.UUID",
			parent: parent("uuid", false), wantReason: "which has no factory of its own"},
		{name: "a parent with no id", idType: "uuid", fkType: "pgtype.UUID",
			parent: &tableGen{name: "ducks", create: &sqlcQuery{}}, wantReason: "which has no id column to take"},
	} {
		t.Run(c.name, func(t *testing.T) {
			expr, reason := parentFill(child, map[string]*tableGen{"ducks": c.parent}, "ducks", "duck_id", c.fkType)
			if expr != c.wantExpr {
				t.Errorf("expr = %q, want %q", expr, c.wantExpr)
			}
			if c.wantReason != "" && !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason = %q, want it to say %q", reason, c.wantReason)
			}
			if c.wantReason == "" && reason != "" {
				t.Errorf("unexpected refusal: %s", reason)
			}
		})
	}
	// A self-reference has no terminating factory, whatever the types say.
	if _, reason := parentFill(child, map[string]*tableGen{"quacks": child}, "quacks", "parent_id", "pgtype.UUID"); !strings.Contains(reason, "would not terminate") {
		t.Errorf("a self-reference must be refused: %q", reason)
	}
}

// TestResolvedTypesMatchSQLC is the drift gate, and the reason this generator
// no longer keeps a type table of its own opinions: every column of the
// nullable fixture is resolved here and compared against the field type SQLC
// ITSELF emitted for it, captured from the same run as the request. A rule
// that falls out of step with sqlc fails here, by name, instead of in a
// product's build.
func TestResolvedTypesMatchSQLC(t *testing.T) {
	dir, raw := loadFixture(t, "nullable")
	req, err := decodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	types := readTypeConfig(dir, req.Settings.Codegen, io.Discard)
	types.schema = req.Catalog.DefaultSchema

	models := sqlcModelFields(t, filepath.Join(dir, "gen", "models.go"))
	if len(models) == 0 {
		t.Fatal("the captured sqlc output carries no model structs")
	}
	var checked int
	for _, s := range req.Catalog.Schemas {
		if s.Name != types.schema {
			continue
		}
		for _, tbl := range s.Tables {
			fields := models[modelName(tbl.Rel.Name)]
			if fields == nil {
				t.Errorf("sqlc emitted no %s model", modelName(tbl.Rel.Name))
				continue
			}
			for _, c := range tbl.Columns {
				got, _ := types.goTypeOf(&tbl.Rel, c)
				if want := fields[goName(c.Name)]; got != want {
					t.Errorf("%s.%s resolved to %q, but sqlc emitted %q", tbl.Rel.Name, c.Name, got, want)
				}
				checked++
			}
		}
	}
	if checked < 14 {
		t.Errorf("only %d columns were compared — the fixture lost its coverage", checked)
	}
}

// sqlcModelFields reads sqlc's own models.go: struct -> field -> Go type, as
// written.
func sqlcModelFields(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		fields := map[string]string{}
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				fields[name.Name] = types.ExprString(f.Type)
			}
		}
		out[spec.Name.Name] = fields
		return true
	})
	return out
}

// goldenRun drives the plugin over one captured fixture and compares (or, with
// -update, rewrites) every emitted byte. It returns nil under -update, because
// there is nothing left for the caller to assert against.
func goldenRun(t *testing.T, fixture, goldenDir string) ([]outFile, []string) {
	t.Helper()
	dir, req := loadFixture(t, fixture)
	golden, err := filepath.Abs(filepath.Join("testdata", "sqlc", goldenDir))
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
		assertImportsUsed(t, f)
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
		return nil, nil
	}
	return files, names
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

// assertImportsUsed is the check gofmt does not do. The plugin computes a
// factory's imports from the literals it decided to write, and an override can
// retire the last pgtype in a file as easily as it can introduce a uuid — an
// import left behind is a generated package that does not compile, which is
// exactly the failure this whole fix is about.
func assertImportsUsed(t *testing.T, f outFile) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), f.name, f.body, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s does not parse: %v", f.name, err)
	}
	used := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		parts := strings.Split(path, "/")
		name := parts[len(parts)-1]
		// A major-version suffix is not the package name: pgx/v5 is pgx.
		if len(parts) > 1 && regexp.MustCompile(`^v\d+$`).MatchString(name) {
			name = parts[len(parts)-2]
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if !used[name] {
			t.Errorf("%s imports %s and never uses it:\n%s", f.name, path, f.body)
		}
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
