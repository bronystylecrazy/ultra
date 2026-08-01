package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// mcpClient is a minimal MCP client for the test conversation: one JSON
// object per line, both directions.
type mcpClient struct {
	t   *testing.T
	in  *bufio.Reader
	out io.Writer
	id  int
}

func (c *mcpClient) request(method string, params any) mcpMessage {
	c.t.Helper()
	c.id++
	c.write(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	return c.recv()
}

func (c *mcpClient) notify(method string, params any) {
	c.t.Helper()
	c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *mcpClient) call(name string, args any) mcpMessage {
	return c.request("tools/call", map[string]any{"name": name, "arguments": args})
}

func (c *mcpClient) write(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(c.out, "%s\n", b)
}

func (c *mcpClient) recv() mcpMessage {
	c.t.Helper()
	line, err := c.in.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("recv: %v", err)
	}
	var msg mcpMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		c.t.Fatalf("recv unmarshal: %v (%q)", err, line)
	}
	return msg
}

// contentText pulls the text out of a tools/call result.
func contentText(t *testing.T, msg mcpMessage) (text string, isError bool) {
	t.Helper()
	raw, _ := json.Marshal(msg.Result)
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("content unmarshal: %v (%s)", err, raw)
	}
	if len(r.Content) == 0 {
		t.Fatalf("no content: %s", raw)
	}
	return r.Content[0].Text, r.IsError
}

func TestMCPConversation(t *testing.T) {
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- cmdMCP(serverIn, serverOut, io.Discard) }()
	c := &mcpClient{t: t, in: bufio.NewReader(clientIn), out: clientOut}

	// initialize → echoes the requested protocol version, names the server.
	init := c.request("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	raw, _ := json.Marshal(init.Result)
	if !strings.Contains(string(raw), `"protocolVersion":"2025-06-18"`) ||
		!strings.Contains(string(raw), `"name":"ultrastack"`) ||
		!strings.Contains(string(raw), `"tools":{}`) {
		t.Fatalf("initialize result: %s", raw)
	}

	// notifications/initialized — accepted, no response.
	c.notify("notifications/initialized", map[string]any{})

	// tools/list → all nine tools, each with an inputSchema.
	list := c.request("tools/list", map[string]any{})
	var lr struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	lraw, _ := json.Marshal(list.Result)
	json.Unmarshal(lraw, &lr)
	got := map[string]bool{}
	for _, tool := range lr.Tools {
		if tool.Description == "" || len(tool.InputSchema) == 0 {
			t.Errorf("tool %q missing description/schema", tool.Name)
		}
		got[tool.Name] = true
	}
	for _, want := range []string{"explain", "codes", "vet", "graph", "blast", "diff", "breaking", "brief", "report", "fleet_status"} {
		if !got[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
	if len(lr.Tools) != 11 {
		t.Fatalf("want 11 tools, got %d", len(lr.Tools))
	}

	// tools/call explain → the DI0001 lesson comes back as text.
	text, isErr := contentText(t, c.call("explain", map[string]any{"code": "DI0001"}))
	if isErr || !strings.Contains(text, "DI0001") || !strings.Contains(text, "no provider") {
		t.Fatalf("explain: isErr=%v text=%q", isErr, text)
	}

	// tools/call codes → the registry, several codes present.
	text, isErr = contentText(t, c.call("codes", map[string]any{}))
	if isErr {
		t.Fatalf("codes isError: %q", text)
	}
	for _, want := range []string{"DI0001", "DI0101", "DI0203"} {
		if !strings.Contains(text, want) {
			t.Errorf("codes missing %s", want)
		}
	}

	// tools/call with a bad code → isError result, not a protocol error.
	text, isErr = contentText(t, c.call("explain", map[string]any{"code": "DI9999"}))
	if !isErr || !strings.Contains(text, "unknown code") {
		t.Fatalf("bad code: isErr=%v text=%q", isErr, text)
	}

	// tools/call unknown tool → JSON-RPC -32602 (bad params).
	unknown := c.call("nope", map[string]any{})
	if unknown.Error == nil || unknown.Error.Code != -32602 {
		t.Fatalf("unknown tool: %+v", unknown.Error)
	}

	// unknown method → JSON-RPC -32601.
	bad := c.request("no/such/method", map[string]any{})
	if bad.Error == nil || bad.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", bad.Error)
	}

	// ping → empty result.
	if ping := c.request("ping", map[string]any{}); ping.Error != nil {
		t.Fatalf("ping errored: %+v", ping.Error)
	}

	// EOF on stdin ends the server cleanly (exit 0).
	clientOut.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("clean EOF must exit 0, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit on EOF")
	}
}

