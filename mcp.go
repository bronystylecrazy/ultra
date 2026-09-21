package main

// `ultra mcp` is the framework's graph intelligence as a Model Context
// Protocol server, so LLM agents (Claude Code, etc.) call the same tools a
// developer runs: brief a product, explain a diagnostic, list the codes, vet
// a package, dump a product's graph/blast, diff two graphs, walk a fleet, and
// file a field report.
//
//	claude mcp add ultrastack -- ultra mcp
//
// Transport is MCP stdio: newline-delimited JSON-RPC 2.0 on stdin/stdout
// (not LSP Content-Length framing). Hand-rolled with encoding/json, stdlib
// only — the analyzer/lsp house style. Each tool reuses the existing
// command cores (diag.Lesson, runVet, goRunProduct, diffGraphs,
// discoverFleet/fleetStatus) rather than duplicating them.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bronystylecrazy/di/diag"
)

// mcpProtocolVersion is the fallback when the client requests a version we
// do not recognize; we otherwise echo the client's requested version.
const mcpProtocolVersion = "2025-03-26"

// mcpServerVersion is reported in serverInfo — cmd/ultra pins one framework
// version (scaffoldVersion); the server rides the same tag.
const mcpServerVersion = scaffoldVersion

type mcpServer struct {
	in  *bufio.Reader
	out io.Writer
	// canElicit is the client's initialize-time declaration of the
	// elicitation capability (MCP 2025-06-18). Without it, tools that need a
	// human answer FAIL SAFE: they return "pending human approval" — an MCP
	// caller can never supply the human signal itself.
	canElicit bool
	elicitID  int
}

// cmdMCP serves the MCP protocol until stdin closes (clean EOF → exit 0).
func cmdMCP(in io.Reader, out, errW io.Writer) int {
	s := &mcpServer{in: bufio.NewReader(in), out: out}
	if err := s.run(); err != nil {
		fmt.Fprintln(errW, "ultra mcp:", err)
		failVerdict(errW, "mcp", err.Error())
		return 1
	}
	// stdout is the JSON-RPC wire and belongs to the client; the verdict rides
	// stderr, which an MCP host shows as server logs.
	verdict(errW, "mcp", "session closed")
	return 0
}

// ---- wire types (JSON-RPC 2.0) ----

type mcpMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpError        `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- main loop ----

func (s *mcpServer) run() error {
	for {
		msg, err := s.read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch msg.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string                     `json:"protocolVersion"`
				Capabilities    map[string]json.RawMessage `json:"capabilities"`
			}
			json.Unmarshal(msg.Params, &p)
			_, s.canElicit = p.Capabilities["elicitation"]
			version := mcpProtocolVersion
			if p.ProtocolVersion != "" {
				version = p.ProtocolVersion // echo the client's request
			}
			s.reply(msg.ID, map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "ultrastack", "version": mcpServerVersion},
			})
		case "notifications/initialized":
			// Accept and ignore: no response for notifications.
		case "tools/list":
			s.reply(msg.ID, map[string]any{"tools": mcpTools})
		case "tools/call":
			s.handleCall(msg)
		case "ping":
			s.reply(msg.ID, map[string]any{})
		default:
			if msg.ID != nil { // requests get answered; notifications are dropped
				s.replyErr(msg.ID, -32601, "method not found: "+msg.Method)
			}
		}
	}
}

// handleCall dispatches tools/call. Tool failures return an isError result
// (the agent reads the text); only bad params raise a JSON-RPC error.
func (s *mcpServer) handleCall(msg *mcpMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		s.replyErr(msg.ID, -32602, "invalid tools/call params")
		return
	}
	if tool, ok := mcpServerDispatch[p.Name]; ok {
		text, err := tool(s, p.Arguments)
		if err != nil {
			s.reply(msg.ID, mcpErrorContent(err.Error()))
			return
		}
		s.reply(msg.ID, mcpTextContent(text))
		return
	}
	tool, ok := mcpDispatch[p.Name]
	if !ok {
		s.replyErr(msg.ID, -32602, "unknown tool: "+p.Name)
		return
	}
	text, err := tool(p.Arguments)
	if err != nil {
		s.reply(msg.ID, mcpErrorContent(err.Error()))
		return
	}
	s.reply(msg.ID, mcpTextContent(text))
}

func mcpTextContent(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func mcpErrorContent(text string) map[string]any {
	r := mcpTextContent(text)
	r["isError"] = true
	return r
}

// ---- tool definitions ----

// mcpTools is the tools/list payload — descriptions are written for an
// agent: what each returns and when to reach for it.
var mcpTools = []map[string]any{
	{
		"name":        "explain",
		"description": "Returns the full mini-lesson for one ultrastack diagnostic code (what it means, why the design works this way, and a worked fix). Use it when you hit a DIxxxx/UVxxxx error and need the reasoning behind the fix.",
		"inputSchema": objSchema(map[string]any{
			"code": strProp("A diagnostic code, e.g. \"DI0001\"."),
		}, "code"),
	},
	{
		"name":        "codes",
		"description": "Returns the whole diagnostic registry: every code with its one-line summary. Use it to discover which code fits a symptom before calling explain.",
		"inputSchema": objSchema(map[string]any{}),
	},
	{
		"name":        "vet",
		"description": "Runs the ultravet static wiring analyzer over a directory and returns its findings — missing providers, cycles, captive deps — before anything runs. Returns the same JSON array `ultra vet --json` prints: {code, severity, message, file, line, col, endLine, endCol, fixable, secondary[], fix?{title, edits[]}} per finding, sorted by position, [] when clean. A finding with fixable:true carries the exact edits `ultra vet -fix` would apply. (An older ultravet without the flag falls back to the flat go/analysis JSON.) Use it to check a product's dependency graph is sound.",
		"inputSchema": objSchema(map[string]any{
			"dir": strProp("Path to the Go module/product to analyze."),
		}, "dir"),
	},
	{
		"name":        "graph",
		"description": "Returns a product's dependency graph as JSON (fingerprint, summary, components) by running its own `graph --json` toolbox command. Use it to inspect what a product wires and to capture a fingerprint for drift tracking.",
		"inputSchema": objSchema(map[string]any{
			"dir": strProp("Path to the product repo (must build with `go run .`)."),
		}, "dir"),
	},
	{
		"name":        "blast",
		"description": "Returns a product's blast-radius analysis as JSON (what breaks if each component fails, critical vs non-critical, most impactful first). Use it to assess the risk of a component before changing it.",
		"inputSchema": objSchema(map[string]any{
			"dir":       strProp("Path to the product repo (must build with `go run .`)."),
			"component": strProp("Optional component to focus on; omit for the whole graph."),
		}, "dir"),
	},
	{
		"name":        "diff",
		"description": "Semantically diffs two GraphSummary files (added/removed/modified providers and their dependency lists). Use it to review architecture drift between two captured graphs.",
		"inputSchema": objSchema(map[string]any{
			"old": strProp("Path to the baseline GraphSummary file."),
			"new": strProp("Path to the new GraphSummary file."),
		}, "old", "new"),
	},
	{
		"name":        "breaking",
		"description": "Compares two OpenAPI documents and returns every change that would break a consumer of the old one, as JSON: {old, new, breaking, warnings, additive, findings[{severity, kind, operation, method, path, location, detail}]}. Severity is breaking|warning|additive, and each finding's detail names WHO breaks. Call it BEFORE proposing an API change, and after making one: a non-zero `breaking` count means the change cannot ship without a consumer migration.",
		"inputSchema": objSchema(map[string]any{
			"old": strProp("Path to the baseline openapi.json."),
			"new": strProp("Path to the candidate openapi.json."),
		}, "old", "new"),
	},
	{
		"name":        "brief",
		"description": "Returns the orientation pack for one product — module, framework pin, canonical root, wired presets, the operation table from openapi.json, config sections and what binds each, artifact state, recent contract commits. Call it FIRST when you land in an unfamiliar product; it replaces five separate reads.",
		"inputSchema": objSchema(map[string]any{
			"dir":   strProp("Path to the product root (the directory holding go.mod)."),
			"check": map[string]any{"type": "boolean", "description": "Also resolve the latest framework release (needs the network)."},
			"drift": map[string]any{"type": "boolean", "description": "Also regenerate openapi.json and report whether the committed one is stale (builds the product)."},
		}, "dir"),
	},
	{
		"name":        "report",
		"description": "Files a field report (friction|bug|docs|idea) as one JSON line in .ultra-reports.jsonl at the fleet root, stamped with product, framework version, path and time. Use it when framework friction blocks or slows the work — a human triages the file.",
		"inputSchema": objSchema(map[string]any{
			"kind":    strProp("One of friction, bug, docs, idea."),
			"message": strProp("What happened, in one or two sentences."),
			"code":    strProp("Optional diagnostic code the finding is about, e.g. \"DI0001\"."),
			"pkg":     strProp("Optional package the finding is about, e.g. \"contrib/pg\"."),
			"dir":     strProp("Optional product directory the report is filed from; defaults to the server's cwd."),
		}, "kind", "message"),
	},
	{
		"name":        "new_table",
		"description": "Writes the whole persistence slice for one table in an ultrastack product: the goose migration (id + your columns + created_at with NO default — the clock is di.Clock), the five canonical queries (create, get, keyset list, update, delete :execrows), and a Store skeleton in the feature; then runs sqlc generate, whose ultra plugin adds the typed error classifier, the keyset page wrapper and a row factory. Refuses on a module that does not build and on a table any migration already defines. The SQL it writes is yours to edit — it is never regenerated.",
		"inputSchema": objSchema(map[string]any{
			"dir":       strProp("Path to the product root (the directory holding go.mod)."),
			"name":      strProp("The table name, e.g. \"notes\" — lowercase, underscores allowed."),
			"columns":   strProp("The columns as SQL, comma separated, e.g. \"title text not null, body text\". Do NOT include id, created_at or subject."),
			"owned":     map[string]any{"type": "boolean", "description": "Add a subject column and thread ownership through the index and every WHERE."},
			"feature":   strProp("Put the store in this EXISTING feature package; defaults to a feature named after the table."),
			"migration": strProp("Append the DDL to this existing migration (number or filename) instead of adding one."),
			"no_store":  map[string]any{"type": "boolean", "description": "Write the SQL only — no Go."},
		}, "dir", "name", "columns"),
	},
	{
		"name":        "trace",
		"description": "Returns the derived traceability record for a product that has opted into requirements/: each REQ's status (draft/approved) and derived rung (implemented ◐ n/m, verified, validated, blocked-on-human), the recorded-run freshness, and the three gates (dead operation, vanished pinned test, unclaimed operation). Dormant no-op without requirements/. Run it before reporting progress on an opted-in product.",
		"inputSchema": objSchema(map[string]any{
			"dir": strProp("Path to the product root (the directory holding go.mod)."),
		}, "dir"),
	},
	{
		"name":        "new_requirement",
		"description": "Scaffolds requirements/REQ-<id>.md (status: draft) — frontmatter joins (operations[] on governed operationIds, tests[], e2e[], frame) plus the Statement/Rationale/Acceptance prose to fill. Draft the requirement BEFORE building the feature. Refuses to overwrite an existing file.",
		"inputSchema": objSchema(map[string]any{
			"dir":   strProp("Path to the product root."),
			"id":    strProp("The requirement id, e.g. \"SPD-01\" (the file becomes REQ-SPD-01.md)."),
			"title": strProp("A one-line human title."),
		}, "dir", "id", "title"),
	},
	{
		"name":        "req_ask",
		"description": "Parks an open clarification question on a requirement (ask-don't-guess): appends `- open: <question>` under ## Clarifications, which makes ultra trace render the requirement blocked-on-human and makes approval refuse. Use it whenever intent or acceptance is unclear instead of guessing; batch related questions.",
		"inputSchema": objSchema(map[string]any{
			"dir":      strProp("Path to the product root."),
			"id":       strProp("The requirement id."),
			"question": strProp("The question a human must answer."),
		}, "dir", "id", "question"),
	},
	{
		"name":        "req_approve",
		"description": "Requests approval of a requirement (draft → approved — the WP.22 validation record). Approval is a HUMAN act: if the MCP client supports elicitation the human is asked directly and their answer is recorded; otherwise this returns 'pending human approval' and the human runs `ultra req approve` in a terminal. The approver identity always comes from the human's environment (git config user.name) — it cannot be supplied by the caller. Refuses while open clarifications stand.",
		"inputSchema": objSchema(map[string]any{
			"dir": strProp("Path to the product root."),
			"id":  strProp("The requirement id."),
		}, "dir", "id"),
	},
	{
		"name":        "req_change",
		"description": "Appends a change-request entry (WP.03) to a requirement's ## Changes log: date, requested-by, description, impact (cite the breaking tool's output as evidence). The DISPOSITION (accept/reject/defer) is a human act: with client elicitation support the human decides now and the decision is recorded (accepted drops an approved REQ back to draft — the re-approval law); without it the entry is recorded UNDECIDED and trace renders blocked-on-human until `ultra req change --disposition` is run in a terminal.",
		"inputSchema": objSchema(map[string]any{
			"dir":          strProp("Path to the product root."),
			"id":           strProp("The requirement id."),
			"description":  strProp("What is being asked for."),
			"requested_by": strProp("Who asked for the change (the customer, a report id) — the REQUESTER, not the decider."),
			"impact":       strProp("Optional impact analysis — cite `breaking` output, not prose guesswork."),
		}, "dir", "id", "description", "requested_by"),
	},
	{
		"name":        "records_sign",
		"description": "Enters a signature REFERENCE (file / scan / message-id of an externally signed artifact) into a frozen records/<version>/RECORD.md — agreement (WP.02), uat (UAT validation record) or acceptance (WP.01). This is a HUMAN act: with client elicitation support the human confirms and the entry is stamped with the server-side git identity; without it this returns 'pending human signature' and a human runs `ultra records sign` in a terminal. A reference is entered once and cannot be overwritten.",
		"inputSchema": objSchema(map[string]any{
			"dir":     strProp("Path to the product root."),
			"wp":      strProp("One of agreement, uat, acceptance."),
			"ref":     strProp("Pointer to the signed artifact (file path, scan, message-id)."),
			"version": strProp("Optional frozen version (records/<version>/); defaults to the latest frozen one."),
		}, "dir", "wp", "ref"),
	},
	{
		"name":        "fleet_status",
		"description": "Walks a workspace of ultrastack products and returns per-product status as JSON (framework version, graph fingerprint, component count, drift vs the saved baseline). Use it for a fleet-wide view of versions and drift.",
		"inputSchema": objSchema(map[string]any{
			"workspace": strProp("Path to the workspace root containing product repos."),
		}, "workspace"),
	},
}

// mcpDispatch maps a tool name to its handler; arguments arrive as the raw
// JSON of the call's "arguments" object.
var mcpDispatch = map[string]func(json.RawMessage) (string, error){
	"explain":         toolExplain,
	"codes":           toolCodes,
	"vet":             toolVet,
	"graph":           toolGraph,
	"blast":           toolBlast,
	"diff":            toolDiff,
	"breaking":        toolBreaking,
	"brief":           toolBrief,
	"report":          toolReport,
	"new_table":       toolNewTable,
	"fleet_status":    toolFleetStatus,
	"trace":           toolTrace,
	"new_requirement": toolNewRequirement,
	"req_ask":         toolReqAsk,
}

// mcpServerDispatch holds the tools that may need to ELICIT the human —
// they take the server so they can reach the wire. Checked first.
var mcpServerDispatch = map[string]func(*mcpServer, json.RawMessage) (string, error){
	"req_approve":  toolReqApprove,
	"req_change":   toolReqChange,
	"records_sign": toolRecordsSign,
}

// ---- tool handlers (each reuses an existing command core) ----

func toolExplain(raw json.RawMessage) (string, error) {
	var a struct {
		Code string `json:"code"`
	}
	json.Unmarshal(raw, &a)
	if a.Code == "" {
		return "", fmt.Errorf("explain: code is required")
	}
	code := diag.Code(strings.ToUpper(a.Code))
	lesson, ok := diag.Lesson(code)
	if !ok {
		if isPresetCode(code) {
			return "", fmt.Errorf("%s is a preset code — this server links no preset. "+
				"Run `./app explain %s` in the product that wires it", code, code)
		}
		return "", fmt.Errorf("unknown code %q — call the codes tool for the registry", a.Code)
	}
	return lesson, nil
}

func toolCodes(json.RawMessage) (string, error) {
	return codesText(), nil
}

func toolVet(raw json.RawMessage) (string, error) {
	var a struct {
		Dir string `json:"dir"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" {
		return "", fmt.Errorf("vet: dir is required")
	}
	// The agent gets EXACTLY what `ultra vet --json` prints — same flag, same
	// runVet, same marshaling in analyzer/ultravet — so the tool and the CLI
	// cannot describe the same findings differently.
	//
	// Detection is version-agnostic: run it, and accept the result only when
	// stdout parses as a JSON array — the emitter always prints at least "[]",
	// so an older binary that does not know the flag (garbage/empty stdout)
	// transparently falls through to the flat go/analysis -json below.
	var diag, diagErr strings.Builder
	if code := runVet(a.Dir, []string{"--json", "./..."}, &diag, &diagErr); code >= 0 && isJSONArray(diag.String()) {
		return diag.String(), nil
	}
	var buf strings.Builder
	if code, _ := runAnalyzer(a.Dir, []string{"-json", "./..."}, &buf, &buf); code == -1 {
		return "", fmt.Errorf("vet: could not run the analyzer (install ultravet, or set GOPRIVATE for `go run`)")
	}
	return buf.String(), nil
}

