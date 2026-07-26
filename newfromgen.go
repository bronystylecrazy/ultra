package main

// Schema → Go, and the feature package that comes out of it.
//
// The translation table lives in goType below and is deliberately small: the
// shapes a bound request and a JSON response can actually express. Everything
// else parks as json.RawMessage under a TODO that NAMES what it was, and shows
// up in the migration report — a wrong Go type that compiles is worse than an
// honest placeholder, because the first one gets shipped.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// featureGen renders one feature package.
type featureGen struct {
	spec   *oaSpec
	pkg    string
	names  *nameSet          // package-scope identifiers
	tname  map[string]string // component → Go struct name (objects only)
	alias  map[string]string // component → inline Go type (non-objects)
	busy   map[string]bool   // component names being resolved (cycle guard)
	rep    *migrationReport
	file   string // "types" or "handler": which file's imports to grow
	needJS map[string]bool
	needTm map[string]bool
}

func (g *featureGen) needJSON() { g.needJS[g.file] = true }
func (g *featureGen) needTime() { g.needTm[g.file] = true }

// ─────────────────────── schema reachability ───────────────────────

// opSchemas is every schema one operation touches.
func opSchemas(op *oaOp) []*oaSchema {
	var out []*oaSchema
	for _, p := range op.Params {
		if p.Schema != nil {
			out = append(out, p.Schema)
		}
	}
	if op.Body != nil && op.Body.Schema != nil {
		out = append(out, op.Body.Schema)
	}
	if op.Resp != nil && op.Resp.Schema != nil {
		out = append(out, op.Resp.Schema)
	}
	return out
}

// reach collects the component schemas a schema depends on, transitively.
func reach(spec *oaSpec, s *oaSchema, into map[string]bool) {
	if s == nil {
		return
	}
	if s.Ref != "" {
		if into[s.Ref] {
			return
		}
		if target, ok := spec.Schemas[s.Ref]; ok {
			into[s.Ref] = true
			reach(spec, target, into)
		}
		return
	}
	reach(spec, s.Items, into)
	reach(spec, s.AddProps, into)
	for _, k := range s.PropKeys {
		reach(spec, s.Props[k], into)
	}
	for _, m := range s.Members {
		reach(spec, m, into)
	}
}

// resolve follows a $ref and merges a mergeable allOf, so callers can ask
// "what shape is this, really" without repeating either walk.
func resolve(spec *oaSpec, s *oaSchema) *oaSchema {
	return resolveAt(spec, s, 0)
}

func resolveAt(spec *oaSpec, s *oaSchema, depth int) *oaSchema {
	if s == nil || depth > 8 {
		return s
	}
	if s.Ref != "" {
		if t, ok := spec.Schemas[s.Ref]; ok {
			return resolveAt(spec, t, depth+1)
		}
		return s
	}
	if s.Compose == "allOf" {
		if merged := mergeAllOf(spec, s, depth); merged != nil {
			return merged
		}
	}
	return s
}

// mergeAllOf folds an allOf of objects (the inheritance idiom) into one object
// schema. A member that is not an object makes the whole composition
// untranslatable, and nil says so — the caller parks it with a TODO.
func mergeAllOf(spec *oaSpec, s *oaSchema, depth int) *oaSchema {
	if depth > 8 || len(s.Members) == 0 {
		return nil
	}
	out := &oaSchema{Props: map[string]*oaSchema{}, Description: s.Description}
	for _, m := range s.Members {
		r := resolveAt(spec, m, depth+1)
		if r == nil || !r.isObject() {
			return nil
		}
		for _, k := range r.PropKeys {
			if _, dup := out.Props[k]; dup {
				continue
			}
			out.PropKeys = append(out.PropKeys, k)
			out.Props[k] = r.Props[k]
		}
		out.Required = append(out.Required, r.Required...)
	}
	if len(out.PropKeys) == 0 {
		return nil
	}
	out.Types = []string{"object"}
	return out
}

// ─────────────────────── the type translation ───────────────────────

