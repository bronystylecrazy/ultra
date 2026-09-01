package main

// `ultra trace [dir]` is the WP.21 traceability record, DERIVED — never
// hand-kept. It joins three things it only ever READS:
//
//	requirements/REQ-*.md        the claims (operations[], tests[], e2e[])
//	openapi.json                 the committed contract (operationIds +
//	                             x-error-codes, for stub detection — the same
//	                             logic as ultra brief's STUBS section)
//	.ultra/test.json, e2e.json   the RECORDED runs `task test` / `task e2e` tee
//
// The seam is deliberate: trace never runs tests and never fakes freshness.
// A status a gate has not proven renders unknown, not green — each record's
// age is shown, and a record older than the newest source file is STALE.
//
// PRESENCE-ACTIVATED: a product without requirements/ gets a clean no-op and
// zero behavior change. That dormancy is a hard covenant, tested as such.

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	testRecordFile = ".ultra/test.json"
	e2eRecordFile  = ".ultra/e2e.json"
	exemptFile     = "requirements/exempt.txt"
)

type tracePack struct {
	Product      string        `json:"product"`
	Dir          string        `json:"dir"`
	Contract     bool          `json:"contract"`
	Requirements []traceReq    `json:"requirements"`
	Records      []traceRecord `json:"records"`
	Errors       []string      `json:"errors"`
	Warnings     []string      `json:"warnings"`
	Notes        []string      `json:"notes,omitempty"`
}

