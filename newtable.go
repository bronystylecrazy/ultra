package main

// `ultra new table` — one command, the whole persistence slice.
//
// A new table used to be six edits in four places, in an order you had to
// know: pick the next goose number, write DDL that remembers law 11, write
// five queries whose keyset shape the paging wrapper depends on, run sqlc,
// write the store, wire it. Every step is derivable from the table name and
// its columns, so all six happen here — and then the SQL is YOURS. Nothing
// regenerates the migration or the queries; the only file that keeps updating
// itself is the one under gen/, which the sqlc plugin owns.

import (
	"fmt"
	"go/format"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// identRe is the identifier rule the migration runner enforces on names, and
// the same shape Postgres lets you write unquoted.
var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// migrationNumRe reads the goose sequence off a migration file name.
var migrationNumRe = regexp.MustCompile(`^(\d+)_`)

// tableData is what the three table templates render against. Every field is
// precomputed so the templates stay readable SQL and Go rather than logic.
type tableData struct {
	Table     string // notes
	Singular  string // note
	Model     string // Note — sqlc's row struct
	Export    string // Notes — the plural stem of the generated symbols
	Feature   string // the package the store lands in
	GenImport string // the sqlc output package's import path
	Owned     bool   // --owned: every row belongs to a subject
	ColsSQL   string // the caller's column SQL, indented, one per line
	Cols      []string
	IndexCols string // the keyset index's column list
	InsertCol string // the INSERT column list
	InsertVal string // its placeholder list
	UpdateSet string // the UPDATE assignment list
	UpdateID  string // the WHERE placeholders for UPDATE
	Migration string // the file the DDL landed in, for the report
	StoreIn   string // the feature file that ALREADY declares Store, if any
}

func cmdNewTable(args []string, out, errW io.Writer) int {
	var name, cols, feature, migration, dir string
	var owned, noStore bool
	rest := args
	for len(rest) > 0 {
		a := rest[0]
		switch {
		case a == "--owned":
			owned, rest = true, rest[1:]
		case a == "--no-store":
			noStore, rest = true, rest[1:]
		case a == "--feature" && len(rest) > 1:
			feature, rest = rest[1], rest[2:]
		case a == "--migration" && len(rest) > 1:
			migration, rest = rest[1], rest[2:]
		case strings.HasPrefix(a, "-"):
			node := ultraTree().find("new").find("table")
			fmt.Fprintf(errW, "Error: unknown flag %q for %q\n\n", a, node.path())
			node.help(errW)
			return 2
		case name == "":
			name, rest = a, rest[1:]
		case cols == "":
			cols, rest = a, rest[1:]
		case dir == "":
			dir, rest = a, rest[1:]
		default:
			ultraTree().find("new").find("table").help(errW)
			return 2
		}
	}
	if name == "" || cols == "" {
		ultraTree().find("new").find("table").help(errW)
		return 2
	}
	if dir == "" {
		dir = "."
	}
	if !identRe.MatchString(name) {
		fmt.Fprintf(errW, "table name %q must be lowercase letters, digits and underscores, starting with a letter — the rule the migration runner enforces\n", name)
		failVerdict(errW, "new table "+name, "not a SQL identifier")
		return 1
	}

	d := tableData{
		Table:    name,
		Singular: singular(name),
		Model:    modelName(name),
		Export:   goName(name),
		Owned:    owned,
		Feature:  feature,
	}
	if d.Feature == "" {
		// A table's default home is a feature of its own name. Go package
		// names carry no underscores, so order_items lands in orderitems —
		// said out loud rather than silently.
		d.Feature = strings.ReplaceAll(name, "_", "")
	}
	if !pkgNameRe.MatchString(d.Feature) {
		fmt.Fprintf(errW, "feature name %q must be a Go package name: lowercase letters and digits — pass --feature\n", d.Feature)
		failVerdict(errW, "new table "+name, "not a Go package name")
		return 1
	}
	if err := fillColumns(&d, cols); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new table "+name, err.Error())
		return 1
	}

	migrations := filepath.Join(dir, "internal", "db", "migrations")
	queriesDir := filepath.Join(dir, "internal", "db", "queries")
	if _, err := os.Stat(migrations); err != nil {
		fmt.Fprintf(errW, "no %s here — run `ultra new table` from a product root scaffolded with --db\n", migrations)
		failVerdict(errW, "new table "+name, "not a --db product root")
		return 1
	}

	// The type-check gate. The plugin cannot go-build mid-sqlc — the package
	// it is emitting IS part of the build — so the gate lives here instead,
	// BEFORE anything is written: adding a table to a module that does not
	// compile buries the real error under generated code.
	if err := gateBuild(dir, out); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new table "+name, "the module does not build yet")
		return 1
	}

	if where := tableAlreadyThere(name, migrations, queriesDir); where != "" {
		fmt.Fprintf(errW, "%q already appears in %s — refusing to write a second definition\n", name, where)
		failVerdict(errW, "new table "+name, "already defined in "+filepath.Base(where))
		return 1
	}

	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil || modulePathOf(string(gomod)) == "" {
		fmt.Fprintf(errW, "no readable module path in %s — run `ultra new table` from the product root\n",
			filepath.Join(dir, "go.mod"))
		failVerdict(errW, "new table "+name, "no module path")
		return 1
	}
	d.GenImport = modulePathOf(string(gomod)) + "/internal/db/gen"

	if !noStore {
		if err := planStore(dir, &d); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "new table "+name, err.Error())
			return 1
		}
	}

	written, err := writeTable(dir, &d, migration, noStore)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new table "+name, err.Error())
		return 1
	}
	for _, f := range written {
		fmt.Fprintln(out, "wrote", f)
	}

	generated := runSQLC(dir, out, errW)
	printTableNextSteps(out, &d, noStore, generated)

	detail := fmt.Sprintf("%s in %s, %s", d.Table, d.Migration, count(len(written), "file"))
	if owned {
		detail += ", subject-owned"
	}
	if !generated {
		detail += " — sqlc did not run"
	}
	verdict(errW, "new table "+name, detail)
	return 0
}

