package main

// `ultra new <name> --from openapi.json` — REVERSE SCAFFOLDING.
//
// The forward scaffold writes an empty product; this writes a product SHAPED
// LIKE THE LEGACY SERVICE: one feature package per tag, one typed operation
// per path+method, request/response structs translated from the document's
// schemas, and every handler a 501 stub behind the problem envelope. The
// product BOOTS on arrival and every endpoint answers honestly; the migration
// is then filling stubs one at a time with the covenant green throughout.
//
// Three rules keep it trustworthy:
//
//   - Nothing is guessed silently. Every construct the translator cannot
//     honestly express is collected and printed in the migration report, with
//     the path that carries it.
//   - Names round-trip. An operationId that is a legal identifier is pinned
//     with api.Name so the regenerated openapi.json says what the source said;
//     anything else derives through the SAME vocabulary contrib/api uses
//     (verb from the method, noun from the path), so the two generators can
//     never disagree.
//   - The output is doctrine, not a dump: <pkg>.go + handler.go + errors.go
//     (+ types.go when there are component schemas), one line per feature in
//     app.go, `api.Handle` in Use().

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// fromPlan is everything --from adds on top of the normal tree.
type fromPlan struct {
	Source   string // the document, as the user spelled it
	Title    string
	Features []*genFeature // sorted by package name
	report   migrationReport
}

// PkgNames is the feature list app.go renders, sorted.
func (p *fromPlan) PkgNames() []string {
	out := make([]string, 0, len(p.Features))
	for _, f := range p.Features {
		out = append(out, f.Pkg)
	}
	return out
}

// genFeature is one generated feature package. The four Body fields are the
// rendered Go the templates splice in — composed here, in Go, because struct
// literals and comment placement read far better as code than as nested
// template ranges (and go/format owns the alignment either way).
type genFeature struct {
	Pkg      string // the Go package name
	Tag      string // the source tag (or path segment) it came from
	Source   string // the document, for the generated file header
	Doc      string // the one-line package doc
	Ops      []*genOp
	Types    []*genType
	Codes    []genCode
	Wiring   string // the api.Handle lines inside Use()
	Handlers string // handler.go: request/response structs + the stubs
	TypeDefs string // types.go: the component schemas this feature uses
	CodeDefs string // errors.go: the const block

	HandlerImports string // the extra std imports handler.go needs
	TypeImports    string // ...and types.go
	HasTypes       bool
}

// genOp is one translated operation.
type genOp struct {
	Dotted     string // the registry name: "depots.list"
	Sym        string // the client symbol / operationId: "listDepots"
	PinName    bool   // emit api.Name (the symbol is not what the deriver says)
	Method     string
	Path       string
	Summary    string
	Deprecated bool
	Require    string // "" → api.Public()
	Func       string // the handler function name
	ReqType    string
	RespType   string
	RespZero   string
	StubCode   string   // the not-implemented const name
	Codes      []string // every code the op declares with api.Codes
	Decls      []string // the request/response struct declarations
}

// genType is one component schema translated into a Go struct.
type genType struct {
	Name      string // the Go type name
	Component string // components.schemas.<Component>
	Decl      string
}

// genCode is one entry of the feature's error contract.
type genCode struct {
	Const string
	Value string
	Note  string
}

// planFrom reads the document and translates it. Pure: no filesystem writes,
// no network — so the whole reverse scaffold is testable without generating a
// product.
func planFrom(path string) (*fromPlan, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return nil, fmt.Errorf(`--from reads JSON only this round, and %s is YAML.
Convert it first, then pass the JSON:
    yq -o=json '.' %s > openapi.json     (or: python -c 'import sys,yaml,json; json.dump(yaml.safe_load(open(sys.argv[1])),sys.stdout)' %s > openapi.json)
    ultra new <name> --from openapi.json`, path, path, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the source document: %w", err)
	}
	spec, err := readSpec(filepath.Base(path), raw)
	if err != nil {
		return nil, err
	}
	return translate(filepath.Base(path), spec)
}

