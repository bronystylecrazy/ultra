package main

// The requirement verbs. `ultra new requirement` scaffolds the file;
// `ultra req approve` is the ONLY path to status: approved — the WP.22
// validation record captured as data at the moment of the human act;
// `ultra req ask` parks a question instead of a guess; `ultra req change`
// appends a WP.03 change-request entry, and an ACCEPTED disposition drops
// the requirement back to draft (the re-approval law).
//
// Each verb has a core function (reqApprove, reqAsk, reqChange) the MCP
// tools reuse, so the CLI and an agent cannot behave differently.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// cmdNewRequirement implements `ultra new requirement <id> "<title>" [dir]`.
func cmdNewRequirement(args []string, out, errW io.Writer) int {
	var id, title, dir string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("new").find("requirement"), a)
		case id == "":
			id = a
		case title == "":
			title = a
		case dir == "":
			dir = a
		}
	}
	if id == "" || title == "" {
		ultraTree().find("new").find("requirement").help(errW)
		return 2
	}
	if dir == "" {
		dir = "."
	}
	if !reqIDRe.MatchString(id) {
		fmt.Fprintf(errW, "requirement id %q must be letters, digits, dots, dashes or underscores\n", id)
		failVerdict(errW, "new requirement "+id, "bad id")
		return 1
	}
	msg, err := newRequirement(dir, id, title)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "new requirement "+id, err.Error())
		return 1
	}
	fmt.Fprint(out, msg)
	verdict(errW, "new requirement "+id, "created "+reqPath(id)+" (draft)")
	return 0
}

func newRequirement(dir, id, title string) (string, error) {
	rel := reqPath(id)
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
		return "", fmt.Errorf("%s already exists — refusing to overwrite a record", rel)
	}
	data := struct{ ID, Title, Date string }{id, title, today()}
	if err := renderAll(dir, map[string]string{"compliance/requirement.md.tmpl": rel}, data); err != nil {
		return "", err
	}
	return fmt.Sprintf("created %s (status: draft)\n\nNext:\n"+
		"  fill Statement / Rationale / Acceptance, and the operations[] joins\n"+
		"  ultra req ask %s \"<question>\"   # park what you would otherwise guess\n"+
		"  ultra req approve %s            # the HUMAN act that makes it buildable\n"+
		"  ultra trace                      # the derived status, any time\n", rel, id, id), nil
}

// cmdReq dispatches `ultra req approve|ask|change`.
func cmdReq(args []string, out, errW io.Writer) int {
	verb, rest := args[0], args[1:]
	var id, dir, question string
	var by, description, requestedBy, impact, disposition, decidedBy string
	flagVal := func(i *int) string {
		if *i+1 < len(rest) {
			*i++
			return rest[*i]
		}
		return ""
	}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--by":
			by = flagVal(&i)
		case a == "--description":
			description = flagVal(&i)
		case a == "--requested-by":
			requestedBy = flagVal(&i)
		case a == "--impact":
			impact = flagVal(&i)
		case a == "--disposition":
			disposition = flagVal(&i)
		case a == "--decided-by":
			decidedBy = flagVal(&i)
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("req").find(verb), a)
		case id == "":
			id = a
		case verb == "ask" && question == "":
			question = a
		default:
			dir = a
		}
	}
	if id == "" || (verb == "ask" && question == "") {
		ultraTree().find("req").find(verb).help(errW)
		return 2
	}
	if dir == "" {
		dir = "."
	}

	var msg string
	var err error
	switch verb {
	case "approve":
		if by == "" {
			by = gitUserName(dir)
		}
		msg, err = reqApprove(dir, id, by)
	case "ask":
		msg, err = reqAsk(dir, id, question)
	case "change":
		if description == "" {
			fmt.Fprintln(errW, "ultra req change: --description is required — a change request with no description is not a record")
			return 2
		}
		if requestedBy == "" {
			requestedBy = gitUserName(dir)
		}
		if disposition != "" && decidedBy == "" {
			decidedBy = gitUserName(dir)
		}
		msg, err = reqChangeEntry(dir, id, reqChange{
			Date: today(), RequestedBy: requestedBy, Description: description,
			Impact: impact, Disposition: disposition, DecidedBy: decidedBy,
		})
	}
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "req "+verb+" "+id, firstLineOf(err.Error()))
		return 1
	}
	fmt.Fprint(out, msg)
	verdict(errW, "req "+verb+" "+id, firstLineOf(msg))
	return 0
}

