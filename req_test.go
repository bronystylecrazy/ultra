package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedToday pins the stamp clock so dates in assertions are literals.
func fixedToday(t *testing.T) {
	t.Helper()
	old := reqNow
	reqNow = func() time.Time { return time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { reqNow = old })
}

const fullReq = `---
id: SPD-01
title: Record a speeding violation
status: approved
approved:
  by: Owner
  date: 2026-08-01
operations:
  - violations.create
  - violations.list
e2e:
  - records a violation from the live feed
tests:
  - TestViolationCreate
frame: "https://figma.com/x?node-id=1-2"
---

## Statement

A violation is recorded when a reading exceeds the zone limit.

## Rationale

The enforcement contract bills per recorded violation.

## Acceptance

- A reading over the limit creates exactly one violation row.

## Clarifications

- 2026-08-01 — Q: Duplicate plates within a minute? A: Deduplicate.

## Changes

- 2026-08-01 — requested-by: customer
  description: also store lane number
  impact: additive — 0 breaking per ultra breaking
  disposition: rejected
  decided-by: Owner (2026-08-01)
`

// The format round-trip: parse → render → parse, and both sides agree on
// every join and every record field.
func TestRequirementRoundTrip(t *testing.T) {
	r, err := parseRequirement("requirements/REQ-SPD-01.md", []byte(fullReq))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.ID != "SPD-01" || r.Title != "Record a speeding violation" || r.Status != "approved" {
		t.Errorf("head fields: %+v", r)
	}
	if r.ApprovedBy != "Owner" || r.ApprovedDate != "2026-08-01" {
		t.Errorf("approved stamp: %q %q", r.ApprovedBy, r.ApprovedDate)
	}
	if strings.Join(r.Operations, ",") != "violations.create,violations.list" {
		t.Errorf("operations: %v", r.Operations)
	}
	if len(r.E2E) != 1 || r.E2E[0] != "records a violation from the live feed" {
		t.Errorf("e2e: %v", r.E2E)
	}
	if len(r.Tests) != 1 || r.Tests[0] != "TestViolationCreate" {
		t.Errorf("tests: %v", r.Tests)
	}
	if r.Frame != "https://figma.com/x?node-id=1-2" {
		t.Errorf("frame: %q", r.Frame)
	}
	if !r.HasAcceptance {
		t.Error("acceptance prose not detected")
	}
	if len(r.Open) != 0 || r.Answered != 1 {
		t.Errorf("clarifications: open=%v answered=%d", r.Open, r.Answered)
	}
	if len(r.Changes) != 1 {
		t.Fatalf("changes: %v", r.Changes)
	}
	c := r.Changes[0]
	if c.Disposition != "rejected" || c.DecidedBy != "Owner (2026-08-01)" || c.RequestedBy != "customer" {
		t.Errorf("change entry: %+v", c)
	}

	r2, err := parseRequirement("requirements/REQ-SPD-01.md", []byte(renderRequirement(r)))
	if err != nil {
		t.Fatalf("re-parse of the canonical render: %v", err)
	}
	if r2.ID != r.ID || r2.Status != r.Status || len(r2.Operations) != 2 ||
		len(r2.Changes) != 1 || r2.HasAcceptance != r.HasAcceptance || r2.Body != r.Body {
		t.Errorf("round trip drifted:\n%s", renderRequirement(r))
	}
}

