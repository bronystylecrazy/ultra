package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// traceSpec is a small committed contract: three live operations, one 501
// stub (zones.get — the x-error-codes marker brief's STUBS uses), and one
// piece of infra surface (auth.token) the exempt file will cover.
const traceSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "shop", "version": "0.1.0"},
  "paths": {
    "/v1/zones": {"get": {"operationId": "zones.list", "responses": {"200": {"description": "ok"}}}},
    "/v1/zones/{id}": {"get": {"operationId": "zones.get", "x-error-codes": ["zones.get.not_implemented"], "responses": {"200": {"description": "ok"}}}},
    "/v1/violations": {"post": {"operationId": "violations.create", "responses": {"200": {"description": "ok"}}}},
    "/v1/export.csv": {"get": {"operationId": "exports.csv", "responses": {"200": {"description": "ok"}}}},
    "/auth/token": {"post": {"operationId": "auth.token", "responses": {"200": {"description": "ok"}}}}
  }
}`

const traceTestJSON = `{"Action":"run","Package":"p","Test":"TestViolations"}
{"Action":"pass","Package":"p","Test":"TestViolations","Elapsed":0.1}
{"Action":"pass","Package":"p","Elapsed":0.2}
`

const traceE2EJSON = `{"suites":[{"title":"golden.spec.ts","specs":[{"title":"records a violation","ok":true}],"suites":[]}]}`

func reqLit(id, title string, ops, tests, e2e []string, body string) string {
	var b strings.Builder
	b.WriteString("---\nid: " + id + "\ntitle: " + title + "\nstatus: draft\n")
	for _, l := range []struct {
		key   string
		items []string
	}{{"operations", ops}, {"tests", tests}, {"e2e", e2e}} {
		if len(l.items) == 0 {
			b.WriteString(l.key + ": []\n")
			continue
		}
		b.WriteString(l.key + ":\n")
		for _, it := range l.items {
			b.WriteString("  - " + it + "\n")
		}
	}
	b.WriteString("---\n" + body)
	return b.String()
}

// freshenRecords pushes the recorded runs' mtimes past every source file, so
// a fixture written in one burst cannot be flaky-stale.
func freshenRecords(t *testing.T, dir string) {
	t.Helper()
	future := time.Now().Add(time.Minute)
	for _, rel := range []string{testRecordFile, e2eRecordFile} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(path); err == nil {
			must(t, os.Chtimes(path, future, future))
		}
	}
}

// traceProduct is the full fixture: contract, records, exemptions, and one
// requirement per rung of the ladder.
func traceProduct(t *testing.T) string {
	t.Helper()
	dir := writeProduct(t, oldMain, map[string]string{
		"openapi.json":             traceSpec,
		".ultra/test.json":         traceTestJSON,
		".ultra/e2e.json":          traceE2EJSON,
		"requirements/exempt.txt":  "# infra\nauth.*\n",
		"requirements/REQ-V1.md":   reqLit("V1", "Record violations", []string{"violations.create"}, []string{"TestViolations"}, []string{"records a violation"}, "\n## Acceptance\n\n- one violation per over-limit reading\n"),
		"requirements/REQ-Z1.md":   reqLit("Z1", "Zone management", []string{"zones.list", "zones.get"}, nil, nil, ""),
		"requirements/REQ-OPEN.md": reqLit("OPEN", "Lane capture", []string{"violations.create"}, nil, nil, "\n## Clarifications\n\n- open: which lanes count?\n"),
		"requirements/REQ-CR.md":   reqLit("CR", "Tolerance", []string{"violations.create"}, nil, nil, "\n## Changes\n\n- 2026-08-01 — requested-by: customer\n  description: add tolerance\n"),
	})
	freshenRecords(t, dir)
	return dir
}

// THE DORMANCY COVENANT: a product without requirements/ gets a clean no-op
// — exit 0, an explanatory line, nothing created, nothing touched.
func TestTraceDormantWithoutRequirements(t *testing.T) {
	dir := writeProduct(t, oldMain, map[string]string{"openapi.json": traceSpec})
	before, err := os.ReadDir(dir)
	must(t, err)

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("dormant trace must exit 0, got %d\n%s", code, errW.String())
	}
	for _, want := range []string{"trace is dormant", "ultra compliance init"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dormant message missing %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(errW.String(), "dormant — no requirements/") {
		t.Errorf("verdict:\n%s", errW.String())
	}
	after, err := os.ReadDir(dir)
	must(t, err)
	if len(after) != len(before) {
		t.Errorf("dormant trace changed the tree: %d entries -> %d", len(before), len(after))
	}
}

// The derived-status ladder against one fixture: validated, implemented ◐,
// blocked-on-human (open question AND undecided CR), plus the unclaimed
// warning with the exemption holding auth.* silent.
func TestTraceDerivesTheLadder(t *testing.T) {
	dir := traceProduct(t)

	var out, errW strings.Builder
	code := cmdTrace([]string{dir}, &out, &errW)
	t.Logf("ultra trace\n%s%s", out.String(), errW.String())
	if code != 0 {
		t.Fatalf("no errors in this fixture — exit %d\n%s", code, errW.String())
	}
	for _, want := range []string{
		"REQ-V1", "validated",
		"REQ-Z1", "implemented ◐ 1/2",
		"REQ-OPEN", "blocked-on-human (1 open question)",
		"REQ-CR", "blocked-on-human (1 undecided change)",
		`operation "exports.csv" is claimed by no requirement (REQ0104)`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("trace output missing %q", want)
		}
	}
	if strings.Contains(out.String(), "auth.token") {
		t.Error("auth.token is exempted in requirements/exempt.txt and must not be warned about")
	}
	if !strings.Contains(errW.String(), "1 warning") {
		t.Errorf("verdict should count the warning:\n%s", errW.String())
	}

	// --json: the same pack, as data.
	var jsonOut, jsonErr strings.Builder
	if code := cmdTrace([]string{dir, "--json"}, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("--json exit %d", code)
	}
	var p tracePack
	if err := json.Unmarshal([]byte(jsonOut.String()), &p); err != nil {
		t.Fatalf("--json is not JSON: %v", err)
	}
	derived := map[string]string{}
	for _, r := range p.Requirements {
		derived[r.ID] = r.Derived
	}
	if derived["V1"] != "validated" {
		t.Errorf("V1 derived = %q", derived["V1"])
	}
	if !strings.HasPrefix(derived["Z1"], "implemented ◐ 1/2") {
		t.Errorf("Z1 derived = %q", derived["Z1"])
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "exports.csv") {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

// Gate 1 both directions: a live join passes (the fixture above); a dead
// operationId is an ERROR that exits 1 and names the REQ, the operation and
// the code.
func TestTraceGateDeadOperation(t *testing.T) {
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-DEAD.md"),
		[]byte(reqLit("DEAD", "Ghost", []string{"ghost.op"}, nil, nil, "")), 0o644))

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 1 {
		t.Fatalf("a dead operation must exit 1, got %d\n%s", code, out.String())
	}
	for _, want := range []string{`REQ-DEAD → operation "ghost.op" is not in openapi.json (REQ0102)`, "broken (dead operation join)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

// Gate 2 both directions: a pinned test present in the recorded run verifies
// (the fixture above); one missing from it is an ERROR — go test -list
// semantics read off the recorded full run.
func TestTraceGatePinnedTestVanished(t *testing.T) {
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-GONE.md"),
		[]byte(reqLit("GONE", "Vanished", []string{"violations.create"}, []string{"TestNoSuch"}, nil, "")), 0o644))

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 1 {
		t.Fatalf("a vanished pinned test must exit 1, got %d\n%s", code, out.String())
	}
	for _, want := range []string{`pins test "TestNoSuch"`, "(REQ0103)", "broken (pinned test vanished)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

// Gate 3 both directions: exports.csv warns while unclaimed (the ladder
// test), stops warning once claimed, and the exemption file silences infra
// without touching real surface.
func TestTraceGateUnclaimedOperation(t *testing.T) {
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-EXP.md"),
		[]byte(reqLit("EXP", "CSV export", []string{"exports.csv"}, nil, nil, "")), 0o644))

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "REQ0104") {
		t.Errorf("everything is claimed or exempt — no warning expected:\n%s", out.String())
	}
}

// The freshness seam: trace never fakes it. A record older than the newest
// source renders the dependent statuses UNKNOWN — and the vanished-test gate
// holds its fire, because a stale list proves nothing.
func TestTraceStaleRecordsRenderUnknown(t *testing.T) {
	dir := traceProduct(t)
	past := time.Now().Add(-2 * time.Hour)
	must(t, os.Chtimes(filepath.Join(dir, ".ultra/test.json"), past, past))

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("stale is unknown, not an error — exit %d\n%s", code, out.String())
	}
	for _, want := range []string{"verification unknown", "stale", "(stale)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "validated") {
		t.Errorf("a stale test record must not let validated stand:\n%s", out.String())
	}
}

// Missing records: the statuses that depend on them render unknown, with the
// verb that records them named.
func TestTraceMissingRecordsRenderUnknown(t *testing.T) {
	dir := traceProduct(t)
	must(t, os.RemoveAll(filepath.Join(dir, ".ultra")))

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("missing records are unknown, not an error — exit %d", code)
	}
	for _, want := range []string{".ultra/test.json missing", "`task test` records it", "missing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
}

// A malformed REQ file fails trace with the positioned REQ0101 error on
// stderr and a nonzero exit — the record is refused, not half-joined.
func TestTraceRefusesMalformedRequirement(t *testing.T) {
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-BAD.md"),
		[]byte("---\nid: BAD\ntitle: T\nstatus: shiny\n---\n"), 0o644))

	var out, errW strings.Builder
	if code := cmdTrace([]string{dir}, &out, &errW); code != 1 {
		t.Fatalf("malformed must exit 1, got %d", code)
	}
	if !strings.Contains(errW.String(), "REQ-BAD.md:1:1:") || !strings.Contains(errW.String(), "ultra explain REQ0101") {
		t.Errorf("stderr should carry the positioned error:\n%s", errW.String())
	}
	if !strings.Contains(out.String(), "1 malformed requirement file (REQ0101)") {
		t.Errorf("the gates section should count it:\n%s", out.String())
	}
}