// fillColumns parses the caller's column SQL for the names Create and Update
// must list explicitly. It validates nothing else: the columns are handed to
// Postgres verbatim, and Postgres is the authority on whether they are legal.
func fillColumns(d *tableData, cols string) error {
	var lines []string
	for _, item := range splitTopLevel(cols) {
		item = strings.TrimSpace(strings.Trim(strings.TrimSpace(item), ";"))
		if item == "" {
			continue
		}
		lines = append(lines, "    "+item+",")
		fields := strings.Fields(item)
		head := strings.ToLower(strings.Trim(fields[0], `"`))
		switch head {
		case "primary", "unique", "foreign", "check", "constraint", "exclude", "like":
			continue // a table constraint, not a column
		}
		if !identRe.MatchString(head) {
			return fmt.Errorf("column %q is not a SQL identifier — expected `name type [modifiers]`, comma separated", fields[0])
		}
		switch head {
		case "id", "created_at", "subject":
			return fmt.Errorf("column %q is written for you — id, created_at, and subject (under --owned) are the table's spine", head)
		}
		d.Cols = append(d.Cols, head)
	}
	if len(d.Cols) == 0 {
		return fmt.Errorf("no columns given — `ultra new table notes \"body text not null\"`")
	}
	d.ColsSQL = strings.Join(lines, "\n")

	// The five queries reference columns by position, so the placeholder
	// numbering is computed once here rather than in three templates.
	insert := append([]string{}, d.Cols...)
	where := []string{"id"}
	if d.Owned {
		insert = append([]string{"subject"}, insert...)
		where = append(where, "subject")
		d.IndexCols = "subject, created_at desc, id desc"
	} else {
		d.IndexCols = "created_at desc, id desc"
	}
	insert = append(insert, "created_at")
	d.InsertCol = strings.Join(insert, ", ")
	var vals []string
	for i := range insert {
		vals = append(vals, "$"+strconv.Itoa(i+1))
	}
	d.InsertVal = strings.Join(vals, ", ")

	var wheres, sets []string
	for i, c := range where {
		wheres = append(wheres, c+" = $"+strconv.Itoa(i+1))
	}
	for i, c := range d.Cols {
		sets = append(sets, c+" = $"+strconv.Itoa(len(where)+i+1))
	}
	d.UpdateID = strings.Join(wheres, " and ")
	d.UpdateSet = strings.Join(sets, ", ")
	return nil
}

// gateBuild refuses to add a table to a module that does not compile.
func gateBuild(dir string, out io.Writer) error {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("`go build ./...` fails here, so nothing was written — fix the module first:\n%s", b)
	}
	fmt.Fprintln(out, "go build ./...   ok")
	return nil
}

// tableAlreadyThere returns the file that already mentions this table, so the
// refusal can name it.
func tableAlreadyThere(name string, dirs ...string) string {
	re := regexp.MustCompile(`(?i)(create\s+table\s+(if\s+not\s+exists\s+)?"?` + name + `"?\b|\bfrom\s+"?` + name + `"?\b|\binto\s+"?` + name + `"?\b|\bupdate\s+"?` + name + `"?\b)`)
	var found string
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(path)
			if err == nil && re.Match(b) && found == "" {
				found = path
			}
		}
	}
	return found
}

