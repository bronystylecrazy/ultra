package main

// Constraints, read from the migrations.
//
// sqlc's CodeGenRequest carries the catalog's TABLES and COLUMNS and stops
// there: plugin/codegen.proto has no Constraint message at all, so a plugin
// cannot learn from the request that `notes_subject_key` exists — and the
// constraint NAME is the only thing pgconn.PgError gives a caller to branch
// on. The migrations sqlc just parsed are on disk beside us (Settings.Schema,
// relative to the sqlc.yaml directory sqlc runs the plugin in), so they are
// read again here for the two facts the proto drops: UNIQUE constraints and
// foreign keys.
//
// This is a scanner, not a SQL parser. It recognizes the DDL a migration
// actually contains and ignores everything else — a constraint it cannot see
// costs a sentinel, never a wrong one, because every name it emits is a name
// it read verbatim (or Postgres's own <table>_<cols>_key rule for the
// unnamed form).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type uniqueFact struct {
	name string
	cols []string
}

type schemaFacts struct {
	uniques map[string][]uniqueFact      // table -> its unique constraints
	fks     map[string]map[string]string // table -> column -> parent table
}

var (
	commentRe    = regexp.MustCompile(`(?m)--[^\n]*`)
	blockRe      = regexp.MustCompile(`(?s)/\*.*?\*/`)
	createTblRe  = regexp.MustCompile(`(?is)create\s+table\s+(?:if\s+not\s+exists\s+)?"?([a-z_][a-z0-9_]*)"?\s*\(`)
	uniqueIdxRe  = regexp.MustCompile(`(?is)create\s+unique\s+index\s+(?:concurrently\s+)?(?:if\s+not\s+exists\s+)?"?([a-z_][a-z0-9_]*)"?\s+on\s+"?([a-z_][a-z0-9_]*)"?\s*\(([^)]*)\)`)
	alterUniqRe  = regexp.MustCompile(`(?is)alter\s+table\s+(?:only\s+)?"?([a-z_][a-z0-9_]*)"?\s+add\s+constraint\s+"?([a-z_][a-z0-9_]*)"?\s+unique\s*\(([^)]*)\)`)
	namedUniqRe  = regexp.MustCompile(`(?is)^constraint\s+"?([a-z_][a-z0-9_]*)"?\s+unique\s*\(([^)]*)\)`)
	tableUniqRe  = regexp.MustCompile(`(?is)^unique\s*\(([^)]*)\)`)
	referencesRe = regexp.MustCompile(`(?is)references\s+"?([a-z_][a-z0-9_]*)"?`)
	fkClauseRe   = regexp.MustCompile(`(?is)^(?:constraint\s+"?[a-z_][a-z0-9_]*"?\s+)?foreign\s+key\s*\(([^)]*)\)\s*references\s+"?([a-z_][a-z0-9_]*)"?`)
	identOnlyRe  = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// readSchemaFacts scans every .sql file under the configured schema paths.
func readSchemaFacts(dir string, paths []string) schemaFacts {
	facts := schemaFacts{uniques: map[string][]uniqueFact{}, fks: map[string]map[string]string{}}
	for _, p := range paths {
		root := filepath.Join(dir, p)
		filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".sql") {
				return nil
			}
			if b, err := os.ReadFile(path); err == nil {
				scanDDL(string(b), &facts)
			}
			return nil
		})
	}
	return facts
}

func scanDDL(sql string, facts *schemaFacts) {
	sql = commentRe.ReplaceAllString(blockRe.ReplaceAllString(sql, " "), " ")
	// Goose's Down section drops what Up created; scanning it would attribute
	// nothing, but skipping it keeps a `drop constraint` from ever confusing
	// a future rule.
	if i := regexp.MustCompile(`(?i)--\s*\+goose\s+down`).FindStringIndex(sql); i != nil {
		sql = sql[:i[0]]
	}
	for _, m := range uniqueIdxRe.FindAllStringSubmatch(sql, -1) {
		if cols := columnList(m[3]); cols != nil {
			facts.uniques[m[2]] = append(facts.uniques[m[2]], uniqueFact{m[1], cols})
		}
	}
	for _, m := range alterUniqRe.FindAllStringSubmatch(sql, -1) {
		if cols := columnList(m[3]); cols != nil {
			facts.uniques[m[1]] = append(facts.uniques[m[1]], uniqueFact{m[2], cols})
		}
	}
	for _, loc := range createTblRe.FindAllStringSubmatchIndex(sql, -1) {
		table := sql[loc[2]:loc[3]]
		body, ok := balanced(sql[loc[1]-1:])
		if !ok {
			continue
		}
		for _, item := range splitTopLevel(body) {
			scanTableItem(table, item, facts)
		}
	}
}

func scanTableItem(table, item string, facts *schemaFacts) {
	item = strings.TrimSpace(item)
	if m := namedUniqRe.FindStringSubmatch(item); m != nil {
		if cols := columnList(m[2]); cols != nil {
			facts.uniques[table] = append(facts.uniques[table], uniqueFact{m[1], cols})
		}
		return
	}
	if m := tableUniqRe.FindStringSubmatch(item); m != nil {
		if cols := columnList(m[1]); cols != nil {
			facts.uniques[table] = append(facts.uniques[table], uniqueFact{impliedName(table, cols), cols})
		}
		return
	}
	if m := fkClauseRe.FindStringSubmatch(item); m != nil {
		if cols := columnList(m[1]); len(cols) == 1 {
			addFK(facts, table, cols[0], m[2])
		}
		return
	}
	// A column definition: the first word is the column, and UNIQUE or
	// REFERENCES may ride the rest of the line.
	fields := strings.Fields(item)
	if len(fields) < 2 || !identOnlyRe.MatchString(strings.Trim(fields[0], `"`)) {
		return
	}
	col := strings.Trim(fields[0], `"`)
	switch strings.ToLower(col) {
	case "primary", "constraint", "unique", "foreign", "check", "exclude", "like":
		return
	}
	rest := strings.ToLower(strings.Join(fields[1:], " "))
	if regexp.MustCompile(`(^|\s)unique(\s|$)`).MatchString(rest) {
		facts.uniques[table] = append(facts.uniques[table],
			uniqueFact{impliedName(table, []string{col}), []string{col}})
	}
	if m := referencesRe.FindStringSubmatch(rest); m != nil {
		addFK(facts, table, col, m[1])
	}
}

func addFK(facts *schemaFacts, table, col, parent string) {
	if facts.fks[table] == nil {
		facts.fks[table] = map[string]string{}
	}
	facts.fks[table][col] = parent
}

// impliedName is Postgres's own name for an unnamed unique constraint:
// <table>_<col>_..._key, truncated to the 63-byte identifier limit.
func impliedName(table string, cols []string) string {
	name := table + "_" + strings.Join(cols, "_") + "_key"
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func columnList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		// Drop DESC/ASC/NULLS LAST and quoting; an expression index is not a
		// column list and disqualifies the whole constraint.
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) == 0 {
			return nil
		}
		c := strings.Trim(f[0], `"`)
		if !identOnlyRe.MatchString(c) {
			return nil
		}
		out = append(out, c)
	}
	return out
}

// balanced returns the contents of the parenthesized group s starts with.
func balanced(s string) (string, bool) {
	depth, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			if depth--; depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits a CREATE TABLE body on the commas that separate its
// items — the ones outside parentheses, so numeric(10,2) stays one item.
func splitTopLevel(s string) []string {
	var out []string
	depth, start, quote := 0, 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