type traceReq struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`  // frontmatter: draft|approved
	Derived string `json:"derived"` // the machine's verdict, with its reason
	Ops     string `json:"ops,omitempty"`
}

type traceRecord struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Fresh   bool   `json:"fresh"`
	Age     string `json:"age,omitempty"`
	Note    string `json:"note,omitempty"`
}

func cmdTrace(args []string, out, errW io.Writer) int {
	dir, jsonOut, list := ".", false, false
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--list":
			list = true
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("trace"), a)
		default:
			dir = a
		}
	}

	// THE DORMANCY COVENANT: no requirements/, no machinery. Say so, touch
	// nothing, exit clean — a fleet product that never opted in must be able
	// to run this in CI forever at zero cost.
	if !hasRequirements(dir) {
		fmt.Fprintf(out, "no %s/ in %s — trace is dormant.\n"+
			"Opting in is an act: ultra compliance init (the class-2 templates),\n"+
			"or ultra new requirement <id> \"<title>\" for the first requirement.\n",
			requirementsDir, displayDir(dir))
		verdict(errW, "trace", "dormant — no requirements/")
		return 0
	}

	p, parseErrs := buildTrace(dir, list)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(p)
	} else {
		renderTrace(out, p, parseErrs)
	}
	for _, e := range parseErrs {
		fmt.Fprintln(errW, e)
	}
	if len(parseErrs) > 0 || len(p.Errors) > 0 {
		failVerdict(errW, "trace", count(len(parseErrs)+len(p.Errors), "error")+", "+count(len(p.Warnings), "warning"))
		return 1
	}
	detail := fmt.Sprintf("%s traced", count(len(p.Requirements), "requirement"))
	if len(p.Warnings) > 0 {
		detail += ", " + count(len(p.Warnings), "warning")
	}
	verdict(errW, "trace", detail)
	return 0
}

func buildTrace(dir string, list bool) (*tracePack, []error) {
	p := &tracePack{Product: productName(dir), Dir: displayDir(dir),
		Errors: []string{}, Warnings: []string{}}
	reqs, parseErrs := loadRequirements(dir)

	// The contract, exactly as brief reads it: the COMMITTED document.
	ops := map[string]string{} // operationId → stub code ("" = live)
	if raw, err := os.ReadFile(filepath.Join(dir, contractFile)); err == nil {
		if spec, err := readSpec(contractFile, raw); err != nil {
			p.Notes = append(p.Notes, contractFile+" is present but unreadable: "+err.Error())
		} else {
			p.Contract = true
			for _, item := range spec.Paths {
				for _, op := range item.Ops {
					if op.OperationID != "" {
						ops[op.OperationID] = stubCode(op)
					}
				}
			}
		}
	} else {
		p.Notes = append(p.Notes, "no "+contractFile+" — `go test ./...` bootstraps it; operation joins render unknown")
	}

	srcTime := newestSourceTime(dir)
	test := readTestRecord(dir, srcTime, p)
	e2e := readE2ERecord(dir, srcTime, p)
	if list {
		compiled, err := goTestList(dir)
		if err != nil {
			p.Notes = append(p.Notes, "--list could not compile the test list: "+err.Error())
		} else {
			test.ran, test.authoritative = compiled, true
		}
	}

	claimedOps := map[string]bool{}
	for _, r := range reqs {
		row := traceReq{ID: r.ID, Title: r.Title, Status: r.Status}
		row.Derived, row.Ops = deriveStatus(r, ops, test, e2e, p)
		for _, o := range r.Operations {
			claimedOps[o] = true
		}
		p.Requirements = append(p.Requirements, row)
	}

	// Gate 3: unclaimed operations — a WARNING with the infra-exemption
	// escape (requirements/exempt.txt), so healthz/ops/auth surface is
	// exemptable without noise.
	exempt := loadExempt(dir)
	var unclaimed []string
	for id := range ops {
		if !claimedOps[id] && !matchesExempt(id, exempt) {
			unclaimed = append(unclaimed, id)
		}
	}
	sort.Strings(unclaimed)
	for _, id := range unclaimed {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"operation %q is claimed by no requirement (REQ0104) — claim it, or exempt it in %s", id, exemptFile))
	}
	return p, parseErrs
}

// deriveStatus computes one requirement's rung on the ladder. The ladder is
// strict: implemented (all operations live) → verified (pinned tests passed
// in a fresh recorded run) → validated (pinned e2e titles passed + acceptance
// prose present). A human blocker overrides the whole ladder.
func deriveStatus(r *reqFile, ops map[string]string, test *testRecord, e2e *e2eRecord, p *tracePack) (derived, opsCol string) {
	undecided := 0
	for _, c := range r.Changes {
		if c.Disposition == "" {
			undecided++
		}
	}
	if len(r.Open) > 0 || undecided > 0 {
		var parts []string
		if len(r.Open) > 0 {
			parts = append(parts, count(len(r.Open), "open question"))
		}
		if undecided > 0 {
			parts = append(parts, count(undecided, "undecided change"))
		}
		return "blocked-on-human (" + strings.Join(parts, ", ") + ")", ""
	}

	if len(r.Operations) == 0 {
		return "unjoined (no operations[] — nothing to trace this claim to)", ""
	}
	live, dead := 0, []string{}
	for _, id := range r.Operations {
		stub, ok := ops[id]
		switch {
		case !ok && p.Contract:
			dead = append(dead, id)
		case ok && stub == "":
			live++
		}
	}
	opsCol = fmt.Sprintf("%d/%d", live, len(r.Operations))
	if len(dead) > 0 {
		// Gate 1: a dead operationId is an ERROR — the record claims work
		// that does not exist.
		for _, id := range dead {
			p.Errors = append(p.Errors, fmt.Sprintf(
				"REQ-%s → operation %q is not in %s (REQ0102) — renamed or never landed", r.ID, id, contractFile))
		}
		return "broken (dead operation join)", opsCol
	}
	if !p.Contract {
		return "unknown (no committed contract to join against)", opsCol
	}
	if live < len(r.Operations) {
		return fmt.Sprintf("implemented ◐ %d/%d (the rest answer 501)", live, len(r.Operations)), opsCol
	}

	// verified: pinned tests, in a fresh recorded run, all green.
	if len(r.Tests) == 0 {
		return "implemented (no tests pinned — verification has no evidence to read)", opsCol
	}
	// Gate 2 needs a trustworthy test LIST: `--list`'s compiled one, or a
	// fresh recorded full run (whose run events are the same enumeration).
	hasList := test.authoritative || (test.present && test.fresh)
	switch {
	case !hasList && !test.present:
		return "implemented (verification unknown — " + testRecordFile + " missing; `task test` records it)", opsCol
	case !hasList:
		return "implemented (verification unknown — " + testRecordFile + " is stale, re-run `task test`)", opsCol
	}
	failing := 0
	for _, name := range r.Tests {
		if !test.ran[name] {
			p.Errors = append(p.Errors, fmt.Sprintf(
				"REQ-%s pins test %q, which is not in the %s (REQ0103)", r.ID, name, test.listName()))
			return "broken (pinned test vanished)", opsCol
		}
		if !test.passed[name] {
			failing++
		}
	}
	// OUTCOMES only ever come from a fresh recorded run — `--list` proves a
	// test compiles, never that it passed.
	switch {
	case !test.present:
		return "implemented (tests exist; outcomes unknown — `task test` records them)", opsCol
	case !test.fresh:
		return "implemented (verification unknown — " + testRecordFile + " is stale, re-run `task test`)", opsCol
	case failing > 0:
		return fmt.Sprintf("implemented (%s failing in the recorded run)", count(failing, "pinned test")), opsCol
	case !test.runPassed:
		return "implemented (pinned tests green but the recorded run FAILED elsewhere — a red suite verifies nothing)", opsCol
	}

	// validated: e2e titles + acceptance prose.
	if !r.HasAcceptance {
		return "verified (no acceptance criteria in the body — validation has nothing to walk)", opsCol
	}
	if len(r.E2E) == 0 {
		return "verified (no e2e titles pinned)", opsCol
	}
	switch {
	case !e2e.present:
		return "verified (validation unknown — " + e2eRecordFile + " missing; `task e2e` records it)", opsCol
	case !e2e.fresh:
		return "verified (validation unknown — " + e2eRecordFile + " is stale, re-run `task e2e`)", opsCol
	}
	for _, title := range r.E2E {
		ok, ran := e2e.specs[title]
		switch {
		case !ran:
			return fmt.Sprintf("verified (e2e title %q is not in the recorded report)", title), opsCol
		case !ok:
			return fmt.Sprintf("verified (e2e %q failed in the recorded run)", title), opsCol
		}
	}
	return "validated", opsCol
}

// ---- the recorded artifacts ----

type testRecord struct {
	present, fresh bool
	// authoritative: the ran set came from `go test -list` (fresh compile)
	// rather than the recorded run.
	authoritative bool
	ran           map[string]bool
	passed        map[string]bool
	runPassed     bool
}

func (t *testRecord) listName() string {
	if t.authoritative {
		return "compiled test list (go test -list)"
	}
	return "recorded run (" + testRecordFile + ")"
}

// readTestRecord parses a teed `go test -json` stream: run events enumerate
// the compiled list (the -list semantics of a full run), pass/fail events
// carry outcomes, and any fail — test or package — makes the run red.
func readTestRecord(dir string, srcTime time.Time, p *tracePack) *testRecord {
	t := &testRecord{ran: map[string]bool{}, passed: map[string]bool{}, runPassed: true}
	rec := recordInfo(dir, testRecordFile, srcTime)
	if !rec.Present {
		rec.Note = "missing — `task test` tees the run here; verified statuses render unknown"
		p.Records = append(p.Records, rec)
		return t
	}
	raw, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(testRecordFile)))
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	for dec.More() {
		var ev struct {
			Action, Package, Test string
		}
		if err := dec.Decode(&ev); err != nil {
			rec.Note = "unparseable — not a `go test -json` stream; re-run `task test`"
			rec.Fresh = false
			p.Records = append(p.Records, rec)
			return t
		}
		switch {
		case ev.Test != "" && ev.Action == "run":
			t.ran[ev.Test] = true
		case ev.Test != "" && ev.Action == "pass":
			t.passed[ev.Test] = true
		case ev.Action == "fail":
			t.runPassed = false
		}
	}
	t.present, t.fresh = true, rec.Fresh
	if !rec.Fresh {
		rec.Note = "STALE — sources changed after this run; statuses depending on it render unknown"
	}
	p.Records = append(p.Records, rec)
	return t
}

type e2eRecord struct {
	present, fresh bool
	specs          map[string]bool // spec title → passed
}

// readE2ERecord parses the playwright JSON report `task e2e` records:
// suites nest, specs carry title + ok.
func readE2ERecord(dir string, srcTime time.Time, p *tracePack) *e2eRecord {
	e := &e2eRecord{specs: map[string]bool{}}
	rec := recordInfo(dir, e2eRecordFile, srcTime)
	if !rec.Present {
		rec.Note = "missing — `task e2e` tees the playwright report here; validated statuses render unknown"
		p.Records = append(p.Records, rec)
		return e
	}
	raw, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(e2eRecordFile)))
	var report struct {
		Suites []json.RawMessage `json:"suites"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		rec.Note = "unparseable — not a playwright JSON report; re-run `task e2e`"
		rec.Fresh = false
		p.Records = append(p.Records, rec)
		return e
	}
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		var suite struct {
			Specs []struct {
				Title string `json:"title"`
				OK    bool   `json:"ok"`
			} `json:"specs"`
			Suites []json.RawMessage `json:"suites"`
		}
		if json.Unmarshal(raw, &suite) != nil {
			return
		}
		for _, s := range suite.Specs {
			e.specs[s.Title] = s.OK
		}
		for _, sub := range suite.Suites {
			walk(sub)
		}
	}
	for _, s := range report.Suites {
		walk(s)
	}
	e.present, e.fresh = true, rec.Fresh
	if !rec.Fresh {
		rec.Note = "STALE — sources changed after this run; statuses depending on it render unknown"
	}
	p.Records = append(p.Records, rec)
	return e
}

