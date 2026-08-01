package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpSession boots a server over pipes and initializes it with (or without)
// the client-side elicitation capability.
func mcpSession(t *testing.T, elicit bool) *mcpClient {
	t.Helper()
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	go cmdMCP(serverIn, serverOut, io.Discard)
	c := &mcpClient{t: t, in: bufio.NewReader(clientIn), out: clientOut}
	caps := map[string]any{}
	if elicit {
		caps["elicitation"] = map[string]any{}
	}
	c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": caps})
	c.notify("notifications/initialized", map[string]any{})
	t.Cleanup(func() { clientOut.Close() })
	return c
}

// mcpReqProduct is a product with one draft requirement and a real approver
// identity in git config — the SERVER-side identity the tools stamp.
func mcpReqProduct(t *testing.T) string {
	t.Helper()
	gitcfg := filepath.Join(t.TempDir(), "gitconfig")
	must(t, os.WriteFile(gitcfg, []byte("[user]\n\tname = MCP Human\n"), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", gitcfg)
	t.Setenv("GIT_CONFIG_SYSTEM", gitcfg)
	dir := writeProduct(t, oldMain, nil)
	var out, errW strings.Builder
	if code := cmdNewRequirement([]string{"M-1", "MCP-born requirement", dir}, &out, &errW); code != 0 {
		t.Fatalf("scaffold: %s", errW.String())
	}
	return dir
}

// THE FAILS-SAFE RULE: a client with no elicitation capability gets "pending
// human approval" — no error, no state change, and no way to pass an
// identity that would make it happen.
func TestMCPReqApprovePendingWithoutElicitation(t *testing.T) {
	dir := mcpReqProduct(t)
	c := mcpSession(t, false)

	text, isErr := contentText(t, c.call("req_approve", map[string]any{"dir": dir, "id": "M-1"}))
	if isErr {
		t.Fatalf("pending is a state, not an error: %s", text)
	}
	for _, want := range []string{"pending human approval", "ultra req approve M-1", "stays draft"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if r, err := loadRequirement(dir, "M-1"); err != nil || r.Status != "draft" {
		t.Errorf("the file must stay draft: %+v (%v)", r, err)
	}
	// The schema itself must offer no identity door.
	for _, tool := range mcpTools {
		if tool["name"] == "req_approve" {
			if raw, _ := json.Marshal(tool["inputSchema"]); strings.Contains(string(raw), "by") &&
				strings.Contains(string(raw), "identity") {
				t.Errorf("req_approve's schema must not accept an identity: %s", raw)
			}
		}
	}
}

// The elicitation path: the server asks the HUMAN, an accept records the
// approval with the server-side git identity, and the wire carries a real
// elicitation/create request in between.
func TestMCPReqApproveElicitsTheHuman(t *testing.T) {
	dir := mcpReqProduct(t)
	c := mcpSession(t, true)

	// Fire the call, then expect the server's elicitation REQUEST first.
	c.write(map[string]any{"jsonrpc": "2.0", "id": 100, "method": "tools/call",
		"params": map[string]any{"name": "req_approve", "arguments": map[string]any{"dir": dir, "id": "M-1"}}})
	elic := c.recv()
	if elic.Method != "elicitation/create" || elic.ID == nil {
		t.Fatalf("expected elicitation/create, got %+v", elic)
	}
	var params struct {
		Message         string          `json:"message"`
		RequestedSchema json.RawMessage `json:"requestedSchema"`
	}
	must(t, json.Unmarshal(elic.Params, &params))
	for _, want := range []string{"REQ-M-1", "MCP-born requirement", `"MCP Human"`} {
		if !strings.Contains(params.Message+string(params.RequestedSchema), want) {
			t.Errorf("the elicit prompt must name the act (%q missing): %s", want, params.Message)
		}
	}

	// The human accepts.
	c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*elic.ID),
		"result": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}})
	resp := c.recv()
	text, isErr := contentText(t, resp)
	if isErr || !strings.Contains(text, "approved REQ-M-1") || !strings.Contains(text, "by MCP Human") {
		t.Fatalf("approve after accept: isErr=%v %s", isErr, text)
	}
	r, err := loadRequirement(dir, "M-1")
	if err != nil || r.Status != "approved" || r.ApprovedBy != "MCP Human" {
		t.Errorf("the stamp must carry the server-side identity: %+v (%v)", r, err)
	}
}

