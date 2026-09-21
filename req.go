package main

// requirements/REQ-<id>.md is the ISO 29110 WP.13 requirements specification,
// one file per requirement: YAML frontmatter carrying the JOINS (operations[]
// keyed on governed operationIds, tests[], e2e[], frame) and a prose body
// carrying the SRS content (statement / rationale / acceptance). The body may
// hold `## Clarifications` (dated Q&A; `- open:` items are unanswered — the
// ask-don't-guess protocol) and `## Changes` (the WP.03 change-request log,
// rejections included). Code carries NO requirement ids, ever — the
// operationId is the join key, and `ultra trace` derives every status.
//
// The parser is deliberately STRICT: these files are records an auditor
// walks, so a malformed one is refused with a positioned, caret-quality
// error (the REQ01xx family below), never half-read.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bronystylecrazy/di/diag"
)

const requirementsDir = "requirements"

// reqNow is stubbed in tests; every stamp in a requirement file is a date,
// not a timestamp — the record names a day, like a signature line does.
var reqNow = time.Now

func today() string { return reqNow().Format("2006-01-02") }

// reqFile is one parsed requirements/REQ-<id>.md.
type reqFile struct {
	Path  string `json:"path"`
	ID    string `json:"id"`
	Title string `json:"title"`
	// Status is draft|approved — the ONLY hand-of-human field. Everything
	// beyond approved (implemented/verified/validated) is derived by trace.
	Status       string   `json:"status"`
	ApprovedBy   string   `json:"approvedBy,omitempty"`
	ApprovedDate string   `json:"approvedDate,omitempty"`
	Operations   []string `json:"operations"`
	E2E          []string `json:"e2e"`
	Tests        []string `json:"tests"`
	Frame        string   `json:"frame,omitempty"`

	// Body is everything after the closing fence, verbatim — the verbs edit
	// sections inside it but never reflow prose that is somebody's.
	Body string `json:"-"`

	Open          []string    `json:"open,omitempty"` // unanswered clarifications
	Answered      int         `json:"answered,omitempty"`
	Changes       []reqChange `json:"changes,omitempty"`
	HasAcceptance bool        `json:"hasAcceptance"`
}

// reqChange is one `## Changes` entry — the WP.03 change request as data. An
// entry without a disposition is UNDECIDED and trace renders the requirement
// blocked-on-human; a rejected entry stays forever (a rejection is knowledge,
// and git history alone records no diff for it).
type reqChange struct {
	Date        string `json:"date"`
	RequestedBy string `json:"requestedBy"`
	Description string `json:"description"`
	Impact      string `json:"impact,omitempty"`
	Disposition string `json:"disposition,omitempty"` // accepted|rejected|deferred, "" = undecided
	DecidedBy   string `json:"decidedBy,omitempty"`
}

// reqError is a positioned parse failure: file:line:col, the offending source
// line, a caret under the column, and the explain pointer — the same
// anatomy every diagnostic in this repo teaches through.
type reqError struct {
	path string
	line int // 1-based
	col  int // 1-based
	src  string
	msg  string
}

func (e *reqError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:%d:%d: %s\n", e.path, e.line, e.col, e.msg)
	fmt.Fprintf(&b, "    %s\n", e.src)
	fmt.Fprintf(&b, "    %s^\n", strings.Repeat(" ", e.col-1))
	b.WriteString("more: ultra explain REQ0101")
	return b.String()
}

// reqIDRe is the id grammar: what fits in a filename and a sentence.
var reqIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var reqScalarKeys = []string{"id", "title", "status", "frame"}
var reqListKeys = []string{"operations", "e2e", "tests"}

// reqPath is where the requirement for an id lives, in slash form.
func reqPath(id string) string {
	return requirementsDir + "/REQ-" + id + ".md"
}