// isJSONArray reports whether s is a JSON array — the marker of ultravet's
// -diagjson emitter (vs. an older binary's error output on stdout).
func isJSONArray(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "[") && json.Valid([]byte(s))
}

func toolGraph(raw json.RawMessage) (string, error) {
	var a struct {
		Dir string `json:"dir"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" {
		return "", fmt.Errorf("graph: dir is required")
	}
	return goRunProduct(a.Dir, "graph", "--json")
}

func toolBlast(raw json.RawMessage) (string, error) {
	var a struct {
		Dir       string `json:"dir"`
		Component string `json:"component"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" {
		return "", fmt.Errorf("blast: dir is required")
	}
	args := []string{"blast", "--json"}
	if a.Component != "" {
		args = append(args, a.Component)
	}
	return goRunProduct(a.Dir, args...)
}

func toolDiff(raw json.RawMessage) (string, error) {
	var a struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	json.Unmarshal(raw, &a)
	if a.Old == "" || a.New == "" {
		return "", fmt.Errorf("diff: old and new are required")
	}
	oldB, err := os.ReadFile(a.Old)
	if err != nil {
		return "", err
	}
	newB, err := os.ReadFile(a.New)
	if err != nil {
		return "", err
	}
	report, _, _ := diffGraphs(string(oldB), string(newB))
	return report, nil
}

