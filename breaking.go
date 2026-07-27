package main

// `ultra breaking` is the SEMANTIC CONTRACT GATE.
//
//	ultra breaking <old.json> <new.json>
//	ultra breaking --against <git-ref> [dir]
//
// A fleet whose products call each other has one failure mode worth building a
// command around: a contract change that ships green and breaks a consumer in
// production. `ultra diff` answers the same question about the DI graph; this
// answers it about the wire, and it exits 1 so a pipeline can refuse the merge:
//
//	ultra breaking --against origin/main
//
// WHAT COUNTS AS BREAKING is decided by the direction the bytes travel, not by
// whether the document changed. A response field is read by consumers, so
// removing it breaks them; a request field is WRITTEN by consumers, so removing
// it does not — contrib/api decodes request bodies with a plain
// json.NewDecoder (api.go:414, no DisallowUnknownFields), and a key the server
// no longer knows is dropped in silence. That asymmetry is why removing a
// request field is a WARNING here and removing a response field is BREAKING.
// The warning is not nothing: the sender's intent is being discarded without a
// 4xx, which is worse to debug than a rejection.
//
// The mirror of that rule runs through the whole taxonomy. Adding a REQUEST
// enum value is safe (the server accepts more) while adding a RESPONSE enum
// value is a warning (a consumer switching exhaustively has no arm for it);
// removing a RESPONSE enum value is safe (the producer promises less) while
// removing a REQUEST one rejects senders. Additive changes are reported too,
// in their own section, because "nothing broke" and "nothing changed" are
// different answers and a gate that conflates them teaches people to ignore it.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Severities. Only breaking fails the build; warnings are printed and
// deliberately do NOT gate, so the gate keeps its authority.
const (
	sevBreaking = "breaking"
	sevWarning  = "warning"
	sevAdditive = "additive"
)

// Directions. The same schema walk serves both, because the shapes are
// identical and only the verdict on each difference flips.
const (
	dirReq  = "request"
	dirResp = "response"
)

// breakMaxDepth caps schema recursion. A self-referential component (a tree
// node whose child is itself) is legal and common; the ref stack below stops
// the cycle, and this stops a pathologically deep one from costing a minute.
const breakMaxDepth = 12