// The strict parser: every malformed shape is refused with a POSITIONED,
// caret-quality error in the REQ0101 family — never half-read.
func TestRequirementMalformedDiagnostics(t *testing.T) {
	cases := []struct {
		name, body string
		want       []string // substrings of the error
	}{
		{"unknown key with did-you-mean",
			"---\nid: A\ntitle: T\nstatus: draft\noperatons: []\n---\n",
			[]string{"REQ-A.md:5:1:", `unknown frontmatter key "operatons"`, `did you mean "operations"?`, "operatons: []", "ultra explain REQ0101"}},
		{"invented status",
			"---\nid: A\ntitle: T\nstatus: implemented\n---\n",
			[]string{`status "implemented"`, "DERIVED by `ultra trace`"}},
		{"unclosed fence",
			"---\nid: A\ntitle: T\nstatus: draft\n",
			[]string{"never closes"}},
		{"id/filename mismatch",
			"---\nid: B\ntitle: T\nstatus: draft\n---\n",
			[]string{"does not match the filename", "REQ-B.md"}},
		{"approved without a stamp",
			"---\nid: A\ntitle: T\nstatus: approved\n---\n",
			[]string{"approved{by,date} stamp is missing", "ultra req approve"}},
		{"draft with a stale stamp",
			"---\nid: A\ntitle: T\nstatus: draft\napproved:\n  by: X\n  date: 2026-01-01\n---\n",
			[]string{"a draft carries an approved{} stamp"}},
		{"bad disposition",
			"---\nid: A\ntitle: T\nstatus: draft\n---\n\n## Changes\n\n- 2026-01-01 — requested-by: x\n  disposition: maybe\n",
			[]string{`disposition "maybe"`, "accepted|rejected|deferred"}},
		{"disposition without decided-by",
			"---\nid: A\ntitle: T\nstatus: draft\n---\n\n## Changes\n\n- 2026-01-01 — requested-by: x\n  disposition: accepted\n",
			[]string{"no decided-by", "names the human"}},
		{"malformed change header",
			"---\nid: A\ntitle: T\nstatus: draft\n---\n\n## Changes\n\n- just some prose\n",
			[]string{"requested-by:"}},
		{"list item with no list",
			"---\nid: A\ntitle: T\nstatus: draft\n- stray\n---\n",
			[]string{"no list key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRequirement("requirements/REQ-A.md", []byte(tc.body))
			if err == nil {
				t.Fatal("the parser accepted a malformed file")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%s", want, err)
				}
			}
		})
	}
	// The caret really points: line echoed, caret under column 1.
	_, err := parseRequirement("requirements/REQ-A.md", []byte("---\nid: A\ntitle: T\nstatus: draft\nbogus: 1\n---\n"))
	if err == nil || !strings.Contains(err.Error(), "\n    bogus: 1\n    ^") {
		t.Errorf("no caret line:\n%v", err)
	}
}

// reqProduct is a fixture product with requirements/ and one draft REQ.
func reqProduct(t *testing.T, extra map[string]string) string {
	t.Helper()
	files := map[string]string{}
	for k, v := range extra {
		files[k] = v
	}
	dir := writeProduct(t, oldMain, files)
	return dir
}

func TestNewRequirementScaffoldsAndRefuses(t *testing.T) {
	fixedToday(t)
	dir := reqProduct(t, nil)

	var out, errW strings.Builder
	if code := cmdNewRequirement([]string{"SPD-01", "Record a violation", dir}, &out, &errW); code != 0 {
		t.Fatalf("exit %d\n%s", code, errW.String())
	}
	raw := readFile(t, filepath.Join(dir, "requirements/REQ-SPD-01.md"))
	r, err := parseRequirement("requirements/REQ-SPD-01.md", []byte(raw))
	if err != nil {
		t.Fatalf("the scaffold does not parse under its own strict parser: %v", err)
	}
	if r.Status != "draft" || r.Title != "Record a violation" || len(r.Operations) != 0 {
		t.Errorf("scaffold fields: %+v", r)
	}
	if r.HasAcceptance {
		t.Error("a TODO acceptance section must not count as acceptance prose")
	}
	// Writing the first REQ is the opt-in: trace is now active.
	if !hasRequirements(dir) {
		t.Error("requirements/ was not created")
	}

	var out2, errW2 strings.Builder
	if code := cmdNewRequirement([]string{"SPD-01", "Again", dir}, &out2, &errW2); code != 1 {
		t.Errorf("overwrite must be refused, got %d", code)
	}
	if !strings.Contains(errW2.String(), "refusing to overwrite") {
		t.Errorf("refusal text:\n%s", errW2.String())
	}
}