// parseRequirement reads one requirement file, strictly.
func parseRequirement(path string, raw []byte) (*reqFile, error) {
	lines := strings.Split(string(raw), "\n")
	fail := func(line, col int, msg string) error {
		src := ""
		if line-1 < len(lines) {
			src = lines[line-1]
		}
		return &reqError{path: path, line: line, col: col, src: src, msg: msg}
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fail(1, 1, "a requirement opens with a `---` frontmatter fence")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, fail(len(lines), 1, "the frontmatter never closes — a second `---` fence is missing")
	}

	r := &reqFile{Path: path, Operations: []string{}, E2E: []string{}, Tests: []string{}}
	listKey := ""   // the list currently accepting `- item` lines
	nestedKey := "" // "approved", accepting indented by:/date:
	for i := 1; i < end; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case strings.HasPrefix(trimmed, "- "):
			if listKey == "" {
				return nil, fail(i+1, indent+1, "a list item with no list key above it")
			}
			item := reqScalar(strings.TrimPrefix(trimmed, "- "))
			if item == "" {
				return nil, fail(i+1, indent+1, "an empty list item")
			}
			r.appendList(listKey, item)
		case indent > 0:
			if nestedKey != "approved" {
				return nil, fail(i+1, indent+1, "an indented key belongs under `approved:` and nothing else")
			}
			key, val, ok := strings.Cut(trimmed, ":")
			if !ok {
				return nil, fail(i+1, indent+1, "expected `by:` or `date:` under `approved:`")
			}
			switch key {
			case "by":
				r.ApprovedBy = reqScalar(val)
			case "date":
				r.ApprovedDate = reqScalar(val)
			default:
				return nil, fail(i+1, indent+1, fmt.Sprintf("unknown key %q under `approved:` — it holds `by` and `date`", key))
			}
		default:
			key, val, ok := strings.Cut(trimmed, ":")
			if !ok {
				return nil, fail(i+1, 1, "expected `key: value`")
			}
			listKey, nestedKey = "", ""
			val = strings.TrimSpace(val)
			switch {
			case key == "approved":
				nestedKey = "approved"
			case contains(reqListKeys, key):
				switch {
				case val == "":
					listKey = key
				case val == "[]":
					// the empty list, explicit
				case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
					for _, item := range strings.Split(strings.Trim(val, "[]"), ",") {
						if s := reqScalar(item); s != "" {
							r.appendList(key, s)
						}
					}
				default:
					return nil, fail(i+1, len(key)+3, fmt.Sprintf("%s is a list — use `[]`, `[a, b]`, or `- item` lines", key))
				}
			case contains(reqScalarKeys, key):
				s := reqScalar(val)
				switch key {
				case "id":
					r.ID = s
				case "title":
					r.Title = s
				case "status":
					r.Status = s
				case "frame":
					r.Frame = s
				}
			default:
				msg := fmt.Sprintf("unknown frontmatter key %q", key)
				known := append(append([]string{"approved"}, reqScalarKeys...), reqListKeys...)
				best, bestD := "", 3
				for _, k := range known {
					if d := editDistance(strings.ToLower(key), k); d < bestD {
						best, bestD = k, d
					}
				}
				if best != "" {
					msg += fmt.Sprintf(" — did you mean %q?", best)
				}
				return nil, fail(i+1, 1, msg)
			}
		}
	}

	// The cross-field laws: the record must be internally coherent or it is
	// not a record.
	base := filepath.Base(path)
	switch {
	case r.ID == "":
		return nil, fail(1, 1, "the frontmatter declares no id")
	case r.Title == "":
		return nil, fail(1, 1, "the frontmatter declares no title")
	case base != "REQ-"+r.ID+".md":
		return nil, fail(1, 1, fmt.Sprintf("id %q does not match the filename — this file must be REQ-%s.md", r.ID, r.ID))
	case r.Status != "draft" && r.Status != "approved":
		return nil, fail(1, 1, fmt.Sprintf("status %q is not one of draft|approved — implemented/verified/validated are DERIVED by `ultra trace`, never written", r.Status))
	case r.Status == "approved" && (r.ApprovedBy == "" || r.ApprovedDate == ""):
		return nil, fail(1, 1, "status is approved but the approved{by,date} stamp is missing — only `ultra req approve` writes this transition")
	case r.Status == "draft" && r.ApprovedBy != "":
		return nil, fail(1, 1, "a draft carries an approved{} stamp — an accepted change drops status to draft AND clears the stamp; re-approve it")
	}

	r.Body = strings.Join(lines[end+1:], "\n")
	if err := r.parseBody(path, lines, end+1); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *reqFile) appendList(key, item string) {
	switch key {
	case "operations":
		r.Operations = append(r.Operations, item)
	case "e2e":
		r.E2E = append(r.E2E, item)
	case "tests":
		r.Tests = append(r.Tests, item)
	}
}