// goType renders a Go type expression for a schema, plus the TODO line the
// field carries when the translation is a placeholder rather than a type.
func (g *featureGen) goType(s *oaSchema) (typ, todo string) {
	if s == nil {
		g.needJSON()
		return "json.RawMessage", "TODO(migration): the source document gave no schema here — give it a type."
	}
	if s.Ref != "" {
		if t := g.refType(s.Ref); t != "" {
			return g.nilable(s, t), ""
		}
		g.needJSON()
		return "json.RawMessage", fmt.Sprintf("TODO(migration): $ref %q resolves to nothing in this document.", s.Ref)
	}
	if s.Compose != "" {
		if merged := mergeAllOf(g.spec, s, 0); merged != nil {
			return g.nilable(s, g.inlineStruct(merged)), ""
		}
		g.needJSON()
		return "json.RawMessage", fmt.Sprintf(
			"TODO(migration): %s composition in the source document — pick the real type (the raw JSON is preserved meanwhile).", s.Compose)
	}
	switch s.primary() {
	case "object":
		return g.objectType(s)
	case "array":
		el, todo := g.goType(s.Items)
		return "[]" + el, todo
	case "string":
		if s.Format == "date-time" {
			g.needTime()
			return g.nilable(s, "time.Time"), ""
		}
		return g.nilable(s, "string"), ""
	case "integer":
		switch s.Format {
		case "int64":
			return g.nilable(s, "int64"), ""
		case "int32":
			return g.nilable(s, "int32"), ""
		}
		return g.nilable(s, "int"), ""
	case "number":
		if s.Format == "float" {
			return g.nilable(s, "float32"), ""
		}
		return g.nilable(s, "float64"), ""
	case "boolean":
		return g.nilable(s, "bool"), ""
	case "":
		// Untyped: 3.1 lets `properties` stand alone, and plenty of hand-written
		// 3.0 does the same.
		if s.isObject() || s.AddProps != nil || s.AddPropsAny {
			return g.objectType(s)
		}
		g.needJSON()
		return "json.RawMessage", "TODO(migration): the source schema declares no type — give it one."
	default:
		g.needJSON()
		return "json.RawMessage", fmt.Sprintf("TODO(migration): unsupported schema type %q.", s.primary())
	}
}

func (g *featureGen) objectType(s *oaSchema) (string, string) {
	switch {
	case s.isObject():
		return g.nilable(s, g.inlineStruct(s)), ""
	case s.AddProps != nil:
		el, todo := g.goType(s.AddProps)
		return "map[string]" + el, todo
	default:
		return "map[string]any", ""
	}
}

// nilable pointerizes a nullable scalar or struct. Slices and maps already
// carry nil, so they are left alone — a **[]T is noise, not information.
func (g *featureGen) nilable(s *oaSchema, t string) string {
	if !s.Nullable || strings.HasPrefix(t, "[]") || strings.HasPrefix(t, "map[") ||
		strings.HasPrefix(t, "*") || t == "json.RawMessage" || t == "any" {
		return t
	}
	return "*" + t
}

// refType is the Go type a $ref resolves to: the named struct for an object
// component, the inline expression for anything else.
func (g *featureGen) refType(name string) string {
	if t, ok := g.tname[name]; ok {
		return t
	}
	if t, ok := g.alias[name]; ok {
		return t
	}
	s, ok := g.spec.Schemas[name]
	if !ok {
		return ""
	}
	if g.busy[name] { // a non-object cycle: park it rather than recurse forever
		g.needJSON()
		return "json.RawMessage"
	}
	g.busy[name] = true
	t, _ := g.goType(s)
	delete(g.busy, name)
	g.alias[name] = t
	return t
}

// inlineStruct renders an anonymous struct for an inline object schema.
func (g *featureGen) inlineStruct(s *oaSchema) string {
	lines := g.fields(s, "", newNameSet())
	if len(lines) == 0 {
		return "map[string]any"
	}
	return "struct {\n" + strings.Join(lines, "\n") + "\n}"
}

