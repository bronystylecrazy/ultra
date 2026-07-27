package main

// The OpenAPI reader behind `ultra new <name> --from openapi.json`.
//
// It is deliberately a READER, not a validator: a legacy document is whatever
// the legacy service happens to serve, and the migration must not stall on a
// missing `description`. Everything it cannot honestly translate is COLLECTED
// (see skips) rather than guessed at, and the scaffold prints the list — an
// unsupported construct the tool stayed quiet about is the one that bites in
// production six weeks later.
//
// JSON only, this round. A .yaml document is refused with the conversion
// one-liner rather than half-parsed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// oaSpec is the whole source document, reduced to what the scaffold needs.
// Document order is preserved everywhere the generators turn it into source,
// so two runs over one file write byte-identical Go.
type oaSpec struct {
	Version    string // the `openapi` string, e.g. "3.1.0"
	Title      string
	APIVersion string
	Paths      []*oaPathItem        // document order
	Schemas    map[string]*oaSchema // components.schemas
	SchemaKeys []string             // ...in document order
	Secured    bool                 // a non-empty top-level `security` is declared
	Webhooks   []string             // 3.1 webhooks: named, never translated
}

// oaPathItem is one entry under `paths`.
type oaPathItem struct {
	Path   string
	Params []*oaParam // path-item level parameters, merged into every operation
	Ops    []*oaOp    // document order
}

// oaOp is one operation object.
type oaOp struct {
	Method      string // upper-case
	Path        string
	OperationID string
	Tags        []string
	Summary     string
	Description string
	Deprecated  bool
	Params      []*oaParam
	Body        *oaPayload // nil when the operation declares no request body
	Resp        *oaPayload // the primary 2xx; nil when there is no 2xx content
	RespStatus  int
	Errors      []int    // the declared 4xx/5xx statuses, ascending
	Callbacks   []string // callback names: listed, never translated
	Secured     bool     // effective security (op override, else the document's)

	// The vendor extensions contrib/api emits and `ultra breaking` gates on.
	// ErrorCodes is the UNION the generated client's <Fn>Error union is built
	// from: the operation-level x-error-codes (api.Codes) plus every error
	// response's own, sorted and deduplicated.
	ErrorCodes []string
	Permission string // x-required-permission

	// Settled by nameOp before any feature renders, because both names are
	// unique across the WHOLE product and a feature cannot see its siblings.
	plannedSym    string // the client symbol / operationId
	plannedDotted string // the dotted registry key
	plannedPin    bool   // the symbol needs an explicit api.Name
}

// oaParam is one path/query/header/cookie parameter.
type oaParam struct {
	Name     string
	In       string
	Required bool
	Schema   *oaSchema
	Desc     string
}

// oaPayload is a request body or a response: the JSON schema plus the media
// types the source declared (so a non-JSON-only payload can be reported by
// name instead of silently becoming `api.None`).
type oaPayload struct {
	Schema   *oaSchema
	Media    []string // every media type the source listed, document order
	JSON     bool     // one of them is application/json (or a +json suffix)
	Empty    bool     // the payload declares no content at all (a 204)
	Required bool     // requestBody.required — meaningless on a response
}

// oaSchema is a JSON Schema subset: the constructs the translator can turn
// into a Go type, plus enough of the rest to name what it is skipping.
type oaSchema struct {
	Ref         string // "#/components/schemas/Depot" → "Depot"
	Types       []string
	Format      string
	Nullable    bool
	Items       *oaSchema
	PropKeys    []string // document order
	Props       map[string]*oaSchema
	Required    []string
	AddProps    *oaSchema   // additionalProperties: {schema}
	AddPropsAny bool        // additionalProperties: true
	Compose     string      // "oneOf" / "anyOf" / "allOf" when present
	Members     []*oaSchema // ...and its members, in document order
	Enum        []string
	Description string
}

// methodOrder is the operation-object key set the translator supports, in the
// order a path item is walked. HEAD/OPTIONS/TRACE are read and reported as
// skipped: they carry no typed contract worth generating, and inventing one
// would put a route in the product the legacy service never served.
var methodOrder = []string{"get", "put", "post", "patch", "delete"}

var unsupportedMethods = []string{"head", "options", "trace"}