// reqScalar strips surrounding quotes from a frontmatter value.
func reqScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// parseBody reads the sections trace joins on. offset is the 0-based index of
// the first body line, so errors still point at the real file position.
func (r *reqFile) parseBody(path string, lines []string, offset int) error {
	section := ""
	sectionHasProse := map[string]bool{}
	var change *reqChange
	inComment := false
	for i := offset; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if inComment {
			inComment = !strings.Contains(trimmed, "-->")
			continue
		}
		if strings.HasPrefix(trimmed, "<!--") {
			inComment = !strings.Contains(trimmed, "-->")
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			section, change = strings.TrimPrefix(trimmed, "## "), nil
			continue
		}
		if trimmed == "" {
			continue
		}
		fail := func(col int, msg string) error {
			return &reqError{path: path, line: i + 1, col: col, src: line, msg: msg}
		}
		switch section {
		case "Clarifications":
			switch {
			case strings.HasPrefix(trimmed, "- open:"):
				r.Open = append(r.Open, strings.TrimSpace(strings.TrimPrefix(trimmed, "- open:")))
			case strings.HasPrefix(trimmed, "- "):
				r.Answered++
			}
		case "Changes":
			switch {
			case strings.HasPrefix(trimmed, "- "):
				date, who, ok := strings.Cut(strings.TrimPrefix(trimmed, "- "), " — requested-by:")
				if !ok {
					return fail(1, "a change entry opens `- <date> — requested-by: <who>` (ultra req change writes one)")
				}
				r.Changes = append(r.Changes, reqChange{Date: strings.TrimSpace(date), RequestedBy: strings.TrimSpace(who)})
				change = &r.Changes[len(r.Changes)-1]
			case change != nil && strings.HasPrefix(line, "  "):
				key, val, ok := strings.Cut(trimmed, ":")
				if !ok {
					return fail(3, "a change detail line is `key: value`")
				}
				val = strings.TrimSpace(val)
				switch key {
				case "description":
					change.Description = val
				case "impact":
					change.Impact = val
				case "disposition":
					if val != "accepted" && val != "rejected" && val != "deferred" {
						return fail(3, fmt.Sprintf("disposition %q is not one of accepted|rejected|deferred — an entry with none is UNDECIDED and trace blocks on it", val))
					}
					change.Disposition = val
				case "decided-by":
					change.DecidedBy = val
				default:
					return fail(3, fmt.Sprintf("unknown change detail %q — description, impact, disposition, decided-by", key))
				}
			default:
				return fail(1, "prose inside ## Changes — the log holds entries only; rationale goes in the entry's description")
			}
		case "Acceptance":
			if !strings.HasPrefix(trimmed, "TODO") {
				sectionHasProse["Acceptance"] = true
			}
		}
	}
	for i, c := range r.Changes {
		if c.Disposition != "" && c.DecidedBy == "" {
			return &reqError{path: path, line: 1, col: 1, src: lines[0],
				msg: fmt.Sprintf("change entry %d (%s) has a disposition but no decided-by — a disposition is a human act and the record names the human", i+1, c.Date)}
		}
	}
	r.HasAcceptance = sectionHasProse["Acceptance"]
	return nil
}

// renderRequirement writes the canonical frontmatter back. The body rides
// verbatim; only the machine-owned half is regenerated.
func renderRequirement(r *reqFile) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", r.ID)
	fmt.Fprintf(&b, "title: %s\n", r.Title)
	fmt.Fprintf(&b, "status: %s\n", r.Status)
	if r.ApprovedBy != "" {
		fmt.Fprintf(&b, "approved:\n  by: %s\n  date: %s\n", r.ApprovedBy, r.ApprovedDate)
	}
	for _, l := range []struct {
		key   string
		items []string
	}{{"operations", r.Operations}, {"e2e", r.E2E}, {"tests", r.Tests}} {
		if len(l.items) == 0 {
			fmt.Fprintf(&b, "%s: []\n", l.key)
			continue
		}
		fmt.Fprintf(&b, "%s:\n", l.key)
		for _, item := range l.items {
			fmt.Fprintf(&b, "  - %s\n", item)
		}
	}
	fmt.Fprintf(&b, "frame: %q\n", r.Frame)
	b.WriteString("---\n")
	b.WriteString(r.Body)
	return b.String()
}

func (r *reqFile) save() error {
	return os.WriteFile(r.Path, []byte(renderRequirement(r)), 0o644)
}