// breakFinding is one difference, classified. Location is JSON-pointer-ish and
// RELATIVE to the operation — the operation is already the group heading, and
// "requestBody/properties/priority" reads better than the absolute pointer.
type breakFinding struct {
	Severity  string `json:"severity"`
	Kind      string `json:"kind"`
	Operation string `json:"operation"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Location  string `json:"location"`
	Detail    string `json:"detail"`
}

// breakReport is the whole answer, and the --json byte contract.
type breakReport struct {
	Old      string         `json:"old"`
	New      string         `json:"new"`
	Breaking int            `json:"breaking"`
	Warnings int            `json:"warnings"`
	Additive int            `json:"additive"`
	Findings []breakFinding `json:"findings"`
}

func cmdBreaking(args []string, out, errW io.Writer) int {
	jsonOut, against := false, ""
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--against":
			if i+1 >= len(args) {
				fmt.Fprintln(errW, "Error: --against needs a git ref, e.g. --against origin/main")
				return 2
			}
			against = args[i+1]
			i++
		case strings.HasPrefix(a, "--against="):
			against = strings.TrimPrefix(a, "--against=")
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("breaking"), a)
		default:
			rest = append(rest, a)
		}
	}

	var oldRaw, newRaw []byte
	var oldName, newName string
	if against != "" {
		dir := "."
		if len(rest) == 1 {
			dir = rest[0]
		} else if len(rest) > 1 {
			fmt.Fprintln(errW, "Error: --against takes at most one directory")
			fmt.Fprintln(errW, "\nUsage:\n  ultra breaking --against <git-ref> [dir]")
			return 2
		}
		var code int
		oldRaw, newRaw, oldName, newName, code = breakingAgainst(dir, against, errW)
		if code != 0 {
			return code
		}
	} else {
		if len(rest) != 2 {
			fmt.Fprintln(errW, "Error: breaking takes two OpenAPI documents, or --against <git-ref>")
			fmt.Fprintln(errW, "\nUsage:\n  ultra breaking <old.json> <new.json>\n  ultra breaking --against <git-ref> [dir]")
			return 2
		}
		var err error
		oldName, newName = rest[0], rest[1]
		if oldRaw, err = os.ReadFile(oldName); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "breaking", "unreadable input")
			return 1
		}
		if newRaw, err = os.ReadFile(newName); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "breaking", "unreadable input")
			return 1
		}
	}

	oldSpec, err := readSpec(oldName, oldRaw)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "breaking", "the baseline is not readable")
		return 1
	}
	newSpec, err := readSpec(newName, newRaw)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "breaking", "the new document is not readable")
		return 1
	}

	rep := compareContracts(oldName, newName, oldSpec, newSpec)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else {
		renderBreaking(out, rep)
	}

	if rep.Breaking > 0 {
		detail := count(rep.Breaking, "breaking change")
		if rep.Warnings > 0 {
			detail += ", " + count(rep.Warnings, "warning")
		}
		failVerdict(errW, "breaking", detail)
		return 1
	}
	detail := "no breaking changes"
	switch {
	case rep.Warnings > 0 && rep.Additive > 0:
		detail += fmt.Sprintf(" (%s, %s)", count(rep.Warnings, "warning"), count(rep.Additive, "additive"))
	case rep.Warnings > 0:
		detail += " (" + count(rep.Warnings, "warning") + ")"
	case rep.Additive > 0:
		detail += " (" + count(rep.Additive, "additive change") + ")"
	}
	verdict(errW, "breaking", detail)
	return 0
}

// breakingAgainst loads the baseline from git and the candidate from the
// working tree. Each way this can fail gets its OWN sentence: "not a
// repository", "no such ref" and "the ref has no contract" are three different
// mistakes with three different fixes, and one shared "could not read" would
// send the reader looking in the wrong place.
func breakingAgainst(dir, ref string, errW io.Writer) (oldRaw, newRaw []byte, oldName, newName string, code int) {
	if _, err := git(dir, "rev-parse", "--git-dir"); err != nil {
		fmt.Fprintf(errW, "ultra breaking: %s is not a git repository — --against reads the baseline with `git show`.\n"+
			"Pass two files instead: ultra breaking <old.json> <new.json>\n", displayDir(dir))
		failVerdict(errW, "breaking", "not a git repository")
		return nil, nil, "", "", 2
	}
	if _, err := git(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		fmt.Fprintf(errW, "ultra breaking: no ref %q in this repository.\n"+
			"In CI, fetch it first: git fetch origin main\n", ref)
		failVerdict(errW, "breaking", "unknown ref "+ref)
		return nil, nil, "", "", 2
	}
	// `<ref>:./file` resolves relative to git's working directory, so [dir]
	// selects the right product in a monorepo without any path arithmetic.
	blob, err := git(dir, "show", ref+":./"+contractFile)
	if err != nil {
		fmt.Fprintf(errW, "ultra breaking: %s does not exist at %s — there is no baseline to compare against.\n"+
			"If the contract is new, this change cannot break a consumer that never saw it.\n", contractFile, ref)
		failVerdict(errW, "breaking", "no "+contractFile+" at "+ref)
		return nil, nil, "", "", 2
	}
	newRaw, err = os.ReadFile(filepath.Join(dir, contractFile))
	if err != nil {
		fmt.Fprintf(errW, "ultra breaking: no %s in %s — regenerate it before gating.\n"+
			"  ./app openapi > %s\n", contractFile, displayDir(dir), contractFile)
		failVerdict(errW, "breaking", "no working-tree "+contractFile)
		return nil, nil, "", "", 2
	}
	return []byte(blob), newRaw, ref + ":" + contractFile, contractFile, 0
}

// ---- the comparison ----

type breakCmp struct {
	oldSpec, newSpec *oaSpec
	found            []breakFinding
	op               *oaOp           // the operation being compared, for the stamp
	stack            map[string]bool // $ref pairs on the current walk: the cycle brake
}

func compareContracts(oldName, newName string, oldSpec, newSpec *oaSpec) *breakReport {
	c := &breakCmp{oldSpec: oldSpec, newSpec: newSpec, stack: map[string]bool{}}

	// Operations are matched on METHOD+PATH first, because that is the wire
	// identity a deployed client is holding. operationId is the SECOND key: an
	// operation that kept its id and moved has been relocated, not deleted, and
	// saying "removed listNotes / added listNotes" would hide the one fact that
	// matters — its callers are now hitting a 404.
	newByRoute, newByID := map[string]*oaOp{}, map[string]*oaOp{}
	for _, item := range newSpec.Paths {
		for _, op := range item.Ops {
			newByRoute[op.Method+" "+op.Path] = op
			if op.OperationID != "" {
				newByID[op.OperationID] = op
			}
		}
	}

	matched := map[*oaOp]bool{}
	for _, item := range oldSpec.Paths {
		for _, o := range item.Ops {
			c.op = o
			if n, ok := newByRoute[o.Method+" "+o.Path]; ok {
				matched[n] = true
				if o.OperationID != "" && n.OperationID != "" && o.OperationID != n.OperationID {
					c.add(sevBreaking, "operation.id_changed", "operationId",
						fmt.Sprintf("the generated client renames %s() to %s() — every call site stops compiling",
							o.OperationID, n.OperationID))
				}
				c.diffOp(o, n)
				continue
			}
			if n, ok := newByID[o.OperationID]; ok && o.OperationID != "" {
				matched[n] = true
				c.add(sevBreaking, "operation.moved", "paths",
					fmt.Sprintf("%s moved from %s %s to %s %s — deployed callers keep requesting the old route and get 404",
						o.OperationID, o.Method, o.Path, n.Method, n.Path))
				c.diffOp(o, n)
				continue
			}
			c.add(sevBreaking, "operation.removed", "paths",
				fmt.Sprintf("%s %s is gone — every consumer calling %s gets 404",
					o.Method, o.Path, opLabel(o)))
		}
	}
	for _, item := range newSpec.Paths {
		for _, n := range item.Ops {
			if !matched[n] {
				c.op = n
				c.add(sevAdditive, "operation.added", "paths",
					fmt.Sprintf("%s %s is new — no existing consumer can be calling it yet", n.Method, n.Path))
			}
		}
	}

	rep := &breakReport{Old: oldName, New: newName, Findings: c.found}
	if rep.Findings == nil {
		rep.Findings = []breakFinding{}
	}
	for _, f := range rep.Findings {
		switch f.Severity {
		case sevBreaking:
			rep.Breaking++
		case sevWarning:
			rep.Warnings++
		default:
			rep.Additive++
		}
	}
	return rep
}

// diffOp compares one matched operation across every axis a consumer can
// depend on: auth, parameters, request body, response, error codes.
func (c *breakCmp) diffOp(o, n *oaOp) {
	c.diffAuth(o, n)
	c.diffParams(o, n)
	c.diffBody(o, n)
	c.diffResponse(o, n)
	c.diffErrors(o, n)
}

func (c *breakCmp) diffAuth(o, n *oaOp) {
	switch {
	case !o.Secured && n.Secured:
		c.add(sevBreaking, "auth.required_added", "security",
			fmt.Sprintf("%s was public and now requires credentials — anonymous callers get 401", opLabel(o)))
	case o.Secured && !n.Secured:
		c.add(sevAdditive, "auth.required_removed", "security",
			fmt.Sprintf("%s no longer requires credentials — a caller that sends them is unaffected", opLabel(o)))
	}
	switch {
	case o.Permission == "" && n.Permission != "":
		c.add(sevBreaking, "auth.permission_added", "x-required-permission",
			fmt.Sprintf("%s now demands the %q permission — callers whose token lacks it get 403", opLabel(o), n.Permission))
	case o.Permission != "" && n.Permission != "" && o.Permission != n.Permission:
		c.add(sevBreaking, "auth.permission_changed", "x-required-permission",
			fmt.Sprintf("%s moved from the %q permission to %q — tokens granted the old one get 403",
				opLabel(o), o.Permission, n.Permission))
	case o.Permission != "" && n.Permission == "":
		c.add(sevAdditive, "auth.permission_removed", "x-required-permission",
			fmt.Sprintf("%s no longer demands the %q permission — every caller that passed before still does",
				opLabel(o), o.Permission))
	}
}

func (c *breakCmp) diffParams(o, n *oaOp) {
	oldP := map[string]*oaParam{}
	for _, p := range o.Params {
		oldP[p.In+" "+p.Name] = p
	}
	newP := map[string]*oaParam{}
	for _, p := range n.Params {
		newP[p.In+" "+p.Name] = p
	}
	for _, p := range o.Params {
		np, ok := newP[p.In+" "+p.Name]
		if !ok {
			c.add(sevWarning, "request.param.removed", "parameters/"+p.In+"/"+p.Name,
				fmt.Sprintf("callers of %s still send the %s parameter %q and it is now ignored in silence, not rejected",
					opLabel(o), p.In, p.Name))
			continue
		}
		if !p.Required && np.Required {
			c.add(sevBreaking, "request.param.required_added", "parameters/"+p.In+"/"+p.Name,
				fmt.Sprintf("the %s parameter %q became required — callers of %s that omit it are rejected",
					p.In, p.Name, opLabel(o)))
		}
		c.schema(dirReq, "parameters/"+p.In+"/"+p.Name, p.Schema, np.Schema, 0)
	}
	for _, p := range n.Params {
		if _, ok := oldP[p.In+" "+p.Name]; ok {
			continue
		}
		if p.Required {
			c.add(sevBreaking, "request.param.required_added", "parameters/"+p.In+"/"+p.Name,
				fmt.Sprintf("the new %s parameter %q is required — no deployed caller of %s sends it",
					p.In, p.Name, opLabel(o)))
			continue
		}
		c.add(sevAdditive, "request.param.added", "parameters/"+p.In+"/"+p.Name,
			fmt.Sprintf("the optional %s parameter %q is new — callers that omit it behave as before", p.In, p.Name))
	}
}

func (c *breakCmp) diffBody(o, n *oaOp) {
	oldHas := o.Body != nil && !o.Body.Empty
	newHas := n.Body != nil && !n.Body.Empty
	switch {
	case !oldHas && newHas:
		if n.Body.Required {
			c.add(sevBreaking, "request.body.required_added", "requestBody",
				fmt.Sprintf("%s now requires a request body and no deployed caller sends one", opLabel(o)))
		} else {
			c.add(sevAdditive, "request.body.added", "requestBody",
				fmt.Sprintf("%s accepts an optional request body — callers that send none are unaffected", opLabel(o)))
		}
		return
	case oldHas && !newHas:
		c.add(sevWarning, "request.body.removed", "requestBody",
			fmt.Sprintf("%s no longer reads a request body — callers keep sending one and it is discarded", opLabel(o)))
		return
	case !oldHas && !newHas:
		return
	}
	if !o.Body.Required && n.Body.Required {
		c.add(sevBreaking, "request.body.required_added", "requestBody",
			fmt.Sprintf("the request body of %s became required — callers that omit it are rejected", opLabel(o)))
	}
	c.schema(dirReq, "requestBody", o.Body.Schema, n.Body.Schema, 0)
}

func (c *breakCmp) diffResponse(o, n *oaOp) {
	if o.RespStatus != 0 && n.RespStatus != 0 && o.RespStatus != n.RespStatus {
		c.add(sevBreaking, "response.status_changed", "responses",
			fmt.Sprintf("%s answers %d where it answered %d — clients branching on the status take the wrong arm",
				opLabel(o), n.RespStatus, o.RespStatus))
	}
	oldHas := o.Resp != nil && !o.Resp.Empty
	newHas := n.Resp != nil && !n.Resp.Empty
	switch {
	case oldHas && !newHas:
		c.add(sevBreaking, "response.body.removed", "responses/"+status(o.RespStatus),
			fmt.Sprintf("%s no longer returns a body — every consumer parsing its response gets nothing", opLabel(o)))
		return
	case !oldHas && newHas:
		c.add(sevAdditive, "response.body.added", "responses/"+status(n.RespStatus),
			fmt.Sprintf("%s now returns a body — consumers that ignore it are unaffected", opLabel(o)))
		return
	case !oldHas && !newHas:
		return
	}
	c.schema(dirResp, "responses/"+status(n.RespStatus), o.Resp.Schema, n.Resp.Schema, 0)
}

// diffErrors gates x-error-codes, which is not decoration: the generated
// TypeScript builds a per-operation <Fn>Error union out of it, and products
// localize and branch on the codes.
func (c *breakCmp) diffErrors(o, n *oaOp) {
	for _, code := range o.ErrorCodes {
		if !slices.Contains(n.ErrorCodes, code) {
			c.add(sevBreaking, "errors.code_removed", "x-error-codes",
				fmt.Sprintf("%s no longer declares %q — consumers branching or localizing on that code have dead handling and an unmapped failure",
					opLabel(o), code))
		}
	}
	for _, code := range n.ErrorCodes {
		if !slices.Contains(o.ErrorCodes, code) {
			c.add(sevWarning, "errors.code_added", "x-error-codes",
				fmt.Sprintf("%s can now fail with %q — it widens the generated <Fn>Error union and consumers have no arm for it",
					opLabel(o), code))
		}
	}
	for _, st := range o.Errors {
		if !slices.Contains(n.Errors, st) {
			c.add(sevWarning, "errors.status_removed", "responses/"+status(st),
				fmt.Sprintf("%s no longer declares %d — a consumer handling that status now has unreachable code", opLabel(o), st))
		}
	}
	for _, st := range n.Errors {
		if !slices.Contains(o.Errors, st) {
			c.add(sevAdditive, "errors.status_added", "responses/"+status(st),
				fmt.Sprintf("%s declares the new error status %d", opLabel(o), st))
		}
	}
}

// ---- schema walking ----

// schema walks two schemas in lockstep and classifies every difference by
// DIRECTION. See the file header for why the same difference is breaking one
// way and free the other.
func (c *breakCmp) schema(dir, loc string, o, n *oaSchema, depth int) {
	if o == nil || n == nil || depth > breakMaxDepth {
		return
	}
	key := o.Ref + "\x00" + n.Ref
	if o.Ref != "" && n.Ref != "" {
		if c.stack[key] {
			return // a cycle through the same component pair; already walked
		}
		c.stack[key] = true
		defer delete(c.stack, key)
	}
	o, n = nullable(c.oldSpec.Schemas, o), nullable(c.newSpec.Schemas, n)
	if o == nil || n == nil {
		return
	}

	if ot, nt := o.primary(), n.primary(); ot != "" && nt != "" && ot != nt {
		if dir == dirReq {
			c.add(sevBreaking, "request.type_changed", loc,
				fmt.Sprintf("%s takes %s for %q where it took %s — every existing sender's payload is the wrong type",
					opLabel(c.op), nt, field(loc), ot))
		} else {
			c.add(sevBreaking, "response.type_changed", loc,
				fmt.Sprintf("%s returns %s for %q where it returned %s — consumers decode into the old type and fail",
					opLabel(c.op), nt, field(loc), ot))
		}
	}

	c.diffNullable(dir, loc, o, n)
	c.diffEnum(dir, loc, o, n)

	if o.Compose != n.Compose || len(o.Members) != len(n.Members) {
		from, to := o.Compose, n.Compose
		if from == "" {
			from = "a plain schema"
		}
		if to == "" {
			to = "a plain schema"
		}
		change := from + " → " + to
		if from == to {
			change = fmt.Sprintf("%s went from %d to %d members", from, len(o.Members), len(n.Members))
		}
		c.add(sevWarning, "schema.composition_changed", loc,
			fmt.Sprintf("%q in %s changed composition (%s) — members are compared positionally, so this one is flagged rather than proven safe",
				field(loc), opLabel(c.op), change))
	}
	for i := range o.Members {
		if i < len(n.Members) {
			c.schema(dir, fmt.Sprintf("%s/%d", loc, i), o.Members[i], n.Members[i], depth+1)
		}
	}

	c.diffProps(dir, loc, o, n, depth)
	// An array's element schema reads as `items[]`, not `items/items` — the
	// pointer is there to be followed by a human, and doubling the word is the
	// kind of noise that makes a report get skimmed.
	c.schema(dir, loc+"[]", o.Items, n.Items, depth+1)
	c.schema(dir, loc+"/additionalProperties", o.AddProps, n.AddProps, depth+1)
}

func (c *breakCmp) diffNullable(dir, loc string, o, n *oaSchema) {
	switch {
	case !o.Nullable && n.Nullable && dir == dirResp:
		// The null→[] guarantee (contrib/api's normalize.go) means a declared
		// array or object in a 2xx NEVER arrives as null. A consumer therefore
		// wrote `for (const x of r.items)` with no null check, and making the
		// field nullable turns that line into a runtime crash.
		if t := n.primary(); t == "array" || t == "object" {
			c.add(sevBreaking, "response.array.nullable_added", loc,
				fmt.Sprintf("%q in %s can now be null, and a declared %s never could — consumers iterate it with no null check and crash",
					field(loc), opLabel(c.op), t))
			return
		}
		c.add(sevBreaking, "response.field.nullable_added", loc,
			fmt.Sprintf("%q in %s can now be null — consumers that never had to null-check it now must", field(loc), opLabel(c.op)))
	case o.Nullable && !n.Nullable && dir == dirResp:
		c.add(sevAdditive, "response.field.nullable_removed", loc,
			fmt.Sprintf("%q is never null now — a consumer's existing null check is simply dead", field(loc)))
	case o.Nullable && !n.Nullable && dir == dirReq:
		c.add(sevBreaking, "request.field.nullable_removed", loc,
			fmt.Sprintf("%q no longer accepts null — senders of %s passing null are rejected", field(loc), opLabel(c.op)))
	case !o.Nullable && n.Nullable && dir == dirReq:
		c.add(sevAdditive, "request.field.nullable_added", loc,
			fmt.Sprintf("%q accepts null now — senders that never sent it are unaffected", field(loc)))
	}
}

func (c *breakCmp) diffEnum(dir, loc string, o, n *oaSchema) {
	switch {
	case len(o.Enum) > 0 && len(n.Enum) > 0:
		for _, v := range o.Enum {
			if slices.Contains(n.Enum, v) {
				continue
			}
			if dir == dirReq {
				c.add(sevBreaking, "request.enum.value_removed", loc+"/enum",
					fmt.Sprintf("%q no longer accepts %q — senders of %s still passing it are rejected",
						field(loc), v, opLabel(c.op)))
			} else {
				c.add(sevAdditive, "response.enum.value_removed", loc+"/enum",
					fmt.Sprintf("%q never returns %q now — the producer promises less, which no consumer can trip over", field(loc), v))
			}
		}
		for _, v := range n.Enum {
			if slices.Contains(o.Enum, v) {
				continue
			}
			if dir == dirReq {
				c.add(sevAdditive, "request.enum.value_added", loc+"/enum",
					fmt.Sprintf("%q also accepts %q now — every previously valid value still is", field(loc), v))
			} else {
				c.add(sevWarning, "response.enum.value_added", loc+"/enum",
					fmt.Sprintf("%q in %s can now be %q — a consumer switching exhaustively over the old set has no arm for it",
						field(loc), opLabel(c.op), v))
			}
		}
	case len(o.Enum) > 0 && dir == dirReq:
		c.add(sevAdditive, "request.enum.dropped", loc+"/enum",
			fmt.Sprintf("%q dropped its value list — it accepts everything it used to and more", field(loc)))
	case len(o.Enum) > 0:
		c.add(sevWarning, "response.enum.dropped", loc+"/enum",
			fmt.Sprintf("%q dropped its value list — consumers that switched over it lose the closed set they relied on", field(loc)))
	case len(n.Enum) > 0 && dir == dirReq:
		c.add(sevBreaking, "request.enum.introduced", loc+"/enum",
			fmt.Sprintf("%q is now restricted to a value list — senders of %s passing anything outside it are rejected",
				field(loc), opLabel(c.op)))
	case len(n.Enum) > 0:
		c.add(sevAdditive, "response.enum.introduced", loc+"/enum",
			fmt.Sprintf("%q now promises a closed value list — every value it returned before is still in range", field(loc)))
	}
}

func (c *breakCmp) diffProps(dir, loc string, o, n *oaSchema, depth int) {
	oldReq, newReq := map[string]bool{}, map[string]bool{}
	for _, k := range o.Required {
		oldReq[k] = true
	}
	for _, k := range n.Required {
		newReq[k] = true
	}

	for _, k := range o.PropKeys {
		at := loc + "/properties/" + k
		ns, ok := n.Props[k]
		if !ok {
			if dir == dirReq {
				// contrib/api decodes with a plain json.Decoder, so the key is
				// dropped rather than rejected — the honest severity is a
				// warning, and the sentence has to say what actually happens.
				c.add(sevWarning, "request.field.removed", at,
					fmt.Sprintf("senders of %s keep setting %q and the server now discards it silently — no 4xx tells them", opLabel(c.op), k))
			} else {
				c.add(sevBreaking, "response.field.removed", at,
					fmt.Sprintf("readers of %s lose %q — code reading that field gets undefined", opLabel(c.op), k))
			}
			continue
		}
		switch {
		case dir == dirReq && !oldReq[k] && newReq[k]:
			c.add(sevBreaking, "request.field.required_added", at,
				fmt.Sprintf("%q became required — senders of %s that omit it get 422", k, opLabel(c.op)))
		case dir == dirReq && oldReq[k] && !newReq[k]:
			c.add(sevAdditive, "request.field.optional_now", at,
				fmt.Sprintf("%q is optional now — senders that still set it are unaffected", k))
		case dir == dirResp && oldReq[k] && !newReq[k]:
			c.add(sevBreaking, "response.field.optional_now", at,
				fmt.Sprintf("%q is no longer guaranteed in the response — readers of %s that assume it get undefined", k, opLabel(c.op)))
		case dir == dirResp && !oldReq[k] && newReq[k]:
			c.add(sevAdditive, "response.field.required_added", at,
				fmt.Sprintf("%q is always present now — a stronger promise than before", k))
		}
		c.schema(dir, at, o.Props[k], ns, depth+1)
	}

	for _, k := range n.PropKeys {
		if _, ok := o.Props[k]; ok {
			continue
		}
		at := loc + "/properties/" + k
		if dir == dirReq {
			if newReq[k] {
				c.add(sevBreaking, "request.field.required_added", at,
					fmt.Sprintf("the new field %q is required — no deployed sender of %s sets it, so every call gets 422", k, opLabel(c.op)))
			} else {
				c.add(sevAdditive, "request.field.added", at,
					fmt.Sprintf("the optional field %q is new — senders that omit it behave as before", k))
			}
			continue
		}
		c.add(sevAdditive, "response.field.added", at,
			fmt.Sprintf("%q is new in the response — consumers ignoring unknown fields are unaffected", k))
	}
}

func (c *breakCmp) add(sev, kind, loc, detail string) {
	c.found = append(c.found, breakFinding{
		Severity: sev, Kind: kind, Operation: opLabel(c.op),
		Method: c.op.Method, Path: c.op.Path, Location: loc, Detail: detail,
	})
}

// nullable resolves a $ref and normalizes the ONE spelling of nullability
// contrib/api emits. A Go pointer field becomes `anyOf: [X, {"type":"null"}]`
// (schema.go), not `nullable: true` and not a 3.1 type array — so without this
// every optional pointer in the fleet would report as a composition change and
// the real nullability transitions would be invisible.
func nullable(schemas map[string]*oaSchema, s *oaSchema) *oaSchema {
	for i := 0; s != nil && s.Ref != "" && i < breakMaxDepth; i++ {
		s = schemas[s.Ref]
	}
	if s == nil || s.Compose != "anyOf" || len(s.Members) != 2 {
		return s
	}
	// parseSchema turns the bare {"type":"null"} member into Nullable with no
	// types of its own; the OTHER member is the real schema.
	nullAt := -1
	for i, m := range s.Members {
		if m.Ref == "" && len(m.Types) == 0 && m.Nullable && len(m.PropKeys) == 0 {
			nullAt = i
		}
	}
	if nullAt < 0 {
		return s
	}
	inner := nullable(schemas, s.Members[1-nullAt])
	if inner == nil {
		return s
	}
	clone := *inner
	clone.Nullable = true
	return &clone
}

// field is the last segment of a JSON-pointer-ish location — the name the
// reader recognizes. The full pointer stays in its own column, where it is
// there to be followed rather than read as prose.
func field(loc string) string {
	if i := strings.LastIndex(loc, "/"); i >= 0 {
		loc = loc[i+1:]
	}
	return strings.TrimSuffix(loc, "[]")
}

// opLabel is what a report calls an operation: its operationId when it has one
// (the name every generated client uses), else the route.
func opLabel(o *oaOp) string {
	if o.OperationID != "" {
		return o.OperationID
	}
	return o.Method + " " + o.Path
}

func status(n int) string {
	if n == 0 {
		return "2xx"
	}
	return fmt.Sprintf("%d", n)
}

// ---- the report ----

// renderBreaking prints the three sections, grouped by operation. The group
// heading is a TAB-LESS line, which is what ends a tabwriter column block —
// so each operation's findings align as their own tight block instead of every
// column in the section being stretched by the widest location anywhere in it.
func renderBreaking(w io.Writer, rep *breakReport) {
	p := colorFor(w)
	if len(rep.Findings) == 0 {
		fmt.Fprintln(w, "the two documents describe the same contract")
		return
	}
	if rep.Breaking == 0 {
		fmt.Fprintln(w, "no breaking changes")
		fmt.Fprintln(w)
	}
	for _, sec := range []struct {
		sev, title string
		paint      func(string) string
	}{
		{sevBreaking, "BREAKING", p.red},
		{sevWarning, "WARNINGS", p.yellow},
		{sevAdditive, "ADDITIVE", p.dim},
	} {
		var rows []breakFinding
		for _, f := range rep.Findings {
			if f.Severity == sec.sev {
				rows = append(rows, f)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s\n", sec.paint(fmt.Sprintf("%s (%d)", sec.title, len(rows))))
		t := newTable(w)
		group := ""
		for _, f := range rows {
			if head := f.Operation + "  " + f.Method + " " + f.Path; head != group {
				group = head
				t.row("  " + head)
			}
			t.row("    "+f.Kind, f.Location, f.Detail)
		}
		t.flush()
		fmt.Fprintln(w)
	}
}

// breakingText is the mcp tool's body: the same render the CLI prints, plus
// the exit-code fact a tool call has no other way to convey.
func breakingText(oldPath, newPath string) (string, error) {
	oldRaw, err := os.ReadFile(oldPath)
	if err != nil {
		return "", err
	}
	newRaw, err := os.ReadFile(newPath)
	if err != nil {
		return "", err
	}
	oldSpec, err := readSpec(oldPath, oldRaw)
	if err != nil {
		return "", err
	}
	newSpec, err := readSpec(newPath, newRaw)
	if err != nil {
		return "", err
	}
	rep := compareContracts(oldPath, newPath, oldSpec, newSpec)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.Encode(rep)
	return b.String(), nil
}