// recordInfo stats one recorded artifact and judges freshness against the
// newest source mtime — never fakes it: older than the code = stale.
func recordInfo(dir, rel string, srcTime time.Time) traceRecord {
	rec := traceRecord{Path: rel}
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return rec
	}
	rec.Present = true
	rec.Age = ageString(fi.ModTime())
	rec.Fresh = !fi.ModTime().Before(srcTime)
	return rec
}

// newestSourceTime is the freshness baseline: the latest mtime among the
// files a recorded run could be stale against. Cheap (one walk), VCS-free.
func newestSourceTime(dir string) time.Time {
	var newest time.Time
	skip := map[string]bool{".git": true, "node_modules": true, ".ultra": true,
		"bin": true, ".svelte-kit": true, "test-results": true, "playwright-report": true}
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".go"), strings.HasSuffix(path, ".sql"),
			strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".tsx"), strings.HasSuffix(path, ".svelte"),
			filepath.Base(path) == contractFile:
			if fi, err := d.Info(); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		return nil
	})
	return newest
}

func ageString(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "seconds ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// goTestList re-derives the compiled test list fresh — `go test -list`
// compiles every test package without running it, which is a whole product
// build. That cost is why it is a flag and the recorded run is the default.
func goTestList(dir string) (map[string]bool, error) {
	out, err := runIn(dir, "go", "test", "-list", "^Test", "./...")
	if err != nil {
		return nil, fmt.Errorf("%s", firstLineOf(out))
	}
	names := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Test") && !strings.ContainsAny(line, " \t") {
			names[line] = true
		}
	}
	return names, nil
}