// A decline records nothing — the requirement stays draft.
func TestMCPReqApproveDeclineStaysDraft(t *testing.T) {
	dir := mcpReqProduct(t)
	c := mcpSession(t, true)

	c.write(map[string]any{"jsonrpc": "2.0", "id": 101, "method": "tools/call",
		"params": map[string]any{"name": "req_approve", "arguments": map[string]any{"dir": dir, "id": "M-1"}}})
	elic := c.recv()
	c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*elic.ID),
		"result": map[string]any{"action": "decline"}})
	text, isErr := contentText(t, c.recv())
	if isErr || !strings.Contains(text, "stays draft") {
		t.Fatalf("decline: isErr=%v %s", isErr, text)
	}
	if r, _ := loadRequirement(dir, "M-1"); r == nil || r.Status != "draft" {
		t.Error("declined approval must leave the file draft")
	}
}

// req_change without elicitation: the entry lands UNDECIDED (a recorded
// question, not a decided one) and the tool says who finishes it and how.
func TestMCPReqChangeUndecidedWithoutElicitation(t *testing.T) {
	dir := mcpReqProduct(t)
	c := mcpSession(t, false)

	text, isErr := contentText(t, c.call("req_change", map[string]any{
		"dir": dir, "id": "M-1", "description": "also capture lane", "requested_by": "customer"}))
	if isErr {
		t.Fatalf("recording a CR is not an error: %s", text)
	}
	if !strings.Contains(text, "UNDECIDED") {
		t.Errorf("the tool must say the disposition is pending:\n%s", text)
	}
	r, err := loadRequirement(dir, "M-1")
	if err != nil || len(r.Changes) != 1 || r.Changes[0].Disposition != "" {
		t.Errorf("entry: %+v (%v)", r, err)
	}
}

// req_change WITH elicitation: the human picks the disposition; accepted on
// an approved requirement executes the draft-drop through the same core the
// CLI uses.
func TestMCPReqChangeElicitsDisposition(t *testing.T) {
	dir := mcpReqProduct(t)
	var out, errW strings.Builder
	if code := cmdReq([]string{"approve", "M-1", dir, "--by", "Owner"}, &out, &errW); code != 0 {
		t.Fatalf("pre-approve: %s", errW.String())
	}
	c := mcpSession(t, true)

	c.write(map[string]any{"jsonrpc": "2.0", "id": 102, "method": "tools/call",
		"params": map[string]any{"name": "req_change", "arguments": map[string]any{
			"dir": dir, "id": "M-1", "description": "tolerance percent", "requested_by": "customer"}}})
	elic := c.recv()
	if elic.Method != "elicitation/create" {
		t.Fatalf("expected elicitation, got %+v", elic)
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*elic.ID),
		"result": map[string]any{"action": "accept", "content": map[string]any{"disposition": "accepted"}}})
	text, isErr := contentText(t, c.recv())
	if isErr || !strings.Contains(text, "drops back to DRAFT") {
		t.Fatalf("accepted disposition: isErr=%v %s", isErr, text)
	}
	r, err := loadRequirement(dir, "M-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "draft" || r.Changes[0].Disposition != "accepted" || !strings.Contains(r.Changes[0].DecidedBy, "MCP Human") {
		t.Errorf("the decision record: status=%s %+v", r.Status, r.Changes)
	}
}

// The read-side tools ride the plain dispatch: trace and req_ask.
func TestMCPTraceAndAskTools(t *testing.T) {
	dir := mcpReqProduct(t)
	c := mcpSession(t, false)

	text, isErr := contentText(t, c.call("req_ask", map[string]any{
		"dir": dir, "id": "M-1", "question": "which lanes?"}))
	if isErr || !strings.Contains(text, "blocked-on-human") {
		t.Fatalf("req_ask: isErr=%v %s", isErr, text)
	}
	text, isErr = contentText(t, c.call("trace", map[string]any{"dir": dir}))
	if isErr || !strings.Contains(text, "REQ-M-1") || !strings.Contains(text, "blocked-on-human (1 open question)") {
		t.Fatalf("trace tool: isErr=%v\n%s", isErr, text)
	}
	// And dormancy through the tool: a product with no requirements/.
	plain := writeProduct(t, oldMain, nil)
	text, isErr = contentText(t, c.call("trace", map[string]any{"dir": plain}))
	if isErr || !strings.Contains(text, "dormant") {
		t.Fatalf("dormant trace tool: isErr=%v %s", isErr, text)
	}
}