// fields renders one object's Go fields, in document order. owner names the
// declaration for the migration report when a property has to be parked.
func (g *featureGen) fields(s *oaSchema, owner string, taken *nameSet) []string {
	var out []string
	for _, prop := range s.PropKeys {
		typ, todo := g.goType(s.Props[prop])
		name := fieldName(prop)
		if name == "" {
			name = "Field"
		}
		name, _ = taken.take(name)
		if todo != "" {
			out = append(out, "// "+todo)
			if owner != "" {
				g.rep.Parked = append(g.rep.Parked, owner+"."+prop)
			}
		}
		line := fmt.Sprintf("%s %s `json:%q`", name, typ, prop)
		if e := s.Props[prop].Enum; len(e) > 0 {
			line += " // " + enumNote(e)
		}
		out = append(out, line)
	}
	return out
}

func enumNote(values []string) string {
	if len(values) > 8 {
		values = append(append([]string{}, values[:8]...), "…")
	}
	return "enum: " + strings.Join(values, " | ")
}

// scalarType constrains a path/query parameter to what the binder can fill.
// Anything else becomes a string and is reported — a silently dropped filter
// is a bug the migration only finds in production.
func (g *featureGen) scalarType(p *oaParam, where string) string {
	t, _ := g.goType(p.Schema)
	switch strings.TrimPrefix(t, "*") {
	case "string", "bool", "int", "int32", "int64", "float32", "float64", "time.Time":
		return strings.TrimPrefix(t, "*") // an optional parameter is the zero value
	}
	g.rep.skip("parameter", where+" ?"+p.Name,
		"a path/query parameter binds as a scalar; this one is "+t+" and was narrowed to string")
	return "string"
}

// ─────────────────────── the feature ───────────────────────

func buildFeature(spec *oaSpec, b *bucket, shared map[string]bool, source string, rep *migrationReport) (*genFeature, error) {
	g := &featureGen{
		spec: spec, pkg: b.pkg, names: newNameSet(),
		tname: map[string]string{}, alias: map[string]string{}, busy: map[string]bool{},
		rep: rep, needJS: map[string]bool{}, needTm: map[string]bool{},
	}
	f := &genFeature{Pkg: b.pkg, Tag: b.tag, Source: source}
	f.Doc = featureDoc(b)

	// 1. Component struct names first, so every $ref below resolves.
	for _, name := range spec.SchemaKeys {
		if !b.seen[name] {
			continue
		}
		s := resolve(spec, spec.Schemas[name])
		if s == nil || !s.isObject() {
			continue // a non-object component inlines at its use site
		}
		want := camelOf(name)
		if shared[name] {
			// Two features need it and features never import features, so it
			// is generated in both — package-qualified, exactly the way the api
			// generators disambiguate a cross-package type name.
			want = camelOf(b.pkg) + camelOf(name)
		}
		if want == "" {
			want = "Schema"
		}
		tn, _ := g.names.take(want)
		g.tname[name] = tn
	}

	// 2. The declarations.
	g.file = "types"
	for _, name := range spec.SchemaKeys {
		tn, ok := g.tname[name]
		if !ok || !b.seen[name] {
			continue
		}
		s := resolve(spec, spec.Schemas[name])
		decl := fmt.Sprintf("// %s is components.schemas.%s.\n", tn, name)
		if d := leadLine(s.Description); d != "" {
			decl += "//\n// " + d + "\n"
		}
		lines := g.fields(s, tn, newNameSet())
		if len(lines) == 0 {
			decl += fmt.Sprintf("type %s struct{}", tn)
		} else {
			decl += fmt.Sprintf("type %s struct {\n%s\n}", tn, strings.Join(lines, "\n"))
		}
		f.Types = append(f.Types, &genType{Name: tn, Component: name, Decl: decl})
	}

	// 3. The operations.
	g.file = "handler"
	codes := newCodeSet(b.pkg, g.names)
	for _, op := range b.ops {
		gop := g.operation(op, codes)
		f.Ops = append(f.Ops, gop)
	}
	codes.standardIfEmpty()
	f.Codes = codes.list()

	// 4. Render.
	f.Wiring = renderWiring(f.Ops)
	f.Handlers = renderHandlers(f.Ops)
	f.CodeDefs = renderCodes(f.Codes)
	if len(f.Types) > 0 {
		f.HasTypes = true
		var parts []string
		for _, t := range f.Types {
			parts = append(parts, t.Decl)
		}
		f.TypeDefs = strings.Join(parts, "\n\n")
	}
	f.HandlerImports = stdImports(g.needJS["handler"], g.needTm["handler"])
	f.TypeImports = stdImports(g.needJS["types"], g.needTm["types"])

	rep.Stubs += len(f.Ops)
	for _, op := range f.Ops {
		if op.Require == "" {
			rep.Public++
		} else {
			rep.Guarded++
		}
	}
	return f, nil
}