// ---- exemptions ----

// loadExempt reads requirements/exempt.txt: one pattern per line, `#`
// comments; an exact operationId or a trailing-`*` prefix glob.
func loadExempt(dir string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(exemptFile)))
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			patterns = append(patterns, line)
		}
	}
	return patterns
}

func matchesExempt(id string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") && strings.HasPrefix(id, strings.TrimSuffix(p, "*")) {
			return true
		}
		if id == p {
			return true
		}
	}
	return false
}

// ---- render ----

func renderTrace(out io.Writer, p *tracePack, parseErrs []error) {
	fmt.Fprintf(out, "trace — %s (%s)\n", p.Product, count(len(p.Requirements), "requirement"))

	fmt.Fprintln(out, "\nREQUIREMENTS")
	if len(p.Requirements) == 0 {
		fmt.Fprintln(out, "  (none yet — ultra new requirement <id> \"<title>\")")
	}
	t := newTable(out)
	for _, r := range p.Requirements {
		t.row("  REQ-"+r.ID, r.Status, r.Ops, r.Derived)
	}
	t.flush()

	fmt.Fprintln(out, "\nRECORDS — trace reads these; it never runs tests")
	t = newTable(out)
	for _, rec := range p.Records {
		state := "missing"
		if rec.Present {
			state = "recorded " + rec.Age
			if !rec.Fresh {
				state += " (stale)"
			}
		}
		t.row("  "+rec.Path, state, rec.Note)
	}
	t.flush()

	if len(p.Errors)+len(p.Warnings)+len(parseErrs) > 0 {
		fmt.Fprintln(out, "\nGATES")
		if n := len(parseErrs); n > 0 {
			fmt.Fprintf(out, "  ERROR    %s (REQ0101) — see stderr for positions\n", count(n, "malformed requirement file"))
		}
		for _, e := range p.Errors {
			fmt.Fprintln(out, "  ERROR    "+e)
		}
		for _, w := range p.Warnings {
			fmt.Fprintln(out, "  WARNING  "+w)
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintln(out, "\nnote: "+n)
	}
}

// traceText renders the human pack into a string for the mcp tool — the
// agent reads exactly what a developer reads.
func traceText(dir string) (string, error) {
	if !hasRequirements(dir) {
		return fmt.Sprintf("no %s/ in %s — trace is dormant (opt in: ultra compliance init)\n", requirementsDir, dir), nil
	}
	p, parseErrs := buildTrace(dir, false)
	var b strings.Builder
	renderTrace(&b, p, parseErrs)
	for _, e := range parseErrs {
		fmt.Fprintln(&b, e)
	}
	return b.String(), nil
}