// ─────────────────────────── the translation ───────────────────────────

// bucket is one feature's operations before naming and type allocation.
type bucket struct {
	pkg  string
	tag  string
	ops  []*oaOp
	seen map[string]bool // component schemas this feature reaches
}

func translate(source string, spec *oaSpec) (*fromPlan, error) {
	plan := &fromPlan{Source: source, Title: spec.Title}
	rep := &plan.report
	rep.Source, rep.Title, rep.SpecVersion = source, spec.Title, spec.Version

	for _, name := range spec.Webhooks {
		rep.skip("webhook", name, "webhooks have no route to serve — port them as ordinary operations")
	}

	// 1. Bucket the operations by feature, collecting what cannot be carried.
	buckets := map[string]*bucket{}
	var order []string
	for _, item := range spec.Paths {
		for _, op := range item.Ops {
			if contains(unsupportedMethods, strings.ToLower(op.Method)) {
				rep.skip("method", op.Method+" "+op.Path, "HEAD/OPTIONS/TRACE carry no typed contract")
				continue
			}
			if reason := unsupportedPayload(spec, op); reason != "" {
				rep.skip("payload", op.Method+" "+op.Path, reason)
				continue
			}
			for _, cb := range op.Callbacks {
				rep.skip("callback", op.Method+" "+op.Path+" → "+cb,
					"the operation is scaffolded; the callback is not")
			}
			key := featureKey(op, item.Path)
			b := buckets[key]
			if b == nil {
				b = &bucket{pkg: key, tag: featureTag(op, item.Path), seen: map[string]bool{}}
				buckets[key] = b
				order = append(order, key)
			} else if t := featureTag(op, item.Path); t != b.tag {
				rep.mergedTag(b.tag, t, key)
			}
			// Path-item parameters apply to every operation under it.
			op.Params = append(append([]*oaParam{}, item.Params...), op.Params...)
			b.ops = append(b.ops, op)
		}
	}
	sort.Strings(order)

	// 2. Which component schemas does each feature reach? A component two
	//    features both use is generated in BOTH — package-qualified, exactly
	//    the way the api generators disambiguate — so a fresh migration
	//    starts with zero cross-feature edges (legal one direction under v3,
	//    but never a choice a generator should make for you).
	users := map[string][]string{} // component → feature packages
	for _, key := range order {
		b := buckets[key]
		for _, op := range b.ops {
			for _, s := range opSchemas(op) {
				reach(spec, s, b.seen)
			}
		}
		for name := range b.seen {
			users[name] = append(users[name], key)
		}
	}
	shared := map[string]bool{}
	for name, fs := range users {
		if len(fs) > 1 {
			shared[name] = true
			sort.Strings(fs)
			rep.Shared = append(rep.Shared, fmt.Sprintf("%s → %s", name, strings.Join(fs, ", ")))
		}
	}
	sort.Strings(rep.Shared)

	// 3. Names, app-wide: the symbol (operationId) and the dotted registry key
	//    are both unique across the whole product, so allocate them before any
	//    feature renders.
	symbols, dotted := newNameSet(), newNameSet()
	for _, key := range order {
		for _, op := range buckets[key].ops {
			nameOp(rep, symbols, dotted, buckets[key].pkg, op)
		}
	}

	// 4. Render each feature.
	for _, key := range order {
		f, err := buildFeature(spec, buckets[key], shared, source, rep)
		if err != nil {
			return nil, err
		}
		plan.Features = append(plan.Features, f)
	}
	rep.Features = plan.Features
	return plan, nil
}

// unsupportedPayload names why an operation cannot be translated, or "".
func unsupportedPayload(spec *oaSpec, op *oaOp) string {
	if p := op.Body; p != nil && !p.Empty {
		if !p.JSON {
			return "request body is " + strings.Join(p.Media, ", ") + " — not JSON"
		}
		if s := resolve(spec, p.Schema); s != nil && !s.isObject() && s.Compose == "" &&
			(s.primary() != "" && s.primary() != "object") {
			return "request body is a JSON " + s.primary() + ", and a bound request must be an object"
		}
	}
	if p := op.Resp; p != nil && !p.Empty && !p.JSON {
		return "the " + fmt.Sprint(op.RespStatus) + " response is " + strings.Join(p.Media, ", ") + " — not JSON"
	}
	return ""
}