// storeDeclRe finds the one declaration a second table in the same feature
// must not repeat.
var storeDeclRe = regexp.MustCompile(`(?m)^type Store struct\b`)

// planStore settles what the feature needs BEFORE anything is written: a whole
// Store, or methods on the one it already has. Every refusal reachable here
// belongs here — a collision discovered halfway through writeTable leaves a
// migration and five queries on disk in front of a build that fails.
func planStore(dir string, d *tableData) error {
	feat := filepath.Join(dir, "internal", "app", d.Feature)
	entries, _ := os.ReadDir(feat)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(feat, e.Name())); err == nil && storeDeclRe.Match(b) {
			d.StoreIn = e.Name()
			break
		}
	}
	store := filepath.Join("internal", "app", d.Feature, d.Singular+".go")
	if _, err := os.Stat(filepath.Join(dir, store)); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite, and nothing was written", store)
	}
	return nil
}

// writeTable renders the migration, the queries and (unless --no-store) the
// feature's store, and returns the paths it wrote.
func writeTable(dir string, d *tableData, migration string, noStore bool) ([]string, error) {
	written := []string{}
	if migration == "" {
		up, err := renderTemplate("table/migration_body.sql.tmpl", d)
		if err != nil {
			return nil, err
		}
		d.Migration = filepath.Join("internal", "db", "migrations",
			nextMigration(filepath.Join(dir, "internal", "db", "migrations"), d.Table))
		body := "-- +goose Up\n" + up + "\n-- +goose Down\ndrop table " + d.Table + ";\n"
		if err := os.WriteFile(filepath.Join(dir, d.Migration), []byte(body), 0o644); err != nil {
			return nil, err
		}
	} else {
		path, err := appendMigration(dir, d, migration)
		if err != nil {
			return nil, err
		}
		d.Migration = path
	}
	written = append(written, d.Migration)

	queries := filepath.Join("internal", "db", "queries", d.Table+".sql")
	if err := renderAll(dir, map[string]string{"table/queries.sql.tmpl": queries}, d); err != nil {
		return nil, err
	}
	written = append(written, queries)
	if noStore {
		return written, nil
	}

	feat := filepath.Join("internal", "app", d.Feature)
	files := map[string]string{}
	if _, err := os.Stat(filepath.Join(dir, feat)); err != nil {
		files["feature.go.tmpl"] = filepath.Join(feat, d.Feature+".go")
		files["feature_errors.go.tmpl"] = filepath.Join(feat, "errors.go")
		if err := renderAll(dir, files, scaffoldData{Name: d.Feature}); err != nil {
			return nil, err
		}
		written = append(written, filepath.Join(feat, d.Feature+".go"), filepath.Join(feat, "errors.go"))
	}
	// A feature holds ONE Store. The first table brings the type and the
	// constructor; every table after it brings methods on that same Store,
	// named for the table they are about — planStore decided which this is,
	// before a byte was written.
	tmpl := "table/store.go.tmpl"
	if d.StoreIn != "" {
		tmpl = "table/store_methods.go.tmpl"
	}
	store := filepath.Join(feat, d.Singular+".go")
	if err := renderAll(dir, map[string]string{tmpl: store}, d); err != nil {
		return nil, err
	}
	written = append(written, store)

	// Remove distinguishes "was not there" from "failed", so the feature's
	// contract page needs the sentinel it returns.
	errsPath := filepath.Join(dir, feat, "errors.go")
	if b, err := os.ReadFile(errsPath); err == nil && !strings.Contains(string(b), "ErrNotFound") {
		body := strings.TrimRight(string(b), "\n") +
			fmt.Sprintf("\n\n// ErrNotFound is \"no such row, or not this caller's\" — Remove and Get\n"+
				"// cannot tell those apart, and neither should a client.\nvar ErrNotFound = errors.New(%q)\n", d.Feature+": not found")
		if !strings.Contains(string(b), `"errors"`) {
			body = strings.Replace(body, "\npackage "+d.Feature+"\n",
				"\npackage "+d.Feature+"\n\nimport \"errors\"\n", 1)
		}
		formatted, err := format.Source([]byte(body))
		if err != nil {
			return nil, fmt.Errorf("adding ErrNotFound to %s: %w", errsPath, err)
		}
		if err := os.WriteFile(errsPath, formatted, 0o644); err != nil {
			return nil, err
		}
		if rel := filepath.Join(feat, "errors.go"); !slices.Contains(written, rel) {
			written = append(written, rel)
		}
	}
	return written, nil
}

