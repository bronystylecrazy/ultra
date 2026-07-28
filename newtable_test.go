package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tableFixture is the smallest thing `ultra new table` will accept: a module
// that builds with no dependencies (so the type-check gate passes offline) and
// the internal/db layout the command writes into.
func tableFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/fixture\n\ngo "+scaffoldGoVersion+"\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("internal/db/migrations/00001_init.sql", "-- +goose Up\nSELECT 1;\n\n-- +goose Down\nSELECT 1;\n")
	write("internal/db/queries/.gitkeep", "")
	return dir
}

func runNewTable(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errW bytes.Buffer
	code := cmdNewTable(append(args, dir), &out, &errW)
	return code, out.String(), errW.String()
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestNewTableOwned is the headline path: the migration remembers law 11, the
// index is the page order, and ownership reaches every WHERE.
func TestNewTableOwned(t *testing.T) {
	dir := tableFixture(t)
	code, out, errW := runNewTable(t, dir, "notes", "body text not null", "--owned", "--no-store")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errW)
	}
	if !strings.Contains(errW, "✓ new table notes") || !strings.Contains(errW, "subject-owned") {
		t.Errorf("the verdict must name the table and the ownership: %q", errW)
	}

	mig := read(t, dir, "internal/db/migrations/00002_notes.sql")
	for _, want := range []string{
		"-- +goose Up",
		"law 11",
		"created_at timestamptz not null",
		"subject text not null",
		"body text not null",
		"create index notes_page_idx on notes (subject, created_at desc, id desc);",
		"drop table notes;",
	} {
		if !strings.Contains(mig, want) {
			t.Errorf("the migration is missing %q:\n%s", want, mig)
		}
	}
	if strings.Contains(mig, "created_at timestamptz not null default") {
		t.Error("created_at must carry NO default — that is law 11")
	}

	q := read(t, dir, "internal/db/queries/notes.sql")
	for _, want := range []string{
		"-- name: CreateNote :one",
		"insert into notes (subject, body, created_at)",
		"values ($1, $2, $3)",
		"-- name: GetNote :one",
		"select * from notes where id = $1 and subject = $2;",
		"-- name: ListNotes :many",
		"sqlc.narg('after_at')::timestamptz",
		"(created_at, id) < (sqlc.narg('after_at')::timestamptz, sqlc.narg('after_id')::uuid)",
		"order by created_at desc, id desc",
		"limit $2;",
		"-- name: UpdateNote :one",
		"update notes set body = $3",
		"-- name: DeleteNote :execrows",
		"delete from notes where id = $1 and subject = $2;",
		"scaffolded once, then YOURS",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("the queries file is missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "DO NOT EDIT") {
		t.Error("the queries file is the developer's — it must not claim to be generated")
	}
}

// TestNewTableUnowned proves --owned is genuinely subtractive: no subject
// column, no subject in the index, no subject in any WHERE.
func TestNewTableUnowned(t *testing.T) {
	dir := tableFixture(t)
	if code, out, errW := runNewTable(t, dir, "notes", "body text not null", "--no-store"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errW)
	}
	mig := read(t, dir, "internal/db/migrations/00002_notes.sql")
	if strings.Contains(mig, "subject") {
		t.Errorf("an unowned table must carry no subject:\n%s", mig)
	}
	if !strings.Contains(mig, "create index notes_page_idx on notes (created_at desc, id desc);") {
		t.Errorf("the keyset index must drop the owner column:\n%s", mig)
	}
	q := read(t, dir, "internal/db/queries/notes.sql")
	if strings.Contains(q, "subject") {
		t.Errorf("an unowned table must carry no subject in its queries:\n%s", q)
	}
	if !strings.Contains(q, "delete from notes where id = $1;") {
		t.Errorf("the delete must be keyed on id alone:\n%s", q)
	}
	if !strings.Contains(q, "limit $1;") {
		t.Errorf("without an owner the limit is the first parameter:\n%s", q)
	}
}