// featureKey buckets an operation: its first tag, else the first path segment.
func featureKey(op *oaOp, path string) string {
	if len(op.Tags) > 0 {
		if k := pkgIdentOf(op.Tags[0]); k != "" {
			return k
		}
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || strings.HasPrefix(seg, "{") {
			continue
		}
		if k := pkgIdentOf(seg); k != "" {
			return k
		}
	}
	return "root"
}

func featureTag(op *oaOp, path string) string {
	if len(op.Tags) > 0 {
		return op.Tags[0]
	}
	for _, seg := range strings.Split(path, "/") {
		if seg != "" && !strings.HasPrefix(seg, "{") {
			return seg
		}
	}
	return "root"
}

// goReservedPkgs are the names a feature package may not take: Go keywords,
// and the identifiers the generated files already bind to imports.
var goReservedPkgs = map[string]bool{
	"api": true, "di": true, "app": true, "main": true, "context": true,
	"http": true, "json": true, "time": true, "internal": true, "db": true,
	"init": true, "test": true, "util": true,
}

// pkgIdentOf folds a tag or path segment into a Go package name: lowercase
// letters and digits, nothing else — the same reduction contrib/api applies to
// a group key, so a tag and its client module agree by construction.
func pkgIdentOf(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "f" + out
	}
	if goReservedPkgs[out] || goKeywords[out] {
		out += "svc"
	}
	return out
}

// ─────────────────────────── naming ───────────────────────────

// nameSet allocates unique identifiers, deterministically.
type nameSet struct{ taken map[string]bool }

func newNameSet() *nameSet { return &nameSet{taken: map[string]bool{}} }

// take returns want, or the first want2, want3, ... that is free.
func (n *nameSet) take(want string) (string, bool) {
	if !n.taken[want] {
		n.taken[want] = true
		return want, false
	}
	for i := 2; ; i++ {
		alt := fmt.Sprintf("%s%d", want, i)
		if !n.taken[alt] {
			n.taken[alt] = true
			return alt, true
		}
	}
}

// nameOp settles an operation's two names. The SYMBOL is the operationId when
// the document gave a legal one — that is what makes the regenerated
// openapi.json say what the source said — and otherwise it is derived from
// method+path through contrib/api's vocabulary. The dotted registry key is
// always <feature>.<verb>.
func nameOp(rep *migrationReport, symbols, dotted *nameSet, pkg string, op *oaOp) {
	derived := deriveOpName(op.Method, op.Path)
	want := derived
	switch {
	case op.OperationID == "":
		rep.Derived = append(rep.Derived, fmt.Sprintf("%s %s → %s (no operationId)", op.Method, op.Path, derived))
	case legalOpName(op.OperationID):
		want = op.OperationID
	default:
		rep.Derived = append(rep.Derived, fmt.Sprintf("%s %s → %s (operationId %q is not an identifier)",
			op.Method, op.Path, derived, op.OperationID))
	}
	sym, renamed := symbols.take(want)
	if renamed {
		rep.Collisions = append(rep.Collisions,
			fmt.Sprintf("%s %s: %s was taken → api.Name(%q)", op.Method, op.Path, want, sym))
	}
	verb, _ := verbNounOf(op.Method, segmentsOf(op.Path))
	key, _ := dotted.take(pkg + "." + verb)

	op.plannedSym, op.plannedDotted = sym, key
	op.plannedPin = sym != derived
}