func toolBreaking(raw json.RawMessage) (string, error) {
	var a struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	json.Unmarshal(raw, &a)
	if a.Old == "" || a.New == "" {
		return "", fmt.Errorf("breaking: old and new are required")
	}
	return breakingText(a.Old, a.New)
}

func toolBrief(raw json.RawMessage) (string, error) {
	var a struct {
		Dir   string `json:"dir"`
		Check bool   `json:"check"`
		Drift bool   `json:"drift"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" {
		return "", fmt.Errorf("brief: dir is required")
	}
	return briefText(a.Dir, a.Check, a.Drift)
}

func toolReport(raw json.RawMessage) (string, error) {
	var a struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Pkg     string `json:"pkg"`
		Dir     string `json:"dir"`
	}
	json.Unmarshal(raw, &a)
	return fileReport(a.Kind, a.Message, a.Code, a.Pkg, a.Dir)
}

// toolNewTable drives cmdNewTable itself rather than reimplementing it, so
// the agent gets the same refusals, the same files and the same verdict a
// developer sees — including the build gate.
func toolNewTable(raw json.RawMessage) (string, error) {
	var a struct {
		Dir       string `json:"dir"`
		Name      string `json:"name"`
		Columns   string `json:"columns"`
		Owned     bool   `json:"owned"`
		Feature   string `json:"feature"`
		Migration string `json:"migration"`
		NoStore   bool   `json:"no_store"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" || a.Name == "" || a.Columns == "" {
		return "", fmt.Errorf("new_table: dir, name and columns are required")
	}
	args := []string{a.Name, a.Columns}
	if a.Owned {
		args = append(args, "--owned")
	}
	if a.Feature != "" {
		args = append(args, "--feature", a.Feature)
	}
	if a.Migration != "" {
		args = append(args, "--migration", a.Migration)
	}
	if a.NoStore {
		args = append(args, "--no-store")
	}
	var out, errW strings.Builder
	if code := cmdNewTable(append(args, a.Dir), &out, &errW); code != 0 {
		return "", fmt.Errorf("%s%s", out.String(), errW.String())
	}
	return out.String() + errW.String(), nil
}