func featureDoc(b *bucket) string {
	return fmt.Sprintf("the %s surface of the legacy service, ported one stub at a time", b.tag)
}

func stdImports(needJSON, needTime bool) string {
	var out []string
	if needJSON {
		out = append(out, `"encoding/json"`)
	}
	if needTime {
		out = append(out, `"time"`)
	}
	return strings.Join(out, "\n")
}

func leadLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 100 {
		s = strings.TrimSpace(s[:100]) + "…"
	}
	return s
}

// operation translates one op into everything the three files need.
func (g *featureGen) operation(op *oaOp, codes *codeSet) *genOp {
	sym := op.plannedSym
	where := op.Method + " " + op.Path

	fn := sym
	if goBuiltins[fn] || goKeywords[fn] {
		// The doctrine's "never name a handler delete" rule, mechanized.
		fn = "handle" + upperFirstOf(fn)
	}
	fn, _ = g.names.take(fn)

	gop := &genOp{
		Dotted: op.plannedDotted, Sym: sym, PinName: op.plannedPin,
		Method: op.Method, Path: op.Path,
		Summary: opSummary(op), Deprecated: op.Deprecated, Func: fn,
	}
	if op.Secured {
		gop.Require = g.pkg + "." + permVerb(op.Method)
	}

	gop.ReqType = g.request(op, sym, where, gop)
	gop.RespType, gop.RespZero = g.response(op, sym, gop)

	gop.StubCode = codes.stub(sym, op.plannedDotted)
	gop.Codes = append(gop.Codes, gop.StubCode)
	for _, status := range op.Errors {
		gop.Codes = append(gop.Codes, codes.declaredConst(status))
	}
	return gop
}

// opSummary is the operation's one line: its summary, else the first line of
// its description — a legacy document usually has one or the other.
func opSummary(op *oaOp) string {
	if s := leadLine(op.Summary); s != "" {
		return s
	}
	return leadLine(op.Description)
}

// permVerb guesses the permission half from the method. It is a GUESS, and the
// generated line says so — the real permission vocabulary is a decision the
// port makes, not one a document can answer.
func permVerb(method string) string {
	if strings.ToUpper(method) == "GET" {
		return "read"
	}
	return "write"
}