// TestNewTableStore covers the Go half: the store lands as a plain-noun file
// in the feature, and errors.go gains the sentinel Remove returns.
func TestNewTableStore(t *testing.T) {
	dir := tableFixture(t)
	if code, out, errW := runNewTable(t, dir, "notes", "body text not null", "--owned"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errW)
	}
	store := read(t, dir, "internal/app/notes/note.go")
	for _, want := range []string{
		"package notes",
		`"example.com/fixture/internal/db/gen"`,
		"type Store struct {",
		"db    *gen.DB",
		"clock di.Clock",
		"func NewStore(db *gen.DB, clock di.Clock) *Store",
		"arg.CreatedAt = pgtype.Timestamptz{Time: s.clock.Now(), Valid: true}",
		`diag.Wrap(gen.NotesError(err), "creating note", "subject", arg.Subject)`,
		"s.db.Q().ListNotesPage(ctx, subject, after, size)",
		"return ErrNotFound",
	} {
		if !strings.Contains(store, want) {
			t.Errorf("the store is missing %q:\n%s", want, store)
		}
	}
	if errs := read(t, dir, "internal/app/notes/errors.go"); !strings.Contains(errs, "var ErrNotFound = errors.New(\"notes: not found\")") {
		t.Errorf("Remove returns ErrNotFound, so the contract page must declare it:\n%s", errs)
	}
	// The feature is created on the way, so the store has somewhere to live.
	if _, err := os.Stat(filepath.Join(dir, "internal/app/notes/notes.go")); err != nil {
		t.Errorf("the feature package was not scaffolded: %v", err)
	}
}