func toolFleetStatus(raw json.RawMessage) (string, error) {
	var a struct {
		Workspace string `json:"workspace"`
	}
	json.Unmarshal(raw, &a)
	if a.Workspace == "" {
		return "", fmt.Errorf("fleet_status: workspace is required")
	}
	repos := discoverFleet(a.Workspace)
	if len(repos) == 0 {
		return "", fmt.Errorf("no ultrastack products under %s", a.Workspace)
	}
	var buf strings.Builder
	fleetStatus(repos, a.Workspace, false, true, &buf, &buf)
	return buf.String(), nil
}

// ---- schema helpers ----

// objSchema builds a JSON Schema object with the given properties; the
// trailing names are marked required.
func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// ---- transport: newline-delimited JSON-RPC 2.0 ----

func (s *mcpServer) read() (*mcpMessage, error) {
	for {
		line, err := s.in.ReadBytes('\n')
		if err != nil {
			if err == io.EOF && len(strings.TrimSpace(string(line))) > 0 {
				return s.decode(line) // last line without a trailing newline
			}
			return nil, err
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue // tolerate blank lines between messages
		}
		return s.decode(line)
	}
}

func (s *mcpServer) decode(line []byte) (*mcpMessage, error) {
	var msg mcpMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (s *mcpServer) reply(id *json.RawMessage, result any) {
	s.write(mcpMessage{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *mcpServer) replyErr(id *json.RawMessage, code int, message string) {
	s.write(mcpMessage{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}})
}

func (s *mcpServer) write(msg mcpMessage) {
	body, _ := json.Marshal(msg)
	s.out.Write(append(body, '\n'))
}
