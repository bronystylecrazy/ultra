package main

// `ultra skill gen` regenerates the reference docs that are build artifacts,
// so hand-written API prose can never drift from the code again.
//
// Two things are generated:
//
//   references/errors.md   — FULLY generated from the diag registry + lessons
//                            (diag.AllCodes + diag.Lesson): one entry per code.
//
//   marker blocks in any references/**.md — the span between a start marker
//   and <!-- ultra:gen end --> is rewritten; prose outside markers is never
//   touched. Three marker kinds:
//
//     <!-- ultra:gen doc <pkg> <Symbol> -->      go doc <pkg> <Symbol>
//     <!-- ultra:gen example <file> <Func> -->   the body of a compiled
//                                                 Example* function
//     <!-- ultra:gen snip <file> <name> -->      the region of a compiled
//                                                 file between `// ultra:snip
//                                                 <name>` and `// ultra:snip
//                                                 end`
//     <!-- ultra:gen codes <dir> -->             the table of diagnostic codes
//                                                 that package registers with
//                                                 diag.Register, read from its
//                                                 source
//
//   example is for a sequence of statements; snip is for whole DECLARATIONS —
//   a constructor's parameter list, a feature's Use(), a product's assembly —
//   where the signature IS the lesson and a function body would drop it.
//
// Generation is idempotent: running gen twice produces no diff. The drift
// gate (skillgen_test.go) regenerates into memory and fails the build when
// disk disagrees — so staleness is a test failure, not a review miss.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bronystylecrazy/ultrastack/di/diag"
)

// genOpenRe matches a block's opening marker: kind, then one or two
// space-separated arguments (package+symbol, file+function/snip name — or,
// for codes, just the preset's directory).
var genOpenRe = regexp.MustCompile(`^<!--\s*ultra:gen\s+(doc|example|snip|codes)\s+(\S+)(?:\s+(\S+))?\s*-->$`)

const genEndMarker = "<!-- ultra:gen end -->"

// cmdSkill implements `ultra skill <sub>`; only `gen` exists today.
func cmdSkill(args []string, out, errW io.Writer) int {
	if len(args) == 0 || args[0] != "gen" {
		fmt.Fprintln(errW, "usage: ultra skill gen")
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "references")); err != nil {
		fmt.Fprintln(errW, "ultra skill gen: run from the repo root (no references/ directory here)")
		return 1
	}
	files, err := skillGen(root)
	if err != nil {
		fmt.Fprintln(errW, "ultra skill gen:", err)
		return 1
	}
	changed := 0
	for rel, want := range files {
		path := filepath.Join(root, rel)
		if got, err := os.ReadFile(path); err == nil && string(got) == want {
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			fmt.Fprintln(errW, err)
			return 1
		}
		fmt.Fprintln(out, "wrote", rel)
		changed++
	}
	if changed == 0 {
		fmt.Fprintln(out, "references are up to date")
	}
	return 0
}

// skillGen computes the desired on-disk content of every generated reference
// file, keyed by path relative to root. It is the single source of truth
// shared by the `gen` command and the drift test — neither writes without
// going through here. It reads the current markdown (to preserve prose
// outside markers) and execs `go doc` / parses example files.
func skillGen(root string) (map[string]string, error) {
	out := map[string]string{}

	// errors.md is generated whole from the diagnostic registry.
	out[filepath.Join("references", "errors.md")] = generateErrorsDoc()

	// Every other reference file may carry marker blocks; rewrite them.
	refs := filepath.Join(root, "references")
	err := filepath.WalkDir(refs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("references", "errors.md") {
			return nil // fully generated above
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), "<!-- ultra:gen ") {
			return nil // no markers, nothing to do
		}
		rendered, err := renderMarkers(root, string(src))
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		out[rel] = rendered
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// renderMarkers rewrites every `ultra:gen` block in content, leaving all
// other lines byte-for-byte unchanged. The replacement is a fenced code
// block, which by construction contains no marker lines — so a second pass
// reproduces it exactly (idempotence).
func renderMarkers(root, content string) (string, error) {
	lines := strings.Split(content, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := genOpenRe.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		// Find the matching end marker.
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != genEndMarker {
			j++
		}
		if j >= len(lines) {
			return "", fmt.Errorf("unterminated `ultra:gen %s %s %s` block", m[1], m[2], m[3])
		}
		block, err := generateBlock(root, m[1], m[2], m[3])
		if err != nil {
			return "", err
		}
		out = append(out, block...)
		out = append(out, lines[j]) // re-emit the end marker
		i = j
	}
	return strings.Join(out, "\n"), nil
}

// generateBlock produces the fenced lines that replace a block's body.
func generateBlock(root, kind, arg1, arg2 string) ([]string, error) {
	if kind != "codes" && arg2 == "" {
		return nil, fmt.Errorf("ultra:gen %s %s: this kind needs two arguments", kind, arg1)
	}
	switch kind {
	case "doc":
		body, err := goDoc(root, arg1, arg2)
		if err != nil {
			return nil, err
		}
		return fence("text", body), nil
	case "example":
		body, err := exampleBody(root, arg1, arg2)
		if err != nil {
			return nil, err
		}
		return fence("go", body), nil
	case "snip":
		body, err := snipBody(root, arg1, arg2)
		if err != nil {
			return nil, err
		}
		return fence("go", body), nil
	case "codes":
		return presetCodesTable(root, arg1)
	default:
		return nil, fmt.Errorf("unknown ultra:gen kind %q", kind)
	}
}

// fence wraps body in a ```lang code fence, one entry per line.
func fence(lang, body string) []string {
	out := []string{"```" + lang}
	out = append(out, strings.Split(body, "\n")...)
	out = append(out, "```")
	return out
}

// goDoc runs `go doc <pkg> <symbol>` and returns its output, minus the
// leading `package … // import …` header line (noise repeated per block).
// pkg names a package directory like "./di" or "./inference"; contrib
// packages resolve automatically — root module first, then contrib/.
func goDoc(root, pkg, symbol string) (string, error) {
	name := strings.TrimPrefix(pkg, "./")
	dir := root
	if st, err := os.Stat(filepath.Join(root, name)); err != nil || !st.IsDir() {
		if st2, err2 := os.Stat(filepath.Join(root, "contrib", name)); err2 == nil && st2.IsDir() {
			dir = filepath.Join(root, "contrib")
		}
	}
	cmd := exec.Command("go", "doc", "./"+name, symbol)
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go doc %s %s: %v\n%s", pkg, symbol, err, raw)
	}
	return stripPackageHeader(strings.TrimRight(string(raw), "\n \t")), nil
}

