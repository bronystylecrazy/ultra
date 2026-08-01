package main

// The requirements tools on `ultra mcp`, and the elicitation seam they need.
//
// SCOUT RESULT, recorded where it matters: this server is hand-rolled stdlib
// JSON-RPC (no MCP library), and the MCP spec since 2025-06-18 defines
// server-initiated `elicitation/create` — so support is OURS to implement,
// gated on the client declaring the capability at initialize. When the
// client declared it, approve/change-disposition ELICIT the human and record
// the human's answer. When it did not, the fails-safe rule holds: the tool
// answers "pending human approval — run `ultra req ...` in a terminal", the
// file stays unapproved/undecided, and NOTHING an MCP caller sends can stand
// in for the human signal — identity always comes from the server side's
// `git config user.name`, never from tool arguments.

import (
	"encoding/json"
	"fmt"
)

func toolTrace(raw json.RawMessage) (string, error) {
	var a struct {
		Dir string `json:"dir"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" {
		return "", fmt.Errorf("trace: dir is required")
	}
	return traceText(a.Dir)
}

func toolNewRequirement(raw json.RawMessage) (string, error) {
	var a struct {
		Dir, ID, Title string
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" || a.ID == "" || a.Title == "" {
		return "", fmt.Errorf("new_requirement: dir, id and title are required")
	}
	if !reqIDRe.MatchString(a.ID) {
		return "", fmt.Errorf("new_requirement: id %q must be letters, digits, dots, dashes or underscores", a.ID)
	}
	return newRequirement(a.Dir, a.ID, a.Title)
}

func toolReqAsk(raw json.RawMessage) (string, error) {
	var a struct {
		Dir, ID, Question string
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" || a.ID == "" || a.Question == "" {
		return "", fmt.Errorf("req_ask: dir, id and question are required")
	}
	return reqAsk(a.Dir, a.ID, a.Question)
}

// toolReqApprove: the human answers, or nothing happens. Note there is
// deliberately no identity argument in the schema — see the file comment.
func toolReqApprove(s *mcpServer, raw json.RawMessage) (string, error) {
	var a struct {
		Dir, ID string
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" || a.ID == "" {
		return "", fmt.Errorf("req_approve: dir and id are required")
	}
	// Parse first so open-question refusals and parse errors reach the agent
	// BEFORE a human is interrupted.
	r, err := loadRequirement(a.Dir, a.ID)
	if err != nil {
		return "", err
	}
	if len(r.Open) > 0 {
		return "", fmt.Errorf("REQ-%s has %s — a human must answer them (dated Q&A in ## Clarifications) before approval can even be asked for", a.ID, count(len(r.Open), "open question"))
	}
	if r.Status == "approved" {
		return fmt.Sprintf("REQ-%s is already approved by %s (%s)", a.ID, r.ApprovedBy, r.ApprovedDate), nil
	}
	if !s.canElicit {
		return fmt.Sprintf("pending human approval — this MCP client declared no elicitation capability, "+
			"so no human can be asked from here. A human runs:\n\n  ultra req approve %s\n\n"+
			"in a terminal (identity stamps from their git config user.name). The requirement stays draft until then.", a.ID), nil
	}
	by := gitUserName(a.Dir)
	if by == "" {
		return "", fmt.Errorf("no approver identity on this machine — set `git config user.name`; an approval nobody signed is not a validation record")
	}
	action, _, err := s.elicitCreate(
		fmt.Sprintf("Approve requirement REQ-%s — %q? This stamps %q and today's date as the validation record (draft → approved).", a.ID, r.Title, by),
		map[string]any{"confirm": map[string]any{"type": "boolean", "description": "true to approve"}},
		"confirm")
	if err != nil {
		return "", err
	}
	if action != "accept" {
		return fmt.Sprintf("the human answered %q — REQ-%s stays draft", action, a.ID), nil
	}
	return reqApprove(a.Dir, a.ID, by)
}

// toolReqChange records the entry always; the DISPOSITION only lands when a
// human decided it through elicitation — otherwise the entry stays
// undecided and trace blocks on it.
func toolReqChange(s *mcpServer, raw json.RawMessage) (string, error) {
	var a struct {
		Dir, ID, Description, Impact string
		RequestedBy                  string `json:"requested_by"`
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" || a.ID == "" || a.Description == "" || a.RequestedBy == "" {
		return "", fmt.Errorf("req_change: dir, id, description and requested_by are required")
	}
	c := reqChange{Date: today(), RequestedBy: a.RequestedBy, Description: a.Description, Impact: a.Impact}
	if s.canElicit {
		action, content, err := s.elicitCreate(
			fmt.Sprintf("Change request on REQ-%s: %s\nDisposition? (accepted drops an approved requirement back to draft)", a.ID, a.Description),
			map[string]any{"disposition": map[string]any{
				"type": "string", "enum": []string{"accepted", "rejected", "deferred"},
				"description": "the decision to record",
			}},
			"disposition")
		if err != nil {
			return "", err
		}
		if action == "accept" {
			if d, _ := content["disposition"].(string); d != "" {
				c.Disposition, c.DecidedBy = d, gitUserName(a.Dir)
				if c.DecidedBy == "" {
					return "", fmt.Errorf("no decider identity on this machine — set `git config user.name`")
				}
			}
		}
	}
	msg, err := reqChangeEntry(a.Dir, a.ID, c)
	if err != nil {
		return "", err
	}
	if c.Disposition == "" {
		msg += "\n(no elicitation-confirmed decision — the entry is UNDECIDED; a human runs " +
			"`ultra req change` with --disposition, or answers the elicit next time)"
	}
	return msg, nil
}

// elicitCreate sends an `elicitation/create` request to the client and
// blocks until the human's answer comes back. Interleaved pings are served;
// other notifications are dropped — the session belongs to this question
// until it is answered.
func (s *mcpServer) elicitCreate(message string, props map[string]any, required ...string) (action string, content map[string]any, err error) {
	s.elicitID++
	id := json.RawMessage(fmt.Sprintf("\"ultra-elicit-%d\"", s.elicitID))
	s.write(mcpMessage{JSONRPC: "2.0", ID: &id, Method: "elicitation/create", Params: mustJSON(map[string]any{
		"message":         message,
		"requestedSchema": objSchema(props, required...),
	})})
	for {
		msg, rerr := s.read()
		if rerr != nil {
			return "", nil, fmt.Errorf("elicitation: the session closed before the human answered")
		}
		switch {
		case msg.Method == "ping":
			s.reply(msg.ID, map[string]any{})
		case msg.Method != "":
			// Another client request mid-elicitation: refuse it politely
			// rather than deadlocking two questions.
			if msg.ID != nil {
				s.replyErr(msg.ID, -32603, "busy: an elicitation is pending")
			}
		case msg.ID != nil && string(*msg.ID) == string(id):
			if msg.Error != nil {
				return "", nil, fmt.Errorf("elicitation failed: %s", msg.Error.Message)
			}
			raw, _ := json.Marshal(msg.Result)
			var res struct {
				Action  string         `json:"action"`
				Content map[string]any `json:"content"`
			}
			if err := json.Unmarshal(raw, &res); err != nil || res.Action == "" {
				return "", nil, fmt.Errorf("elicitation: unreadable answer %s", raw)
			}
			return res.Action, res.Content, nil
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