// request builds the operation's request struct and returns its type
// expression ("api.None" when the operation binds nothing).
func (g *featureGen) request(op *oaOp, sym, where string, gop *genOp) string {
	taken := newNameSet()
	var fields []string

	// Every {param} in the path becomes a field whether the document declared
	// it or not: the pattern demands it, and an undeclared path parameter is a
	// documentation bug, not an absent input.
	for _, seg := range segmentsOf(op.Path) {
		if !isParamSeg(seg) {
			continue
		}
		name := seg[1 : len(seg)-1]
		typ := "string"
		if p := findParam(op.Params, "path", name); p != nil && p.Schema != nil {
			typ = g.scalarType(p, where)
		}
		field, _ := taken.take(fieldName(name))
		fields = append(fields, fmt.Sprintf("%s %s `path:%q`", field, typ, name))
	}
	for _, p := range op.Params {
		switch p.In {
		case "query":
			field, _ := taken.take(fieldName(p.Name))
			fields = append(fields, fmt.Sprintf("%s %s `query:%q`", field, g.scalarType(p, where), p.Name))
		case "header", "cookie":
			g.rep.skip("parameter", where+" "+p.In+" "+p.Name,
				"a bound request reads path and query; read this one from the request in a raw route if you need it")
		}
	}

	// The body. A $ref to an object embeds (one source of truth for the
	// payload); an inline object flattens into the request itself.
	if op.Body != nil && !op.Body.Empty && op.Body.Schema != nil {
		bs := op.Body.Schema
		if bs.Ref != "" {
			if t, ok := g.tname[bs.Ref]; ok {
				if len(fields) > 0 {
					fields = append(fields, "")
				}
				fields = append(fields, "// the request body — components.schemas."+bs.Ref, t)
			}
		} else if r := resolve(g.spec, bs); r != nil && r.isObject() {
			if len(fields) > 0 {
				fields = append(fields, "")
			}
			fields = append(fields, g.fields(r, camelOf(sym)+"Req", taken)...)
		}
	}

	if len(fields) == 0 {
		return "api.None"
	}
	name, _ := g.names.take(camelOf(sym) + "Req")
	gop.Decls = append(gop.Decls, fmt.Sprintf(
		"// %s is the request contract of %s %s.\ntype %s struct {\n%s\n}",
		name, op.Method, op.Path, name, strings.Join(fields, "\n")))
	return name
}

// response picks the primary 2xx payload and returns its Go type plus the zero
// value the stub returns with its 501.
func (g *featureGen) response(op *oaOp, sym string, gop *genOp) (string, string) {
	p := op.Resp
	if p == nil || p.Empty || p.Schema == nil {
		return "api.None", "api.None{}"
	}
	s := p.Schema
	// An inline object response earns a named struct: it IS the contract, and
	// an anonymous struct in a signature is unreadable and unextendable.
	if r := resolve(g.spec, s); s.Ref == "" && r != nil && r.isObject() {
		name, _ := g.names.take(camelOf(sym) + "Resp")
		lines := g.fields(r, name, newNameSet())
		gop.Decls = append(gop.Decls, fmt.Sprintf(
			"// %s is the %d response of %s %s.\ntype %s struct {\n%s\n}",
			name, op.RespStatus, op.Method, op.Path, name, strings.Join(lines, "\n")))
		return name, name + "{}"
	}
	typ, _ := g.goType(s)
	return typ, zeroOf(typ)
}

func zeroOf(typ string) string {
	switch {
	case strings.HasPrefix(typ, "[]"), strings.HasPrefix(typ, "map["),
		strings.HasPrefix(typ, "*"), typ == "json.RawMessage", typ == "any":
		return "nil"
	case typ == "string":
		return `""`
	case typ == "bool":
		return "false"
	case typ == "int", typ == "int32", typ == "int64", typ == "float32", typ == "float64":
		return "0"
	default:
		return typ + "{}"
	}
}

func findParam(params []*oaParam, in, name string) *oaParam {
	for _, p := range params {
		if p.In == in && p.Name == name {
			return p
		}
	}
	return nil
}

// ─────────────────────── the error contract ───────────────────────

// codeSet builds one feature's errors.go: the codes the source document's
// error responses imply, plus one not-implemented code per operation.
type codeSet struct {
	pkg      string
	names    *nameSet
	declared map[int]string
	order    []genCode
}

func newCodeSet(pkg string, names *nameSet) *codeSet {
	return &codeSet{pkg: pkg, names: names, declared: map[int]string{}}
}

// statusSuffix is the dotted tail a declared error status scaffolds into.
var statusSuffix = map[int]string{
	400: "invalid_request", 401: "unauthorized", 403: "forbidden",
	404: "not_found", 405: "not_allowed", 409: "conflict", 410: "gone",
	412: "precondition_failed", 415: "unsupported_media", 422: "invalid",
	423: "locked", 429: "rate_limited", 500: "internal", 501: "not_implemented",
	502: "upstream", 503: "unavailable", 504: "upstream_timeout",
}

