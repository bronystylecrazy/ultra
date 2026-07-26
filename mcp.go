package main

// `ultra mcp` is the framework's graph intelligence as a Model Context
// Protocol server, so LLM agents (Claude Code, etc.) call the same tools a
// developer runs: explain a diagnostic, list the codes, vet a package,
// dump a product's graph/blast, diff two graphs, walk a fleet.
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

	"github.com/bronystylecrazy/ultrastack/di/diag"
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
}

// cmdMCP serves the MCP protocol until stdin closes (clean EOF → exit 0).
func cmdMCP(in io.Reader, out, errW io.Writer) int {
	s := &mcpServer{in: bufio.NewReader(in), out: out}
	if err := s.run(); err != nil {
		fmt.Fprintln(errW, "ultra mcp:", err)
		return 1
	}
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
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(msg.Params, &p)
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
	"explain":      toolExplain,
	"codes":        toolCodes,
	"vet":          toolVet,
	"graph":        toolGraph,
	"blast":        toolBlast,
	"diff":         toolDiff,
	"fleet_status": toolFleetStatus,
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
	if code := runAnalyzer(a.Dir, []string{"-json", "./..."}, &buf, &buf); code == -1 {
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
	report, _ := diffGraphs(string(oldB), string(newB))
	return report, nil
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