// reqApprove is the one human-declared transition. It refuses while open
// questions stand (ask-don't-guess has teeth), and it refuses with no
// identity: no human signal, no approval — the fails-safe rule.
func reqApprove(dir, id, by string) (string, error) {
	r, err := loadRequirement(dir, id)
	if err != nil {
		return "", err
	}
	if len(r.Open) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "REQ-%s has %s — approval is refused until a human answers them:\n", id, count(len(r.Open), "open question"))
		for _, q := range r.Open {
			fmt.Fprintf(&b, "  open: %s\n", q)
		}
		b.WriteString("write each answer into ## Clarifications as a dated Q&A, then approve")
		return "", fmt.Errorf("%s", b.String())
	}
	if by == "" {
		return "", fmt.Errorf("no approver identity — set `git config user.name` or pass --by; an approval nobody signed is not a validation record")
	}
	if r.Status == "approved" {
		return fmt.Sprintf("REQ-%s is already approved by %s (%s) — nothing to do\n", id, r.ApprovedBy, r.ApprovedDate), nil
	}
	r.Status, r.ApprovedBy, r.ApprovedDate = "approved", by, today()
	if err := r.save(); err != nil {
		return "", err
	}
	return fmt.Sprintf("approved REQ-%s %q — by %s, %s (the WP.22 validation record, as data)\n", id, r.Title, by, r.ApprovedDate), nil
}

// reqAsk appends an `- open:` clarification — the parked question that makes
// trace render blocked-on-human instead of letting an agent guess.
func reqAsk(dir, id, question string) (string, error) {
	r, err := loadRequirement(dir, id)
	if err != nil {
		return "", err
	}
	r.Body = appendToSection(r.Body, "Clarifications", fmt.Sprintf("- open: %s (asked %s)", question, today()))
	if err := r.save(); err != nil {
		return "", err
	}
	return fmt.Sprintf("parked on REQ-%s: %q\ntrace now renders it blocked-on-human; write the answer into ## Clarifications as a dated Q&A\n", id, question), nil
}

// reqChangeEntry appends a ## Changes entry. An ACCEPTED disposition drops
// an approved requirement back to draft and clears the stamp — a materially
// changed requirement is a requirement nobody has approved yet.
func reqChangeEntry(dir, id string, c reqChange) (string, error) {
	r, err := loadRequirement(dir, id)
	if err != nil {
		return "", err
	}
	entry := fmt.Sprintf("- %s — requested-by: %s\n  description: %s", c.Date, c.RequestedBy, c.Description)
	if c.Impact != "" {
		entry += "\n  impact: " + c.Impact
	}
	if c.Disposition != "" {
		entry += fmt.Sprintf("\n  disposition: %s\n  decided-by: %s (%s)", c.Disposition, c.DecidedBy, c.Date)
	}
	r.Body = appendToSection(r.Body, "Changes", entry)
	msg := fmt.Sprintf("recorded change request on REQ-%s (%s)\n", id, c.Date)
	switch {
	case c.Disposition == "":
		msg += "no disposition yet — trace renders blocked-on-human until a human decides (accept/reject/defer)\n"
	case c.Disposition == "accepted" && r.Status == "approved":
		r.Status, r.ApprovedBy, r.ApprovedDate = "draft", "", ""
		msg += fmt.Sprintf("disposition: accepted — REQ-%s drops back to DRAFT; re-approval required (ultra req approve %s)\n", id, id)
	default:
		msg += "disposition: " + c.Disposition + " — recorded (a rejection is knowledge; git alone holds no diff for it)\n"
	}
	if err := r.save(); err != nil {
		return "", err
	}
	return msg, nil
}

// appendToSection adds a line at the end of a `## <name>` section, creating
// the section at the end of the body when it does not exist yet.
func appendToSection(body, name, entry string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "## "+name {
			start = i
			break
		}
	}
	if start < 0 {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return body + "\n## " + name + "\n\n" + entry + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	// Insert before the next heading, trimming trailing blanks first so the
	// entry lands under the last one instead of after a gap.
	insert := end
	for insert > start+1 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	out := append([]string{}, lines[:insert]...)
	out = append(out, strings.Split(entry, "\n")...)
	if end < len(lines) {
		out = append(out, "")
		out = append(out, lines[end:]...)
	} else {
		out = append(out, lines[insert:]...)
	}
	return strings.Join(out, "\n")
}