// TestMCPGraphTool exercises a real product toolbox through the graph tool:
// examples/speedwatch answers `go run . graph --json` offline and fast.
func TestMCPGraphTool(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run against a real product")
	}
	dir, err := filepath.Abs("../../examples/speedwatch")
	if err != nil {
		t.Fatal(err)
	}
	out, err := toolGraph(json.RawMessage(fmt.Sprintf(`{"dir":%q}`, dir)))
	if err != nil {
		t.Fatalf("graph tool: %v", err)
	}
	var g struct {
		Fingerprint string `json:"fingerprint"`
		Components  []any  `json:"components"`
	}
	if err := json.Unmarshal([]byte(out), &g); err != nil {
		t.Fatalf("graph output not JSON: %v\n%s", err, out)
	}
	if g.Fingerprint == "" || len(g.Components) == 0 {
		t.Fatalf("graph missing fingerprint/components: %s", out)
	}
}

// fakeUltravet installs a shell-script `ultravet` on PATH for the duration of
// a test and returns a restore func. runVet finds it via exec.LookPath.
func fakeUltravet(t *testing.T, script string) func() {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "ultravet")
	if err := os.WriteFile(bin, []byte(vetStub(script)), 0o755); err != nil {
		t.Fatal(err)
	}
	old := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+old)
	return func() { os.Setenv("PATH", old) }
}

// TestMCPVetStructuredJSON asserts the vet tool asks for exactly what
// `ultra vet --json` prints — the analyzer's -format=json document — and that
// the findings flow through to the MCP result unchanged. One marshaling
// implementation: the tool never reshapes what the analyzer emitted.
func TestMCPVetStructuredJSON(t *testing.T) {
	restore := fakeUltravet(t, `#!/bin/sh
case "$1" in
-format=json) echo '[{"code":"DI0001","severity":"error","message":"error[DI0001]: no provider for *x.Config (needed by NewDB)","file":"x.go","line":10,"col":2,"endLine":10,"endCol":7,"fixable":true,"secondary":[{"file":"x.go","line":3,"col":6,"endLine":3,"endCol":17,"message":"this parameter of NewDB created the need"}],"fix":{"title":"Register NewConfig, which provides *x.Config","edits":[{"file":"x.go","line":9,"col":12,"endLine":9,"endCol":12,"startOffset":120,"endOffset":120,"newText":"di.Provide(NewConfig), "}]}}]'; exit 1 ;;
-json) echo '{"x":{"ultravet":[]}}'; exit 0 ;;
esac
`)
	defer restore()

	out, err := toolVet(json.RawMessage(fmt.Sprintf(`{"dir":%q}`, t.TempDir())))
	if err != nil {
		t.Fatalf("vet: %v", err)
	}
	var findings []struct {
		Code      string `json:"code"`
		Severity  string `json:"severity"`
		Line      int    `json:"line"`
		EndCol    int    `json:"endCol"`
		Fixable   bool   `json:"fixable"`
		Secondary []struct {
			Message string `json:"message"`
		} `json:"secondary"`
		Fix *struct {
			Title string `json:"title"`
			Edits []struct {
				NewText string `json:"newText"`
			} `json:"edits"`
		} `json:"fix"`
	}
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		t.Fatalf("vet output is not a finding array: %v\n%s", err, out)
	}
	if len(findings) != 1 {
		t.Fatalf("structured shape not preserved: %s", out)
	}
	f := findings[0]
	if f.Code != "DI0001" || f.Severity != "error" || f.Line != 10 || f.EndCol != 7 ||
		!f.Fixable || len(f.Secondary) != 1 {
		t.Fatalf("structured shape not preserved: %s", out)
	}
	if f.Fix == nil || len(f.Fix.Edits) != 1 || f.Fix.Edits[0].NewText == "" {
		t.Fatalf("the fix edits must reach the agent: %s", out)
	}
}