// stripPackageHeader drops a leading `package X // import "..."` line and the
// blank line after it, so doc blocks start at the declaration.
func stripPackageHeader(doc string) string {
	lines := strings.Split(doc, "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "package ") {
		lines = lines[1:]
		if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}

// exampleBody returns the source between the braces of the named function in
// file (relative to root), dedented one tab. This lets a reference show a
// snippet that is guaranteed to compile, because it IS a compiled Example.
func exampleBody(root, file, fn string) (string, error) {
	path := filepath.Join(root, file)
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return "", err
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		start := fset.Position(fd.Body.Lbrace).Offset + 1
		end := fset.Position(fd.Body.Rbrace).Offset
		body := strings.Trim(string(src[start:end]), "\n")
		out := strings.Split(body, "\n")
		for i, l := range out {
			out[i] = strings.TrimPrefix(l, "\t")
		}
		return strings.Join(out, "\n"), nil
	}
	return "", fmt.Errorf("%s: no function %s with a body", file, fn)
}

// snipBody returns the lines of file (relative to root) between the sentinels
// `// ultra:snip <name>` and `// ultra:snip end`, dedented by the tab depth
// they share. Where an Example's body is the right unit for a sequence of
// statements, a snip is the right unit for whole declarations: a constructor's
// parameter list, a feature's `func Use() di.Reg`, a product's `var App` — the
// snippets whose SIGNATURE is what the doc teaches. The region is ordinary
// compiled code in an ordinary package, so it cannot drift either.
func snipBody(root, file, name string) (string, error) {
	src, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return "", err
	}
	open := "// ultra:snip " + name
	lines := strings.Split(string(src), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != open {
			continue
		}
		if start >= 0 {
			return "", fmt.Errorf("%s: snip %q opened twice", file, name)
		}
		start = i + 1
	}
	if start < 0 {
		return "", fmt.Errorf("%s: no `%s` sentinel", file, open)
	}
	end := start
	for end < len(lines) && strings.TrimSpace(lines[end]) != "// ultra:snip end" {
		if strings.HasPrefix(strings.TrimSpace(lines[end]), "// ultra:snip ") {
			return "", fmt.Errorf("%s: snip %q is not closed before the next one", file, name)
		}
		end++
	}
	if end >= len(lines) {
		return "", fmt.Errorf("%s: snip %q has no `// ultra:snip end`", file, name)
	}
	return dedent(trimBlank(lines[start:end])), nil
}

// trimBlank drops leading and trailing blank lines from a block.
func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// dedent removes the tab indentation every non-blank line shares, so a snip
// taken from inside a function reads flush-left in the doc.
func dedent(lines []string) string {
	depth := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, "\t"))
		if depth < 0 || n < depth {
			depth = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if depth > 0 && len(l) >= depth {
			l = l[depth:]
		}
		out[i] = l
	}
	return strings.Join(out, "\n")
}