// goKeywords is the Go reserved-word list; a generated identifier is never one.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// goBuiltins are the predeclared identifiers a handler must not shadow — the
// doctrine's "never name a handler delete" rule, mechanized.
var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// tsReserved mirrors contrib/api's list: the symbol becomes a TypeScript
// function and type name too, so a name illegal there is illegal here.
var tsReserved = map[string]bool{
	"await": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "debugger": true, "default": true,
	"delete": true, "do": true, "else": true, "enum": true, "export": true,
	"extends": true, "false": true, "finally": true, "for": true,
	"function": true, "if": true, "implements": true, "import": true,
	"in": true, "instanceof": true, "interface": true, "let": true,
	"new": true, "null": true, "package": true, "private": true,
	"protected": true, "public": true, "return": true, "static": true,
	"super": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "var": true, "void": true, "while": true,
	"with": true, "yield": true,
}

// legalOpName is contrib/api's checkNames rule INTERSECTED with Go's: the
// symbol is written as an operationId, a TypeScript function, and (upper-cased)
// a Go type name, so it must be spellable in all three. `$` is legal in
// JavaScript and not in Go, which is exactly why the intersection matters.
func legalOpName(s string) bool {
	if s == "" || tsReserved[s] || goKeywords[s] {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// deriveOpName mirrors contrib/api's deriveName for the no-operationId case:
// a verb from the method, a noun from the path.
func deriveOpName(method, path string) string {
	verb, resource := verbNounOf(method, segmentsOf(path))
	return verb + camelOf(resource)
}

func verbNounOf(method string, segs []string) (verb, resource string) {
	lastIsParam := len(segs) > 0 && isParamSeg(segs[len(segs)-1])
	switch strings.ToUpper(method) {
	case "GET":
		if lastIsParam {
			return "get", singularOf(resourceSegOf(segs, false))
		}
		return "list", resourceSegOf(segs, false)
	case "PUT":
		return "replace", singularOf(resourceSegOf(segs, false))
	case "PATCH":
		return "update", singularOf(resourceSegOf(segs, false))
	case "DELETE":
		return "delete", singularOf(resourceSegOf(segs, false))
	case "POST":
		if !lastIsParam && trailingActionOf(segs) {
			return lowerFirstOf(camelOf(segs[len(segs)-1])), singularOf(resourceSegOf(segs, true))
		}
		return "create", singularOf(resourceSegOf(segs, false))
	default:
		return strings.ToLower(method), singularOf(resourceSegOf(segs, false))
	}
}

func resourceSegOf(segs []string, skipAction bool) string {
	end := len(segs)
	if skipAction && end > 0 && !isParamSeg(segs[end-1]) {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if !isParamSeg(segs[i]) {
			return segs[i]
		}
	}
	return "resource"
}

func trailingActionOf(segs []string) bool {
	if len(segs) == 0 || isParamSeg(segs[len(segs)-1]) {
		return false
	}
	for _, s := range segs[:len(segs)-1] {
		if isParamSeg(s) {
			return true
		}
	}
	return false
}

func segmentsOf(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func isParamSeg(s string) bool { return strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") }

func singularOf(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"),
		strings.HasSuffix(s, "zes"), strings.HasSuffix(s, "ches"), strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") && len(s) > 1:
		return s[:len(s)-1]
	}
	return s
}

// camelOf TitleCases an arbitrary label into a Go identifier fragment.
// Anything that cannot appear in a Go identifier — including the underscore,
// so `not_found` becomes NotFound rather than Not_found — is a word separator
// and never appears in the output.
func camelOf(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if upper {
				b.WriteString(strings.ToUpper(string(r)))
				upper = false
			} else {
				b.WriteRune(r)
			}
		default:
			upper = true
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "N" + out
	}
	return out
}

// fieldName is camelOf for a wire name, with the one initialism that shows up
// in every document: `id` is ID, and so is the tail of `depotId`. Go's own
// convention, and the difference between generated code you keep and generated
// code you rewrite by hand on day one.
func fieldName(s string) string {
	out := camelOf(s)
	switch {
	case out == "Id":
		return "ID"
	case strings.HasSuffix(out, "Id") && len(out) > 2:
		return out[:len(out)-2] + "ID"
	}
	return out
}

func lowerFirstOf(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func upperFirstOf(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