// TestNewTableSecondStore is the redeclaration trap: a second table aimed at a
// feature that already has a Store must add METHODS, not a second type.
func TestNewTableSecondStore(t *testing.T) {
	// The fixture module has no dependencies, so the Store an earlier table
	// would have left stands in as the smallest thing that still compiles.
	// What the real one looks like is TestNewTableStore's job.
	dir := tableFixture(t)
	if err := os.MkdirAll(filepath.Join(dir, "internal/app/ducks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal/app/ducks/duck.go"),
		[]byte("package ducks\n\ntype Store struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errW := runNewTable(t, dir, "quacks", "loudness integer not null", "--feature", "ducks")
	if code != 0 {
		t.Fatalf("a second table in one feature must land: %d\n%s\n%s", code, out, errW)
	}
	quack := read(t, dir, "internal/app/ducks/quack.go")
	for _, want := range []string{
		"package ducks",
		"func (s *Store) CreateQuack(ctx context.Context, arg gen.CreateQuackParams) (gen.Quack, error)",
		"func (s *Store) QuacksPage(ctx context.Context, after *gen.QuacksCursor, size int) ([]gen.Quack, bool, error)",
		"func (s *Store) RemoveQuack(ctx context.Context, id pgtype.UUID) error",
	} {
		if !strings.Contains(quack, want) {
			t.Errorf("the methods-only file is missing %q:\n%s", want, quack)
		}
	}
	for _, never := range []string{"type Store struct", "func NewStore", "di.Clock"} {
		if strings.Contains(quack, never) {
			t.Errorf("the second table must not redeclare %q:\n%s", never, quack)
		}
	}
	// The store lives in duck.go and stays alone there.
	if n := strings.Count(read(t, dir, "internal/app/ducks/duck.go"), "type Store struct"); n != 1 {
		t.Errorf("duck.go declares Store %d times, want 1", n)
	}
	if !strings.Contains(out, "already has a Store (duck.go)") {
		t.Errorf("the report must say the store was extended, not created:\n%s", out)
	}

	// The noun-file check runs before ANY write, so a refusal leaves no
	// orphan migration in front of a build that cannot pass.
	if err := os.WriteFile(filepath.Join(dir, "internal/app/ducks/pond.go"),
		[]byte("package ducks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(filepath.Join(dir, "internal/db/migrations"))
	code, _, errW = runNewTable(t, dir, "ponds", "depth integer not null", "--feature", "ducks")
	if code == 0 {
		t.Fatal("a noun file that already exists must be refused")
	}
	if !strings.Contains(errW, "nothing was written") {
		t.Errorf("the refusal must say nothing landed: %q", errW)
	}
	if now, _ := os.ReadDir(filepath.Join(dir, "internal/db/migrations")); len(now) != len(before) {
		t.Errorf("the refusal wrote a migration anyway: %d, want %d", len(now), len(before))
	}
}

// TestNewTableAppendsMigration is the parent-and-child case: one deploy step,
// and a Down that drops in reverse.
func TestNewTableAppendsMigration(t *testing.T) {
	dir := tableFixture(t)
	if code, out, errW := runNewTable(t, dir, "notes", "body text not null", "--migration", "1", "--no-store"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errW)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/db/migrations/00002_notes.sql")); err == nil {
		t.Error("--migration must fold into the existing file, not add one")
	}
	mig := read(t, dir, "internal/db/migrations/00001_init.sql")
	up := strings.Index(mig, "create table notes")
	down := strings.Index(mig, "-- +goose Down")
	drop := strings.Index(mig, "drop table notes;")
	switch {
	case up < 0 || down < 0 || drop < 0:
		t.Fatalf("the DDL did not land:\n%s", mig)
	case up > down:
		t.Errorf("the create belongs in the Up section:\n%s", mig)
	case drop < down:
		t.Errorf("the drop belongs in the Down section:\n%s", mig)
	}

	// A migration that is not there must be refused, and the refusal must say
	// what IS there.
	_, _, errW := runNewTable(t, tableFixture(t), "notes", "body text not null", "--migration", "9", "--no-store")
	if !strings.Contains(errW, "00001_init.sql") {
		t.Errorf("the refusal must list the migrations it found: %q", errW)
	}
}

func TestNewTableRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"dashed name", []string{"my-notes", "body text"}, "SQL identifier"},
		{"reserved column", []string{"notes", "created_at timestamptz not null"}, "written for you"},
		{"reserved id", []string{"notes", "id uuid"}, "written for you"},
		{"no columns", []string{"notes", "  "}, "no columns given"},
		{"underscored feature", []string{"notes", "body text", "--feature", "my_notes"}, "Go package name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := tableFixture(t)
			code, _, errW := runNewTable(t, dir, append(c.args, "--no-store")...)
			if code == 0 {
				t.Fatal("this must be refused")
			}
			if !strings.Contains(errW, c.want) {
				t.Errorf("the refusal must say %q, got: %s", c.want, errW)
			}
		})
	}

	// A second definition of the same table is refused, and the refusal names
	// the file that already has it.
	dir := tableFixture(t)
	if code, _, errW := runNewTable(t, dir, "notes", "body text not null", "--no-store"); code != 0 {
		t.Fatalf("the first table must land: %s", errW)
	}
	code, _, errW := runNewTable(t, dir, "notes", "title text not null", "--no-store")
	if code == 0 {
		t.Fatal("a table that already exists must be refused")
	}
	if !strings.Contains(errW, "00002_notes.sql") {
		t.Errorf("the refusal must name where the table already is: %q", errW)
	}

	// A module that does not compile is refused BEFORE anything is written.
	broken := tableFixture(t)
	if err := os.WriteFile(filepath.Join(broken, "main.go"), []byte("package main\nfunc main() { undefined() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errW = runNewTable(t, broken, "notes", "body text not null", "--no-store")
	if code == 0 {
		t.Fatal("a broken module must be refused")
	}
	if !strings.Contains(errW, "does not build") {
		t.Errorf("the refusal must name the gate: %q", errW)
	}
	if _, err := os.Stat(filepath.Join(broken, "internal/db/queries/notes.sql")); err == nil {
		t.Error("the gate must refuse BEFORE writing anything")
	}
}

// TestFillColumns pins the placeholder arithmetic the five queries share.
func TestFillColumns(t *testing.T) {
	owned := tableData{Owned: true}
	if err := fillColumns(&owned, "title text not null, body text, price numeric(10,2) not null"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(owned.Cols, ","); got != "title,body,price" {
		t.Errorf("columns = %q; numeric(10,2) must not split on its own comma", got)
	}
	if owned.InsertCol != "subject, title, body, price, created_at" {
		t.Errorf("insert columns = %q", owned.InsertCol)
	}
	if owned.InsertVal != "$1, $2, $3, $4, $5" {
		t.Errorf("insert values = %q", owned.InsertVal)
	}
	if owned.UpdateID != "id = $1 and subject = $2" {
		t.Errorf("update where = %q", owned.UpdateID)
	}
	if owned.UpdateSet != "title = $3, body = $4, price = $5" {
		t.Errorf("update set = %q", owned.UpdateSet)
	}
	if owned.IndexCols != "subject, created_at desc, id desc" {
		t.Errorf("index = %q", owned.IndexCols)
	}

	plain := tableData{}
	if err := fillColumns(&plain, "body text not null, unique (body)"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plain.Cols, ","); got != "body" {
		t.Errorf("a table constraint is not a column: %q", got)
	}
	if plain.UpdateID != "id = $1" || plain.UpdateSet != "body = $2" {
		t.Errorf("unowned numbering: where=%q set=%q", plain.UpdateID, plain.UpdateSet)
	}
}

// TestNewTableCovenant is the whole wave, end to end and for real: scaffold a
// product against THIS checkout, add a table to it, let sqlc run with this
// binary as its plugin, and make the result compile. It is the only test that
// proves the two halves fit — the queries `ultra new table` writes are the
// input the plugin's shape detection reads, and nothing but a real sqlc run
// connects them.
func TestNewTableCovenant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a product and runs sqlc; skipped in -short")
	}
	if _, err := exec.LookPath("sqlc"); err != nil {
		t.Skip("no sqlc on PATH, so the generated half was never exercised: " + err.Error())
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	// This binary IS the plugin the scaffolded sqlc.yaml names, so it has to
	// be on PATH under that name before sqlc runs.
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "ultra"), "./cmd/ultra")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the plugin: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := testData("tablecheck", scaffoldData{DB: true})
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	sh := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			if offline(out) {
				t.Skipf("%s %s needs the network: %s", name, strings.Join(args, " "), out)
			}
			t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	sh("go", "mod", "edit",
		"-replace="+modulePath+"="+repoRoot,
		"-replace="+modulePath+"/contrib="+filepath.Join(repoRoot, "contrib"))
	sh("go", "mod", "tidy")

	code, out, errW := runNewTable(t, dir, "notes", "body text not null", "--owned")
	if code != 0 {
		t.Fatalf("ultra new table exit %d\n%s\n%s", code, out, errW)
	}
	if !strings.Contains(out, "sqlc generate     ok") {
		t.Fatalf("sqlc did not run:\n%s\n%s", out, errW)
	}

	gen := filepath.Join(dir, "internal", "db", "gen")
	body, err := os.ReadFile(filepath.Join(gen, "ultra_notes.go"))
	if err != nil {
		t.Fatalf("the plugin wrote no ultra_notes.go: %v", err)
	}
	for _, want := range []string{
		"DO NOT EDIT — regenerate: sqlc generate",
		"func NotesError(err error) error",
		`if pgErr.Code == "42P01"`,
		"type NotesCursor struct",
		"func (q *Queries) ListNotesPage(ctx context.Context, subject string, after *NotesCursor, size int) ([]Note, bool, error)",
		`diag.Wrap(NotesError(err), "query ListNotes", "size", size, "cursor", after != nil)`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("gen/ultra_notes.go is missing %q:\n%s", want, body)
		}
	}
	if _, err := os.Stat(filepath.Join(gen, "factory", "notes.go")); err != nil {
		t.Errorf("the plugin wrote no factory: %v", err)
	}
	for _, f := range []string{"ultra.go", filepath.Join("factory", "ultra.go")} {
		b, err := os.ReadFile(filepath.Join(gen, f))
		if err != nil {
			t.Fatalf("gen/%s: %v", f, err)
		}
		if !strings.HasPrefix(string(b), "// Code generated by sqlc (ultra plugin). DO NOT EDIT") {
			t.Errorf("gen/%s does not lead with the generated header", f)
		}
	}

	// Only now does the product have to compile — and if the one thing
	// missing is the transaction helper contrib is growing in parallel, say so
	// rather than failing for someone else's unlanded commit.
	build = exec.Command("go", "build", "./...")
	build.Dir = dir
	if compiled, err := build.CombinedOutput(); err != nil {
		if strings.Contains(string(compiled), "undefined: pg.Atomic") {
			t.Skipf("the generated gen.DB.Atomic awaits contrib/pg.Atomic, which is not in this checkout yet:\n%s", compiled)
		}
		t.Fatalf("the product does not build with its new table: %v\n%s", err, compiled)
	}

	// A SECOND table into the SAME feature: one Store, methods named for the
	// table. This is the redeclaration the playground hit, and only a real
	// build proves it is gone.
	if code, out, errW := runNewTable(t, dir, "quacks", "loudness integer not null", "--feature", "notes", "--owned"); code != 0 {
		t.Fatalf("a second table in one feature must land: %d\n%s\n%s", code, out, errW)
	}
	build = exec.Command("go", "build", "./...")
	build.Dir = dir
	if compiled, err := build.CombinedOutput(); err != nil {
		t.Fatalf("two tables in one feature must still build:\n%s", compiled)
	}

	// Re-running must refuse: the table is already defined, and the SQL on
	// disk belongs to the developer now.
	if code, _, errW := runNewTable(t, dir, "notes", "title text not null", "--owned"); code == 0 {
		t.Errorf("a second `new table notes` must be refused: %s", errW)
	}
}