func (c *codeSet) declaredCode(status int) string {
	suffix, ok := statusSuffix[status]
	if !ok {
		suffix = "status_" + strconv.Itoa(status)
	}
	return suffix
}

// declaredConst registers (once) the code for a status the document declared,
// and returns the const name that carries it.
func (c *codeSet) declaredConst(status int) string {
	return c.code(status, fmt.Sprintf("%d, declared by the source document", status))
}

func (c *codeSet) code(status int, note string) string {
	if name, ok := c.declared[status]; ok {
		return name
	}
	suffix := c.declaredCode(status)
	name, _ := c.names.take("Code" + camelOf(suffix))
	c.declared[status] = name
	c.order = append(c.order, genCode{Const: name, Value: c.pkg + "." + suffix, Note: note})
	return name
}

func (c *codeSet) stub(sym, dotted string) string {
	name, _ := c.names.take("Code" + camelOf(sym) + "NotImplemented")
	c.order = append(c.order, genCode{
		Const: name, Value: dotted + ".not_implemented",
		Note: "the scaffolded stub; delete this const when the port lands",
	})
	return name
}

// standardIfEmpty scaffolds the standard trio when the source document
// declared no error responses at all — plenty of legacy documents describe
// only the happy path, and a feature still needs somewhere for its first real
// code to land. Unused consts cost nothing and name the convention.
func (c *codeSet) standardIfEmpty() {
	if len(c.declared) > 0 {
		return
	}
	for _, status := range []int{404, 422, 409} {
		c.code(status, fmt.Sprintf("%d — the standard set; the source document declared no error responses", status))
	}
}

func (c *codeSet) list() []genCode {
	// Declared codes first (they are the feature's real error surface), then
	// the stub codes, each group in allocation order.
	var declared, stubs []genCode
	for _, g := range c.order {
		if strings.HasSuffix(g.Value, ".not_implemented") {
			stubs = append(stubs, g)
			continue
		}
		declared = append(declared, g)
	}
	return append(declared, stubs...)
}

// ─────────────────────── rendering ───────────────────────

func renderWiring(ops []*genOp) string {
	var b strings.Builder
	for _, op := range ops {
		fmt.Fprintf(&b, "\n// %s %s", op.Method, op.Path)
		if op.Summary != "" {
			fmt.Fprintf(&b, " — %s", op.Summary)
		}
		fmt.Fprintf(&b, "\napi.Handle(%q, %q, %s,\n", op.Dotted, op.Method+" "+op.Path, op.Func)
		if op.PinName {
			fmt.Fprintf(&b, "api.Name(%q), // the source document's operationId\n", op.Sym)
		}
		if op.Require != "" {
			fmt.Fprintf(&b, "api.Require(%q), // TODO(migration): guessed from the method — pin the real permission\n", op.Require)
		} else {
			b.WriteString("api.Public(), // TODO(migration): unguarded in the source document — confirm that is still right\n")
		}
		if op.Summary != "" {
			fmt.Fprintf(&b, "api.Summary(%q),\n", op.Summary)
		}
		if op.Deprecated {
			b.WriteString("api.Deprecated(),\n")
		}
		fmt.Fprintf(&b, "api.Codes(%s),\n),\n", strings.Join(op.Codes, ", "))
	}
	return strings.TrimLeft(b.String(), "\n")
}

func renderHandlers(ops []*genOp) string {
	var parts []string
	for _, op := range ops {
		parts = append(parts, op.Decls...)
	}
	for _, op := range ops {
		req := "req " + op.ReqType
		if op.ReqType == "api.None" {
			req = "_ api.None"
		}
		parts = append(parts, fmt.Sprintf(`// %s serves %s %s.
//
// TODO(migration): port the legacy implementation. Until then it answers 501
// through the problem envelope — the endpoint exists, the contract is real,
// and the client learns the truth instead of a 404.
func %s(ctx context.Context, %s) (%s, error) {
	return %s, api.Fail(http.StatusNotImplemented, %s,
		"TODO: port the legacy implementation")
}`, op.Func, op.Method, op.Path, op.Func, req, op.RespType, op.RespZero, op.StubCode))
	}
	return strings.Join(parts, "\n\n")
}