// nextMigration is the goose number after the highest one on disk.
func nextMigration(dir, name string) string {
	next := 1
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if m := migrationNumRe.FindStringSubmatch(e.Name()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	return fmt.Sprintf("%05d_%s.sql", next, name)
}

// appendMigration folds this table's DDL into an EXISTING migration, so a
// parent and its child arrive in one deploy step. The create goes last in Up
// and the drop goes FIRST in Down — a Down that dropped in creation order
// would trip over its own foreign keys.
func appendMigration(dir string, d *tableData, want string) (string, error) {
	migrations := filepath.Join(dir, "internal", "db", "migrations")
	entries, err := os.ReadDir(migrations)
	if err != nil {
		return "", err
	}
	var match string
	padded := want
	if n, err := strconv.Atoi(want); err == nil {
		padded = fmt.Sprintf("%05d", n)
	}
	for _, e := range entries {
		if e.Name() == want || strings.HasPrefix(e.Name(), padded+"_") {
			match = e.Name()
			break
		}
	}
	if match == "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return "", fmt.Errorf("no migration %q in %s — it holds %s", want, migrations, strings.Join(names, ", "))
	}
	path := filepath.Join(migrations, match)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	body := string(b)
	down := regexp.MustCompile(`(?im)^--\s*\+goose\s+down\s*$`).FindStringIndex(body)
	if down == nil {
		return "", fmt.Errorf("%s has no `-- +goose Down` section — append the DDL by hand", path)
	}
	up, err := renderTemplate("table/migration_body.sql.tmpl", d)
	if err != nil {
		return "", err
	}
	merged := strings.TrimRight(body[:down[0]], "\n") + "\n\n" + up +
		body[down[0]:down[1]] + "\n\ndrop table " + d.Table + ";\n" +
		strings.TrimLeft(body[down[1]:], "\n")
	if err := os.WriteFile(path, []byte(merged), 0o644); err != nil {
		return "", err
	}
	return filepath.Join("internal", "db", "migrations", match), nil
}

// runSQLC closes the loop: the queries are on disk, so the typed package can
// exist now. A missing sqlc is REPORTED, never fatal — the SQL is written and
// correct either way.
func runSQLC(dir string, out, errW io.Writer) bool {
	db := filepath.Join(dir, "internal", "db")
	if _, err := os.Stat(filepath.Join(db, "sqlc.yaml")); err != nil {
		return false
	}
	if _, err := exec.LookPath("sqlc"); err != nil {
		fmt.Fprintf(errW, `
sqlc is not on PATH, so internal/db/gen was NOT refreshed. Install it and run
the one command this scaffold stops short of:
  brew install sqlc        (or: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest)
  cd %s && sqlc generate
`, db)
		return false
	}
	cmd := exec.Command("sqlc", "generate")
	cmd.Dir = db
	b, err := cmd.CombinedOutput()
	if len(b) > 0 {
		fmt.Fprint(errW, string(b))
	}
	if err != nil {
		fmt.Fprintf(errW, "sqlc generate failed (%v) — the SQL is written; fix it and re-run `cd %s && sqlc generate`\n", err, db)
		return false
	}
	fmt.Fprintln(out, "sqlc generate     ok")
	return true
}

func printTableNextSteps(out io.Writer, d *tableData, noStore, generated bool) {
	if noStore {
		fmt.Fprintf(out, "\nNo store was written (--no-store). The typed querier is gen.%sError,\ngen.%sCursor and (*gen.Queries).List%sPage.\n",
			d.Export, d.Export, d.Export)
		return
	}
	if d.StoreIn != "" {
		fmt.Fprintf(out, "\n%s already has a Store (%s), so %s.go adds methods on it —\nCreate%s, %sPage and Remove%s. Both lines are already wired.\n",
			d.Feature, d.StoreIn, d.Singular, d.Model, d.Export, d.Model)
		if !generated {
			fmt.Fprintf(out, "\ninternal/db/gen is stale until `sqlc generate` runs, so they will not\ncompile yet.\n")
		}
		return
	}
	fmt.Fprintf(out, `
Two lines left:

  internal/app/%s/%s.go
	di.Provide(NewStore),

  internal/app/app.go
	%s.Use(),

`, d.Feature, d.Feature, d.Feature)
	fmt.Fprintf(out, "NewStore takes a *gen.DB — provide it once per product, beside pg.Use():\n\tdi.Provide(gen.NewDB),\n")
	if !generated {
		fmt.Fprintf(out, "\ninternal/db/gen is stale until `sqlc generate` runs, so those lines will\nnot compile yet.\n")
	}
}