// readSpec parses an OpenAPI 3.x JSON document.
func readSpec(name string, raw []byte) (*oaSpec, error) {
	var head struct {
		OpenAPI string `json:"openapi"`
		Swagger string `json:"swagger"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths      json.RawMessage `json:"paths"`
		Components struct {
			Schemas json.RawMessage `json:"schemas"`
		} `json:"components"`
		Security []json.RawMessage `json:"security"`
		Webhooks json.RawMessage   `json:"webhooks"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", name, err)
	}
	switch {
	case head.Swagger != "":
		return nil, fmt.Errorf("%s is Swagger %s, not OpenAPI 3.x — convert it first "+
			"(https://converter.swagger.io) and pass the 3.x document", name, head.Swagger)
	case head.OpenAPI == "":
		return nil, fmt.Errorf("%s has no `openapi` version field — is it an OpenAPI document?", name)
	case !strings.HasPrefix(head.OpenAPI, "3."):
		return nil, fmt.Errorf("%s declares OpenAPI %s; --from reads 3.0 and 3.1", name, head.OpenAPI)
	}

	spec := &oaSpec{
		Version:    head.OpenAPI,
		Title:      head.Info.Title,
		APIVersion: head.Info.Version,
		Schemas:    map[string]*oaSchema{},
		Secured:    len(head.Security) > 0 && !onlyEmptyRequirements(head.Security),
	}

	if len(head.Components.Schemas) > 0 {
		keys, vals, err := orderedObject(head.Components.Schemas)
		if err != nil {
			return nil, fmt.Errorf("%s: components.schemas: %w", name, err)
		}
		spec.SchemaKeys = keys
		for _, k := range keys {
			spec.Schemas[k] = parseSchema(vals[k])
		}
	}
	if len(head.Webhooks) > 0 {
		keys, _, err := orderedObject(head.Webhooks)
		if err == nil {
			spec.Webhooks = keys
		}
	}
	if len(head.Paths) > 0 {
		keys, vals, err := orderedObject(head.Paths)
		if err != nil {
			return nil, fmt.Errorf("%s: paths: %w", name, err)
		}
		for _, p := range keys {
			item, err := parsePathItem(p, vals[p], spec.Secured)
			if err != nil {
				return nil, fmt.Errorf("%s: paths.%s: %w", name, p, err)
			}
			spec.Paths = append(spec.Paths, item)
		}
	}
	return spec, nil
}

// onlyEmptyRequirements reports whether every entry of a `security` array is
// the empty object — the spec's way of saying "optional", which for our
// purposes is the same as unguarded.
func onlyEmptyRequirements(reqs []json.RawMessage) bool {
	for _, r := range reqs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r, &m); err == nil && len(m) > 0 {
			return false
		}
	}
	return true
}

func parsePathItem(path string, raw json.RawMessage, globalSecurity bool) (*oaPathItem, error) {
	var item struct {
		Parameters []json.RawMessage `json:"parameters"`
	}
	_ = json.Unmarshal(raw, &item)

	out := &oaPathItem{Path: path}
	for _, p := range item.Parameters {
		if param := parseParam(p); param != nil {
			out.Params = append(out.Params, param)
		}
	}

	keys, vals, err := orderedObject(raw)
	if err != nil {
		return nil, err
	}
	supported := map[string]bool{}
	for _, m := range methodOrder {
		supported[m] = true
	}
	for _, k := range keys {
		method := strings.ToLower(k)
		if !supported[method] && !contains(unsupportedMethods, method) {
			continue
		}
		op, err := parseOp(strings.ToUpper(method), path, vals[k], globalSecurity)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		out.Ops = append(out.Ops, op)
	}
	// Document order for a path item is whatever the author typed; the method
	// order is normalized so two documents describing the same endpoints
	// scaffold identically.
	sort.SliceStable(out.Ops, func(i, j int) bool {
		return methodRank(out.Ops[i].Method) < methodRank(out.Ops[j].Method)
	})
	return out, nil
}

func methodRank(m string) int {
	for i, s := range methodOrder {
		if strings.EqualFold(s, m) {
			return i
		}
	}
	return len(methodOrder) + 1
}