func TestReqApproveStampsWhoAndDate(t *testing.T) {
	fixedToday(t)
	dir := reqProduct(t, nil)
	var out, errW strings.Builder
	cmdNewRequirement([]string{"A-1", "Title", dir}, &out, &errW)

	var aOut, aErr strings.Builder
	if code := cmdReq([]string{"approve", "A-1", dir, "--by", "Owner"}, &aOut, &aErr); code != 0 {
		t.Fatalf("approve: exit %d\n%s", code, aErr.String())
	}
	r, err := loadRequirement(dir, "A-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "approved" || r.ApprovedBy != "Owner" || r.ApprovedDate != "2026-08-02" {
		t.Errorf("stamp: %+v", r)
	}
	if !strings.Contains(aOut.String(), "by Owner, 2026-08-02") {
		t.Errorf("the act must echo the record:\n%s", aOut.String())
	}

	// Idempotent second approve: says so, changes nothing.
	var bOut, bErr strings.Builder
	if code := cmdReq([]string{"approve", "A-1", dir, "--by", "Someone Else"}, &bOut, &bErr); code != 0 {
		t.Fatalf("re-approve: exit %d", code)
	}
	if !strings.Contains(bOut.String(), "already approved by Owner") {
		t.Errorf("re-approve:\n%s", bOut.String())
	}
}

// The refusal with teeth: open questions make approval impossible, and the
// refusal lists them so the human knows what to answer.
func TestReqApproveRefusesOnOpenQuestions(t *testing.T) {
	fixedToday(t)
	dir := reqProduct(t, nil)
	var out, errW strings.Builder
	cmdNewRequirement([]string{"A-1", "Title", dir}, &out, &errW)
	if code := cmdReq([]string{"ask", "A-1", "What timezone renders?", dir}, &out, &errW); code != 0 {
		t.Fatalf("ask: %s", errW.String())
	}

	var aOut, aErr strings.Builder
	if code := cmdReq([]string{"approve", "A-1", dir, "--by", "Owner"}, &aOut, &aErr); code != 1 {
		t.Fatalf("approve over an open question must exit 1, got %d", code)
	}
	for _, want := range []string{"1 open question", "What timezone renders?", "dated Q&A"} {
		if !strings.Contains(aErr.String(), want) {
			t.Errorf("refusal missing %q:\n%s", want, aErr.String())
		}
	}
	if r, _ := loadRequirement(dir, "A-1"); r == nil || r.Status != "draft" {
		t.Error("a refused approval must leave the file draft")
	}
}

// No identity, no approval — the fails-safe rule on the CLI side.
func TestReqApproveRefusesWithoutIdentity(t *testing.T) {
	setupGitEnv(t) // empty global git config: user.name resolves to nothing
	dir := reqProduct(t, nil)
	var out, errW strings.Builder
	cmdNewRequirement([]string{"A-1", "Title", dir}, &out, &errW)

	var aOut, aErr strings.Builder
	if code := cmdReq([]string{"approve", "A-1", dir}, &aOut, &aErr); code != 1 {
		t.Fatalf("approve with no identity must exit 1, got %d", code)
	}
	if !strings.Contains(aErr.String(), "git config user.name") {
		t.Errorf("refusal should name the fix:\n%s", aErr.String())
	}
}

func TestReqAskParksAndTraceWillBlock(t *testing.T) {
	fixedToday(t)
	dir := reqProduct(t, nil)
	var out, errW strings.Builder
	cmdNewRequirement([]string{"A-1", "Title", dir}, &out, &errW)
	cmdReq([]string{"ask", "A-1", "First?", dir}, &out, &errW)
	cmdReq([]string{"ask", "A-1", "Second?", dir}, &out, &errW)

	r, err := loadRequirement(dir, "A-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Open) != 2 || !strings.Contains(r.Open[0], "First?") || !strings.Contains(r.Open[1], "Second?") {
		t.Errorf("open items: %v", r.Open)
	}
	if !strings.Contains(r.Open[0], "asked 2026-08-02") {
		t.Errorf("ask must date the question: %v", r.Open[0])
	}
}