// presetCodesTable renders a preset's own diagnostic codes — the PGxxxx of
// contrib/pg — as a table, read from the package's SOURCE rather than from a
// registry.
//
// It has to be source, not imports: the preset codes live in the contrib
// module, `ultra` links no preset (so `ultra explain PG0101` is deliberately a
// miss), and a companion binary that imported every preset to document them
// would be exactly the coupling presets exist to avoid. What the generator
// reads instead is the one call that defines the fact — diag.Register(code,
// lesson) — plus the string constants in the same package, so a code that is
// declared but never registered simply is not in the table.
func presetCodesTable(root, dir string) ([]string, error) {
	path := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("ultra:gen codes %s: %w", dir, err)
	}
	fset := token.NewFileSet()
	consts := map[string]string{}
	var registers [][2]ast.Expr
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(path, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("ultra:gen codes %s: %w", dir, err)
		}
		collectStringConsts(f, consts)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Register" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "diag" {
				return true
			}
			registers = append(registers, [2]ast.Expr{call.Args[0], call.Args[1]})
			return true
		})
	}
	if len(registers) == 0 {
		return nil, fmt.Errorf("ultra:gen codes %s: no diag.Register calls found", dir)
	}

	type row struct{ code, summary string }
	rows := make([]row, 0, len(registers))
	for _, r := range registers {
		code, ok := resolveString(r[0], consts)
		if !ok {
			return nil, fmt.Errorf("ultra:gen codes %s: cannot resolve the code in a diag.Register call "+
				"(use a string constant declared in the package)", dir)
		}
		lesson, ok := resolveString(r[1], consts)
		if !ok {
			return nil, fmt.Errorf("ultra:gen codes %s: cannot resolve the lesson for %s "+
				"(use a string constant declared in the package)", dir, code)
		}
		title, _ := lessonHead(lesson)
		rows = append(rows, row{code, strings.TrimSpace(strings.TrimPrefix(title, code+" —"))})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].code < rows[j].code })

	out := []string{"| Code | Meaning |", "|------|---------|"}
	for _, r := range rows {
		out = append(out, fmt.Sprintf("| `%s` | %s |", r.code, r.summary))
	}
	return out, nil
}

// collectStringConsts records every package-level `const|var NAME = "..."` so
// a Register call written with named constants (which is how a preset should
// write it) resolves.
func collectStringConsts(f *ast.File, into map[string]string) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if s, ok := stringLit(vs.Values[i]); ok {
					into[name.Name] = s
				}
			}
		}
	}
}

// resolveString reads a string out of an expression that is either a literal,
// a named constant, or a conversion like diag.Code("PG0101").
func resolveString(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.CallExpr:
		if len(v.Args) == 1 {
			return resolveString(v.Args[0], consts)
		}
	}
	return stringLit(e)
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// generateErrorsDoc renders references/errors.md entirely from the diagnostic
// registry: intro + one entry per code (title line from the lesson, then its
// first paragraph as the condensed summary). `ultra explain <code>` remains
// the full lesson — this file is the scannable index.
func generateErrorsDoc() string {
	var b strings.Builder
	b.WriteString(`<!-- GENERATED by ` + "`ultra skill gen`" + ` — do not edit; edit di/diag/lessons.go -->

# Diagnostic codes

Every failure is a ` + "`*diag.Diag`" + ` that names what, where, why, and the fix.
This is the index: code, title, and the one-paragraph summary. For the worked
fix behind any code run ` + "`ultra explain <code>`" + ` (or the mcp ` + "`explain`" + ` tool);
in-process the same text is ` + "`diag.Lesson(code)`" + `.

The prefix tells the domain: ` + "`DI00xx`" + ` graph shape, ` + "`DI01xx`" + ` scopes &
families, ` + "`DI02xx`" + ` lifecycle, ` + "`DI03xx`" + ` lint warnings, and ` + "`UVxxxx`" + ` the
static analyzer's own lints (` + "`ultra vet`" + ` / ` + "`go vet`" + `) — source facts the graph
cannot see: what a constructor does, what a file is named, what a package
imports. Warnings (severity "warning") never fail Validate or block boot —
` + "`di.Warnings`" + ` computes them and ` + "`serve`" + ` prints them at startup; promote codes
to hard errors with ` + "`ULTRA_WERROR=DI0301,DI0302`" + ` (or ` + "`all`" + `).

**Preset codes are not listed here.** A preset owns its own codes —
` + "`PG0101`" + `, ` + "`PG0201`" + `, prefix = the package name uppercased, ` + "`01xx`" + ` runtime and
` + "`02xx`" + ` config — and registers them at init in the binary that wires it. This
file is generated from the registry a preset-free binary can see, so each
preset's codes live in ITS reference (` + "`references/presets/pg.md`" + `), and the
lesson is ` + "`./app explain PG0101`" + ` rather than ` + "`ultra explain`" + `. A DI0202 whose
cause carries one nests the preset's help and points at it. See DIAGNOSTICS.md.
`)
	for _, c := range diag.AllCodes() {
		lesson, ok := diag.Lesson(c)
		if !ok {
			continue
		}
		title, summary := lessonHead(lesson)
		b.WriteString("\n## " + title + "\n\n")
		b.WriteString(summary + "\n")
	}
	return b.String()
}

// lessonHead splits a lesson into its title line (e.g. "DI0001 — no provider
// for a required type") and its first paragraph (the condensed summary).
func lessonHead(lesson string) (title, summary string) {
	lines := strings.Split(lesson, "\n")
	title = strings.TrimSpace(lines[0])
	// Skip the blank line after the title, then collect until the next blank.
	i := 1
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	var para []string
	for ; i < len(lines) && strings.TrimSpace(lines[i]) != ""; i++ {
		para = append(para, lines[i])
	}
	return title, strings.Join(para, "\n")
}