func parseOp(method, path string, raw json.RawMessage, globalSecurity bool) (*oaOp, error) {
	var o struct {
		OperationID string            `json:"operationId"`
		Tags        []string          `json:"tags"`
		Summary     string            `json:"summary"`
		Description string            `json:"description"`
		Deprecated  bool              `json:"deprecated"`
		Parameters  []json.RawMessage `json:"parameters"`
		RequestBody json.RawMessage   `json:"requestBody"`
		Responses   json.RawMessage   `json:"responses"`
		Security    []json.RawMessage `json:"security"`
		HasSecurity json.RawMessage   `json:"-"`
		Callbacks   json.RawMessage   `json:"callbacks"`
		ErrorCodes  []string          `json:"x-error-codes"`
		Permission  string            `json:"x-required-permission"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	// An operation-level `security` KEY overrides the document's, including the
	// empty array that means "this one is public". Its absence inherits.
	var probe map[string]json.RawMessage
	_ = json.Unmarshal(raw, &probe)
	secured := globalSecurity
	if _, declared := probe["security"]; declared {
		secured = len(o.Security) > 0 && !onlyEmptyRequirements(o.Security)
	}

	op := &oaOp{
		Method: method, Path: path,
		OperationID: o.OperationID,
		Tags:        o.Tags,
		Summary:     o.Summary,
		Description: o.Description,
		Deprecated:  o.Deprecated,
		Secured:     secured,
		ErrorCodes:  o.ErrorCodes,
		Permission:  o.Permission,
	}
	for _, p := range o.Parameters {
		if param := parseParam(p); param != nil {
			op.Params = append(op.Params, param)
		}
	}
	if len(o.RequestBody) > 0 {
		op.Body = parsePayload(o.RequestBody)
	}
	if len(o.Callbacks) > 0 {
		if keys, _, err := orderedObject(o.Callbacks); err == nil {
			op.Callbacks = keys
		}
	}
	if len(o.Responses) > 0 {
		keys, vals, err := orderedObject(o.Responses)
		if err != nil {
			return nil, fmt.Errorf("responses: %w", err)
		}
		best := ""
		for _, k := range keys {
			switch {
			case len(k) == 3 && k[0] == '2':
				if best == "" || k < best {
					best = k
				}
			case len(k) == 3 && (k[0] == '4' || k[0] == '5'):
				n := 0
				if _, err := fmt.Sscanf(k, "%d", &n); err == nil {
					op.Errors = append(op.Errors, n)
				}
				// contrib/api puts the codes valid for THIS status on the
				// response; api.Codes declarations ride the operation. The
				// generated client's <Fn>Error union is the union of both.
				var resp struct {
					Codes []string `json:"x-error-codes"`
				}
				if json.Unmarshal(vals[k], &resp) == nil {
					op.ErrorCodes = append(op.ErrorCodes, resp.Codes...)
				}
			}
		}
		if best != "" {
			op.Resp = parsePayload(vals[best])
			_, _ = fmt.Sscanf(best, "%d", &op.RespStatus)
		}
		sort.Ints(op.Errors)
	}
	sort.Strings(op.ErrorCodes)
	op.ErrorCodes = slices.Compact(op.ErrorCodes)
	return op, nil
}

func parseParam(raw json.RawMessage) *oaParam {
	var p struct {
		Name        string          `json:"name"`
		In          string          `json:"in"`
		Required    bool            `json:"required"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Ref         string          `json:"$ref"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Name == "" {
		// A $ref'd parameter component is a construct this round does not
		// resolve; the path template still yields the field, so nothing is
		// lost for path parameters and a query parameter is reported.
		return nil
	}
	out := &oaParam{Name: p.Name, In: strings.ToLower(p.In), Required: p.Required, Desc: p.Description}
	if len(p.Schema) > 0 {
		out.Schema = parseSchema(p.Schema)
	}
	if out.In == "path" {
		out.Required = true
	}
	return out
}

// jsonMedia reports whether a media type carries JSON (application/json and
// the +json structured-suffix family).
func jsonMedia(m string) bool {
	m = strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0]))
	return m == "application/json" || m == "*/*" || strings.HasSuffix(m, "+json")
}

func parsePayload(raw json.RawMessage) *oaPayload {
	var body struct {
		Content  json.RawMessage `json:"content"`
		Required bool            `json:"required"`
	}
	_ = json.Unmarshal(raw, &body)
	out := &oaPayload{Required: body.Required}
	if len(body.Content) == 0 {
		out.Empty = true
		return out
	}
	keys, vals, err := orderedObject(body.Content)
	if err != nil || len(keys) == 0 {
		out.Empty = true
		return out
	}
	out.Media = keys
	for _, k := range keys {
		if !jsonMedia(k) {
			continue
		}
		out.JSON = true
		var mt struct {
			Schema json.RawMessage `json:"schema"`
		}
		if err := json.Unmarshal(vals[k], &mt); err == nil && len(mt.Schema) > 0 {
			out.Schema = parseSchema(mt.Schema)
		}
		break
	}
	return out
}

// parseSchema reads one schema object. Unknown keywords are ignored rather
// than rejected; what matters is which of the translatable shapes it is.
func parseSchema(raw json.RawMessage) *oaSchema {
	var s struct {
		Ref         string            `json:"$ref"`
		Type        json.RawMessage   `json:"type"`
		Format      string            `json:"format"`
		Nullable    bool              `json:"nullable"`
		Items       json.RawMessage   `json:"items"`
		Properties  json.RawMessage   `json:"properties"`
		Required    []string          `json:"required"`
		AddProps    json.RawMessage   `json:"additionalProperties"`
		OneOf       []json.RawMessage `json:"oneOf"`
		AnyOf       []json.RawMessage `json:"anyOf"`
		AllOf       []json.RawMessage `json:"allOf"`
		Enum        []json.RawMessage `json:"enum"`
		Description string            `json:"description"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return &oaSchema{}
	}
	out := &oaSchema{
		Format: s.Format, Nullable: s.Nullable,
		Required: s.Required, Description: s.Description,
	}
	if s.Ref != "" {
		out.Ref = strings.TrimPrefix(s.Ref, "#/components/schemas/")
		return out
	}
	// 3.0 spells the type as a string; 3.1 allows the ["string","null"] array
	// that replaces `nullable`. Both land in Types with "null" extracted.
	if len(s.Type) > 0 {
		var one string
		var many []string
		if err := json.Unmarshal(s.Type, &one); err == nil {
			many = []string{one}
		} else {
			_ = json.Unmarshal(s.Type, &many)
		}
		for _, t := range many {
			if t == "null" {
				out.Nullable = true
				continue
			}
			out.Types = append(out.Types, t)
		}
	}
	var members []json.RawMessage
	switch {
	case len(s.OneOf) > 0:
		out.Compose, members = "oneOf", s.OneOf
	case len(s.AnyOf) > 0:
		out.Compose, members = "anyOf", s.AnyOf
	case len(s.AllOf) > 0:
		out.Compose, members = "allOf", s.AllOf
	}
	for _, m := range members {
		out.Members = append(out.Members, parseSchema(m))
	}
	if len(s.Items) > 0 {
		out.Items = parseSchema(s.Items)
	}
	if len(s.Properties) > 0 {
		keys, vals, err := orderedObject(s.Properties)
		if err == nil {
			out.PropKeys = keys
			out.Props = map[string]*oaSchema{}
			for _, k := range keys {
				out.Props[k] = parseSchema(vals[k])
			}
		}
	}
	if len(s.AddProps) > 0 {
		var b bool
		if err := json.Unmarshal(s.AddProps, &b); err == nil {
			out.AddPropsAny = b
		} else {
			out.AddProps = parseSchema(s.AddProps)
		}
	}
	for _, e := range s.Enum {
		var str string
		if err := json.Unmarshal(e, &str); err == nil {
			out.Enum = append(out.Enum, str)
			continue
		}
		out.Enum = append(out.Enum, strings.TrimSpace(string(e)))
	}
	return out
}

// primary returns the schema's leading non-null type ("" when untyped).
func (s *oaSchema) primary() string {
	if s == nil || len(s.Types) == 0 {
		return ""
	}
	return s.Types[0]
}

// isObject reports whether a schema describes a JSON object with named
// properties — the only shape that becomes a Go struct.
func (s *oaSchema) isObject() bool {
	return s != nil && len(s.PropKeys) > 0 && s.Compose == ""
}

// orderedObject decodes a JSON object while PRESERVING key order.
// encoding/json's map loses it, and losing it means a struct whose field order
// changes with the wind — the generated source would churn on every run for no
// reason a reviewer could explain.
func orderedObject(raw []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("expected a JSON object")
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected an object key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := vals[key]; !dup {
			keys = append(keys, key)
		}
		vals[key] = v
	}
	return keys, vals, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