// TestMCPVetFallsBackToJSON asserts that an older ultravet that does not know
// the structured format transparently falls back to the flat go/analysis
// -json output — which runVet must NOT rewrite on the way out.
func TestMCPVetFallsBackToJSON(t *testing.T) {
	restore := fakeUltravet(t, `#!/bin/sh
case "$1" in
-format=json) echo 'flag provided but not defined: -format=json' 1>&2; exit 2 ;;
-json) echo '{"pkg":{"ultravet":[{"posn":"x.go:1:1","message":"error[DI0001]: no provider"}]}}'; exit 0 ;;
esac
`)
	defer restore()

	out, err := toolVet(json.RawMessage(fmt.Sprintf(`{"dir":%q}`, t.TempDir())))
	if err != nil {
		t.Fatalf("vet: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("expected flat -json fallback (object), got an array: %s", out)
	}
	if !strings.Contains(out, `"posn"`) || !strings.Contains(out, "ultravet") {
		t.Fatalf("expected flat go/analysis -json passthrough, got: %s", out)
	}
}

// TestMCPToolValidation covers the argument-validation error paths that do
// not need a product on disk.
func TestMCPToolValidation(t *testing.T) {
	cases := []struct {
		name string
		fn   func(json.RawMessage) (string, error)
	}{
		{"vet", toolVet},
		{"graph", toolGraph},
		{"blast", toolBlast},
		{"brief", toolBrief},
		{"report", toolReport},
		{"fleet_status", toolFleetStatus},
	}
	for _, tc := range cases {
		if _, err := tc.fn(json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s: missing required arg must error", tc.name)
		}
	}
	if _, err := toolDiff(json.RawMessage(`{"old":"x"}`)); err == nil {
		t.Error("diff: missing new must error")
	}
	// The kind set is closed on the tool side too, or the inbox an agent fills
	// is a tag soup the CLI would have refused.
	if _, err := toolReport(json.RawMessage(`{"kind":"grumble","message":"x"}`)); err == nil {
		t.Error("report: an unknown kind must error")
	}
}

// The two new tools go through the SAME cores the CLI does — brief renders the
// developer's page, report writes the developer's line.
func TestMCPBriefAndReportTools(t *testing.T) {
	dir := briefFixture(t)
	text, err := toolBrief(json.RawMessage(`{"dir":` + quoted(dir) + `}`))
	if err != nil {
		t.Fatalf("brief tool: %v", err)
	}
	for _, want := range []string{"shop — example.com/shop", "WIRED (4)", "OPERATIONS (3)", "CONFIG (4)"} {
		if !strings.Contains(text, want) {
			t.Errorf("brief tool missing %q:\n%s", want, text)
		}
	}

	t.Chdir(dir)
	text, err = toolReport(json.RawMessage(`{"kind":"docs","message":"the preset page still shows Product()","pkg":"contrib/pg"}`))
	if err != nil {
		t.Fatalf("report tool: %v", err)
	}
	if !strings.Contains(text, "product root") || !strings.Contains(text, reportFile) {
		t.Errorf("report tool must name where it filed: %q", text)
	}
	entries, err := readReports(filepath.Join(dir, reportFile))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v (%v)", entries, err)
	}
	if entries[0].Kind != "docs" || entries[0].Pkg != "contrib/pg" || entries[0].Product != "example.com/shop" {
		t.Errorf("entry = %+v", entries[0])
	}
}

// quoted is a JSON string literal for the hand-built argument objects above —
// a Windows path is full of backslashes, and %q is not JSON.
func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