func renderCodes(codes []genCode) string {
	if len(codes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("const (\n")
	for i, c := range codes {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "// %s\n%s = %q\n", c.Note, c.Const, c.Value)
	}
	b.WriteString(")\n")
	return b.String()
}

// ─────────────────────── the migration report ───────────────────────

type reportSkip struct{ Kind, Where, Why string }

// migrationReport is what the scaffold prints when it is done — the honest
// inventory of what came across, what was guessed, and what did not.
type migrationReport struct {
	Source, Title, SpecVersion string
	Features                   []*genFeature
	Derived                    []string // operations whose name was derived
	Collisions                 []string // ...and whose name had to be disambiguated
	Shared                     []string // component schemas two features both use
	Merged                     []string // tags that folded into one package
	Parked                     []string // fields parked as json.RawMessage
	Skips                      []reportSkip
	Guarded, Public, Stubs     int
}

func (r *migrationReport) skip(kind, where, why string) {
	r.Skips = append(r.Skips, reportSkip{kind, where, why})
}

func (r *migrationReport) mergedTag(kept, folded, pkg string) {
	line := fmt.Sprintf("%q and %q both reduce to package %s", kept, folded, pkg)
	for _, l := range r.Merged {
		if l == line {
			return
		}
	}
	r.Merged = append(r.Merged, line)
}

// String renders the report. It is the last thing `ultra new --from` prints
// and the first thing the migration reads, so it answers three questions in
// order: what did I get, what did the tool guess, and what did it refuse.
func (r *migrationReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nmigration report — %s (OpenAPI %s", r.Source, r.SpecVersion)
	if r.Title != "" {
		fmt.Fprintf(&b, ", %q", r.Title)
	}
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "  %-11s %d\n", "features", len(r.Features))
	for _, f := range r.Features {
		types := ""
		if len(f.Types) > 0 {
			types = fmt.Sprintf(", %d schema%s", len(f.Types), plural(len(f.Types)))
		}
		fmt.Fprintf(&b, "  %-11s internal/app/%s  (tag %q, %d op%s%s)\n",
			"", f.Pkg, f.Tag, len(f.Ops), plural(len(f.Ops)), types)
	}
	fmt.Fprintf(&b, "  %-11s %d — every one a 501 stub behind the problem envelope\n", "operations", r.Stubs)
	fmt.Fprintf(&b, "  %-11s %d guarded (api.Require, GUESSED), %d public\n", "security", r.Guarded, r.Public)

	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n  %s (%d):\n", title, len(lines))
		for _, l := range lines {
			fmt.Fprintf(&b, "    - %s\n", l)
		}
	}
	section("names derived (the document gave none, or an illegal one)", r.Derived)
	section("names disambiguated — pinned with api.Name", r.Collisions)
	section("component schemas used by more than one feature — generated in each, package-qualified", r.Shared)
	section("tags folded into one package", r.Merged)
	section("fields parked as json.RawMessage — give them a real type", r.Parked)

	if len(r.Skips) > 0 {
		fmt.Fprintf(&b, "\n  NOT translated (%d) — these did not come across, and nothing pretends they did:\n", len(r.Skips))
		sorted := append([]reportSkip{}, r.Skips...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Kind < sorted[j].Kind })
		for _, s := range sorted {
			fmt.Fprintf(&b, "    - %-9s %s\n                %s\n", s.Kind+":", s.Where, s.Why)
		}
	}

	b.WriteString(`
  next:
    task test        # the covenant: wiring, boot, and the contract gate
    task dev:api     # every endpoint answers 501 with a problem envelope
    then fill ONE stub, run task contracts, commit — and repeat. The covenant
    stays green the whole way across, which is the point of the 501s.
`)
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