// The re-approval law: an ACCEPTED change drops approved → draft and clears
// the stamp; a REJECTED one is recorded and changes nothing else.
func TestReqChangeDispositions(t *testing.T) {
	fixedToday(t)
	dir := reqProduct(t, nil)
	var out, errW strings.Builder
	cmdNewRequirement([]string{"A-1", "Title", dir}, &out, &errW)
	cmdReq([]string{"approve", "A-1", dir, "--by", "Owner"}, &out, &errW)

	// rejected: recorded, still approved.
	var rOut, rErr strings.Builder
	if code := cmdReq([]string{"change", "A-1", dir, "--description", "make it purple",
		"--requested-by", "customer", "--disposition", "rejected", "--decided-by", "Owner"}, &rOut, &rErr); code != 0 {
		t.Fatalf("change: %s", rErr.String())
	}
	r, err := loadRequirement(dir, "A-1")
	if err != nil {
		t.Fatalf("after rejected change: %v", err)
	}
	if r.Status != "approved" || len(r.Changes) != 1 || r.Changes[0].Disposition != "rejected" {
		t.Errorf("rejected CR: status=%s changes=%+v", r.Status, r.Changes)
	}

	// accepted: the draft-drop.
	var aOut, aErr strings.Builder
	if code := cmdReq([]string{"change", "A-1", dir, "--description", "also store lane",
		"--requested-by", "customer", "--impact", "additive, 0 breaking",
		"--disposition", "accepted", "--decided-by", "Owner"}, &aOut, &aErr); code != 0 {
		t.Fatalf("change: %s", aErr.String())
	}
	if !strings.Contains(aOut.String(), "drops back to DRAFT") {
		t.Errorf("the act must announce the re-approval law:\n%s", aOut.String())
	}
	r, err = loadRequirement(dir, "A-1")
	if err != nil {
		t.Fatalf("after accepted change: %v", err)
	}
	if r.Status != "draft" || r.ApprovedBy != "" || r.ApprovedDate != "" {
		t.Errorf("accepted CR must clear the stamp: %+v", r)
	}
	if len(r.Changes) != 2 || r.Changes[1].Impact != "additive, 0 breaking" {
		t.Errorf("entries: %+v", r.Changes)
	}

	// undecided: recorded without a disposition; trace will block on it.
	var uOut, uErr strings.Builder
	if code := cmdReq([]string{"change", "A-1", dir, "--description", "tbd",
		"--requested-by", "customer"}, &uOut, &uErr); code != 0 {
		t.Fatalf("undecided change: %s", uErr.String())
	}
	if !strings.Contains(uOut.String(), "blocked-on-human") {
		t.Errorf("undecided must say what happens next:\n%s", uOut.String())
	}
	r, _ = loadRequirement(dir, "A-1")
	if len(r.Changes) != 3 || r.Changes[2].Disposition != "" {
		t.Errorf("undecided entry: %+v", r.Changes)
	}
}

// The example file compliance init ships must parse under the same strict
// parser (renamed to REQ- form), or the taught shape is a lie.
func TestExampleTemplateParses(t *testing.T) {
	raw, err := templates.ReadFile("templates/compliance/EXAMPLE.md.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	// The example is prefixed with an HTML comment block; the parser sees the
	// fence first only in real REQ files, so strip to the first fence.
	body := string(raw)
	body = body[strings.Index(body, "---"):]
	if _, err := parseRequirement("requirements/REQ-EXAMPLE-01.md", []byte(body)); err != nil {
		t.Fatalf("EXAMPLE.md.tmpl does not parse: %v", err)
	}
}

func TestRequirementIDsSorted(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, requirementsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"REQ-B.md", "REQ-A.md", "README.md", "EXAMPLE.md"} {
		must(t, os.WriteFile(filepath.Join(dir, requirementsDir, name), []byte("x"), 0o644))
	}
	ids := requirementIDs(dir)
	if strings.Join(ids, ",") != "A,B" {
		t.Errorf("ids: %v (README/EXAMPLE must not count)", ids)
	}
}