// loadRequirement reads and parses one requirement by id.
func loadRequirement(dir, id string) (*reqFile, error) {
	path := filepath.Join(dir, filepath.FromSlash(reqPath(id)))
	raw, err := os.ReadFile(path)
	if err != nil {
		msg := fmt.Sprintf("no %s here", reqPath(id))
		if ids := requirementIDs(dir); len(ids) > 0 {
			msg += " — this product has: " + strings.Join(ids, ", ")
		} else {
			msg += " — `ultra new requirement <id> \"<title>\"` writes the first one"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return parseRequirement(filepath.ToSlash(filepath.Join(dir, filepath.FromSlash(reqPath(id)))), raw)
}

// requirementIDs lists the ids on disk, sorted.
func requirementIDs(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, requirementsDir))
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "REQ-") && strings.HasSuffix(name, ".md") {
			ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(name, "REQ-"), ".md"))
		}
	}
	sort.Strings(ids)
	return ids
}

// loadRequirements parses every REQ file, collecting EVERY parse failure —
// one run reports the whole work list, the house rule for diagnostics.
func loadRequirements(dir string) ([]*reqFile, []error) {
	var reqs []*reqFile
	var errs []error
	for _, id := range requirementIDs(dir) {
		r, err := loadRequirement(dir, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		reqs = append(reqs, r)
	}
	return reqs, errs
}

// hasRequirements is the PRESENCE ACTIVATION check: no requirements/
// directory means the whole phase-2 machinery is dormant and every verb
// behaves exactly as it did before the directory existed.
func hasRequirements(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, requirementsDir))
	return err == nil && fi.IsDir()
}

// gitUserName is the approver identity: the human at THIS terminal. It is
// never accepted from an MCP tool caller — no human signal, no approval.
func gitUserName(dir string) string {
	out, err := git(dir, "config", "user.name")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// The REQ family, registered like any preset's codes: `ultra codes` lists
// them, `ultra explain REQ0102` teaches them.
func init() {
	diag.Register("REQ0101", `REQ0101 — a requirement file is malformed

A requirements/REQ-<id>.md could not be parsed. These files are audit
records (the ISO 29110 WP.13 requirements specification), so the parser is
strict on purpose: a half-read record is worse than a refused one.

The error above this lesson points at the exact file, line and column. The
shape it expects: a YAML frontmatter fence holding id, title,
status (draft|approved), an approved{by, date} stamp written only by
`+"`ultra req approve`"+`, the join lists operations/e2e/tests, and frame;
then the prose body, whose ## Clarifications entries are `+"`- open:`"+`
questions or dated Q&As and whose ## Changes entries open
`+"`- <date> — requested-by: <who>`"+` with indented detail lines.

Fix the named line. Never invent new frontmatter keys or status values —
implemented/verified/validated are DERIVED by `+"`ultra trace`"+`, not written.
The full format is references/trace.md in the vendored skill.`)

	diag.Register("REQ0102", `REQ0102 — a requirement claims an operation the contract does not declare

A REQ file's operations[] names an operationId that is not in the committed
openapi.json. The join key between requirements and code is the GOVERNED
operationId (code carries no REQ ids, ever), so a dead operationId means the
traceability record is claiming work that does not exist — the exact drift
a WP.21 traceability record exists to catch.

Either the operation was renamed (`+"`ultra breaking --against`"+` would have
named that) — update the REQ's operations[] in the same landing — or the
contract is stale: run `+"`task contracts`"+` and commit openapi.json, then
re-run `+"`ultra trace`"+`. Never park a wrong join to silence the error.`)

	diag.Register("REQ0103", `REQ0103 — a pinned test vanished from the compiled test list

A REQ file's tests[] pins a Go test name that the recorded run does not
contain. A pinned test is the requirement's verification evidence; a name
that no longer compiles means "verified" would be claimed on a test nobody
runs — so this is an error, not a warning.

trace checks against the RECORDED full run (.ultra/test.json — what
`+"`task test`"+` tees) by `+"`go test -list`"+` semantics; `+"`ultra trace --list`"+`
re-derives the list fresh, at the cost of compiling every test package.
Either the test was renamed — update tests[] in the same landing as the
rename — or it was deleted, in which case the requirement has lost its
verification and needs a new pinned test before trace can call it verified.`)

	diag.Register("REQ0104", `REQ0104 — an operation is claimed by no requirement

An operationId in the committed openapi.json appears in no REQ file's
operations[]. In a product that has opted into requirements/ this is a
warning: shipped surface that no approved requirement asked for is scope
nobody governs.

Claim it — add it to the requirement it serves, in the same landing — or
exempt it: requirements/exempt.txt holds one pattern per line (an exact
operationId or a prefix glob like `+"`auth.*`"+`) for infrastructure surface
(healthz, ops, the auth scaffold routes) that is real but is not a product
requirement. The exemption file is reviewed like any other record.`)
}
