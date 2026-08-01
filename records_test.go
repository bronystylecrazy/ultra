package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// frameReq pins a Figma frame (with a pinned version-id) and carries an
// approval stamp, so one fixture exercises the export AND the WP.22 render.
const frameReq = `---
id: F1
title: Zone editor screen
status: approved
approved:
  by: Owner
  date: 2026-08-01
operations:
  - zones.list
e2e: []
tests: []
frame: "https://www.figma.com/design/KEY123/shop?node-id=12-34&version-id=987"
---

## Acceptance

- the zone editor matches the frame
`

// mockFigma serves the two-step images API: the render request (token and
// pinned version checked) answers with a URL back into the same server.
func mockFigma(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/images/KEY123"):
			if r.Header.Get("X-Figma-Token") != "figd_test" {
				w.WriteHeader(403)
				return
			}
			if r.URL.Query().Get("ids") != "12:34" || r.URL.Query().Get("version") != "987" {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"images": map[string]string{"12:34": srv.URL + "/render.png"}})
		case r.URL.Path == "/render.png":
			w.Write([]byte("PNGBYTES"))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stubFigmaBase(t *testing.T, base string) {
	t.Helper()
	orig := figmaAPIBase
	figmaAPIBase = base
	t.Cleanup(func() { figmaAPIBase = orig })
}

// The whole evidence set lands: matrix (both forms), record copies with
// their timestamps, the requirements summary, the frame export, and the
// manifest with UNSIGNED markers — plus the commit reminder.
func TestRecordsFreezeWritesTheEvidenceSet(t *testing.T) {
	fixedToday(t)
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-F1.md"), []byte(frameReq), 0o644))
	t.Setenv("FIGMA_ACCESS_TOKEN", "figd_test")
	stubFigmaBase(t, mockFigma(t).URL)

	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.3", dir}, &out, &errW); code != 0 {
		t.Fatalf("freeze: exit %d\n%s%s", code, out.String(), errW.String())
	}
	t.Logf("ultra records freeze\n%s%s", out.String(), errW.String())

	dest := filepath.Join(dir, "records/v1.2.3")
	for _, rel := range []string{"trace.md", "trace.json", "test.json", "e2e.json",
		"requirements.md", "frames/REQ-F1.png", "RECORD.md"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("freeze did not write %s: %v", rel, err)
		}
	}

	// The copies are byte-identical AND keep the recorded run's own mtime —
	// the timestamp is part of the evidence.
	if got := readFile(t, filepath.Join(dest, "test.json")); got != traceTestJSON {
		t.Errorf("test.json copy differs:\n%s", got)
	}
	src, err := os.Stat(filepath.Join(dir, ".ultra/test.json"))
	must(t, err)
	cp, err := os.Stat(filepath.Join(dest, "test.json"))
	must(t, err)
	if d := src.ModTime().Sub(cp.ModTime()); d > time.Second || d < -time.Second {
		t.Errorf("the copy's mtime must match the recorded run: src=%v copy=%v", src.ModTime(), cp.ModTime())
	}

	var p tracePack
	must(t, json.Unmarshal([]byte(readFile(t, filepath.Join(dest, "trace.json"))), &p))
	derived := map[string]string{}
	for _, r := range p.Requirements {
		derived[r.ID] = r.Derived
	}
	if derived["V1"] != "validated" {
		t.Errorf("trace.json: V1 derived = %q", derived["V1"])
	}

	reqsum := readFile(t, filepath.Join(dest, "requirements.md"))
	for _, want := range []string{
		"## REQ-F1 — Zone editor screen",
		"status: approved — by Owner, 2026-08-01",
		"status: draft — nobody has approved",
		"OPEN QUESTION: which lanes count?",
		"add tolerance → UNDECIDED",
	} {
		if !strings.Contains(reqsum, want) {
			t.Errorf("requirements.md missing %q:\n%s", want, reqsum)
		}
	}

	if got := readFile(t, filepath.Join(dest, "frames/REQ-F1.png")); got != "PNGBYTES" {
		t.Errorf("frame export bytes: %q", got)
	}

	record := readFile(t, filepath.Join(dest, "RECORD.md"))
	for _, want := range []string{
		".ultra/test.json (frozen as test.json): recorded seconds ago, FRESH at freeze",
		".ultra/e2e.json (frozen as e2e.json): recorded seconds ago, FRESH at freeze",
		"- agreement (WP.02 Agreement): UNSIGNED",
		"- uat (UAT validation record): UNSIGNED",
		"- acceptance (WP.01 Acceptance Record): UNSIGNED",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("RECORD.md missing %q:\n%s", want, record)
		}
	}
	if !strings.Contains(out.String(), "commit it WITH the release commit") {
		t.Errorf("the commit reminder is missing:\n%s", out.String())
	}
}

// Stale evidence freezes AS stale: the manifest and the freeze output both
// say so — a freeze never launders staleness into freshness.
func TestRecordsFreezeLabelsStaleEvidence(t *testing.T) {
	fixedToday(t)
	dir := traceProduct(t)
	past := time.Now().Add(-2 * time.Hour)
	must(t, os.Chtimes(filepath.Join(dir, ".ultra/test.json"), past, past))
	t.Setenv("FIGMA_ACCESS_TOKEN", "")

	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.3", dir}, &out, &errW); code != 0 {
		t.Fatalf("stale evidence still freezes (labeled): exit %d\n%s", code, errW.String())
	}
	record := readFile(t, filepath.Join(dir, "records/v1.2.3/RECORD.md"))
	if !strings.Contains(record, "test.json): recorded 2h ago, STALE at freeze") {
		t.Errorf("RECORD.md must label the stale run:\n%s", record)
	}
	if !strings.Contains(out.String(), "frozen STALE") {
		t.Errorf("the freeze output must say it froze stale evidence:\n%s", out.String())
	}
	// And the frozen matrix carries the unknown verdict, not a laundered green.
	if trace := readFile(t, filepath.Join(dir, "records/v1.2.3/trace.md")); !strings.Contains(trace, "verification unknown") {
		t.Errorf("the frozen trace must render the stale-dependent statuses unknown:\n%s", trace)
	}
}

// Idempotence with teeth: the same version freezes once; --force replaces it
// LOUDLY and carries entered signature references over instead of erasing a
// human's record.
func TestRecordsFreezeRefusesRefreeze(t *testing.T) {
	fixedToday(t)
	dir := traceProduct(t)
	t.Setenv("FIGMA_ACCESS_TOKEN", "")

	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.3", dir}, &out, &errW); code != 0 {
		t.Fatalf("first freeze: %s", errW.String())
	}
	if _, err := recordsSign(dir, "v1.2.3", "uat", "scans/uat.pdf", "Owner"); err != nil {
		t.Fatal(err)
	}

	var out2, errW2 strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.3", dir}, &out2, &errW2); code != 1 {
		t.Fatalf("re-freeze must refuse, got %d\n%s", code, out2.String())
	}
	if !strings.Contains(errW2.String(), "--force") {
		t.Errorf("the refusal must name the loud override:\n%s", errW2.String())
	}

	var out3, errW3 strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.3", dir, "--force"}, &out3, &errW3); code != 0 {
		t.Fatalf("--force re-freeze: exit %d\n%s", code, errW3.String())
	}
	if !strings.Contains(out3.String(), "carried over from the previous freeze") {
		t.Errorf("--force must announce the carried signatures:\n%s", out3.String())
	}
	record := readFile(t, filepath.Join(dir, "records/v1.2.3/RECORD.md"))
	if !strings.Contains(record, "- uat (UAT validation record): scans/uat.pdf — entered by Owner, 2026-08-02") {
		t.Errorf("the entered reference must survive a --force re-freeze:\n%s", record)
	}
}

// The fail-safe on frames: no token, or an unreachable API, is a NOTE in the
// record — the freeze states what is missing and never pretends or dies.
func TestRecordsFreezeSkipsFramesWithANote(t *testing.T) {
	fixedToday(t)
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-F1.md"), []byte(frameReq), 0o644))
	t.Setenv("FIGMA_ACCESS_TOKEN", "")

	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.3", dir}, &out, &errW); code != 0 {
		t.Fatalf("no token must not fail the freeze: exit %d\n%s", code, errW.String())
	}
	record := readFile(t, filepath.Join(dir, "records/v1.2.3/RECORD.md"))
	if !strings.Contains(record, "FIGMA_ACCESS_TOKEN is not set") {
		t.Errorf("the record must state the missing export:\n%s", record)
	}
	if _, err := os.Stat(filepath.Join(dir, "records/v1.2.3/frames")); err == nil {
		t.Error("no frames directory should exist when every export was skipped")
	}

	// Token present, API down: per-REQ note with the reason, still exit 0.
	t.Setenv("FIGMA_ACCESS_TOKEN", "figd_test")
	dead := httptest.NewServer(nil)
	dead.Close()
	stubFigmaBase(t, dead.URL)
	var out2, errW2 strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.2.4", dir}, &out2, &errW2); code != 0 {
		t.Fatalf("unreachable API must not fail the freeze: exit %d\n%s", code, errW2.String())
	}
	record2 := readFile(t, filepath.Join(dir, "records/v1.2.4/RECORD.md"))
	if !strings.Contains(record2, "frame export for REQ-F1 skipped") || !strings.Contains(record2, "unreachable") {
		t.Errorf("the record must name the skipped REQ and the reason:\n%s", record2)
	}
}

// THE DORMANCY COVENANT holds for the freeze too: a product that never opted
// in gets a polite refusal that names the opt-in, and no records/ appears.
func TestRecordsFreezeDormantWithoutRequirements(t *testing.T) {
	dir := writeProduct(t, oldMain, map[string]string{"openapi.json": traceSpec})
	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.0.0", dir}, &out, &errW); code != 1 {
		t.Fatalf("dormant freeze must exit 1, got %d", code)
	}
	for _, want := range []string{"no requirements/", "ultra compliance init"} {
		if !strings.Contains(errW.String(), want) {
			t.Errorf("the refusal must stay polite and name the opt-in (%q):\n%s", want, errW.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "records")); err == nil {
		t.Error("a refused freeze must create nothing")
	}
}

// A malformed requirement refuses the freeze: a half-read record must never
// enter the frozen evidence.
func TestRecordsFreezeRefusesMalformedRequirement(t *testing.T) {
	dir := traceProduct(t)
	must(t, os.WriteFile(filepath.Join(dir, "requirements/REQ-BAD.md"),
		[]byte("---\nid: BAD\ntitle: T\nstatus: shiny\n---\n"), 0o644))
	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.0.0", dir}, &out, &errW); code != 1 {
		t.Fatalf("malformed must refuse the freeze, got %d", code)
	}
	if !strings.Contains(errW.String(), "REQ-BAD.md:1:1:") {
		t.Errorf("the refusal carries the positioned error:\n%s", errW.String())
	}
}

// The sign verb: stamps ref + who + date into the frozen manifest, refuses a
// second entry, refuses unknown work products, refuses before any freeze —
// and defaults to the SEMVER-latest frozen version.
func TestRecordsSignStampsTheReference(t *testing.T) {
	fixedToday(t)
	dir := traceProduct(t)
	t.Setenv("FIGMA_ACCESS_TOKEN", "")

	var sOut, sErr strings.Builder
	if code := cmdRecordsSign([]string{"uat", "--ref", "x.pdf", "--by", "Owner", dir}, &sOut, &sErr); code != 1 ||
		!strings.Contains(sErr.String(), "records freeze") {
		t.Fatalf("sign before any freeze must refuse and name the fix: %d\n%s", code, sErr.String())
	}

	var out, errW strings.Builder
	must0 := func(args []string) {
		t.Helper()
		if code := cmdRecordsFreeze(args, &out, &errW); code != 0 {
			t.Fatalf("freeze %v: %s", args, errW.String())
		}
	}
	must0([]string{"v1.2.3", dir})
	must0([]string{"v1.10.0", dir})

	var s2Out, s2Err strings.Builder
	if code := cmdRecordsSign([]string{"acceptance", "--ref", "message-id <cut-approval@customer>", "--by", "Owner", dir}, &s2Out, &s2Err); code != 0 {
		t.Fatalf("sign: exit %d\n%s", code, s2Err.String())
	}
	// No --version: the semver-latest frozen record (v1.10.0 > v1.2.3).
	record := readFile(t, filepath.Join(dir, "records/v1.10.0/RECORD.md"))
	if !strings.Contains(record, "- acceptance (WP.01 Acceptance Record): message-id <cut-approval@customer> — entered by Owner, 2026-08-02") {
		t.Errorf("the reference must land in the latest frozen record:\n%s", record)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "records/v1.2.3/RECORD.md")), "cut-approval") {
		t.Error("the older record must stay untouched")
	}

	var s3Out, s3Err strings.Builder
	if code := cmdRecordsSign([]string{"acceptance", "--ref", "other.pdf", "--by", "Owner", dir}, &s3Out, &s3Err); code != 1 ||
		!strings.Contains(s3Err.String(), "already entered") {
		t.Fatalf("a reference is entered once: %d\n%s", code, s3Err.String())
	}
	var s4Out, s4Err strings.Builder
	if code := cmdRecordsSign([]string{"warranty", "--ref", "x", "--by", "Owner", dir}, &s4Out, &s4Err); code != 1 ||
		!strings.Contains(s4Err.String(), "agreement, uat, acceptance") {
		t.Fatalf("unknown wp must refuse with the list: %d\n%s", code, s4Err.String())
	}
}

// No identity, no signature entry — the same fails-safe rule as approve.
func TestRecordsSignRefusesWithoutIdentity(t *testing.T) {
	setupGitEnv(t) // empty git config: user.name resolves to nothing
	dir := traceProduct(t)
	t.Setenv("FIGMA_ACCESS_TOKEN", "")
	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.0.0", dir}, &out, &errW); code != 0 {
		t.Fatalf("freeze: %s", errW.String())
	}
	var sOut, sErr strings.Builder
	if code := cmdRecordsSign([]string{"uat", "--ref", "x.pdf", dir}, &sOut, &sErr); code != 1 ||
		!strings.Contains(sErr.String(), "git config user.name") {
		t.Fatalf("no identity must refuse naming the fix: %d\n%s", code, sErr.String())
	}
}

// MCP fails-safe: no elicitation capability = pending, UNSIGNED stands, and
// no argument can stand in for the human.
func TestMCPRecordsSignPendingWithoutElicitation(t *testing.T) {
	dir := mcpReqProduct(t)
	t.Setenv("FIGMA_ACCESS_TOKEN", "")
	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.0.0", dir}, &out, &errW); code != 0 {
		t.Fatalf("freeze: %s", errW.String())
	}
	c := mcpSession(t, false)

	text, isErr := contentText(t, c.call("records_sign", map[string]any{
		"dir": dir, "wp": "uat", "ref": "scans/uat.pdf"}))
	if isErr {
		t.Fatalf("pending is a state, not an error: %s", text)
	}
	for _, want := range []string{"pending human signature", "ultra records sign uat", "stays UNSIGNED"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "records/v1.0.0/RECORD.md")), "- uat (UAT validation record): UNSIGNED") {
		t.Error("the record must stay UNSIGNED")
	}
}

// MCP with elicitation: the human confirms, the entry stamps the SERVER-side
// identity — never a caller-supplied one.
func TestMCPRecordsSignElicitsTheHuman(t *testing.T) {
	dir := mcpReqProduct(t)
	t.Setenv("FIGMA_ACCESS_TOKEN", "")
	var out, errW strings.Builder
	if code := cmdRecordsFreeze([]string{"v1.0.0", dir}, &out, &errW); code != 0 {
		t.Fatalf("freeze: %s", errW.String())
	}
	c := mcpSession(t, true)

	c.write(map[string]any{"jsonrpc": "2.0", "id": 200, "method": "tools/call",
		"params": map[string]any{"name": "records_sign", "arguments": map[string]any{
			"dir": dir, "wp": "uat", "ref": "scans/uat.pdf", "version": "v1.0.0"}}})
	elic := c.recv()
	if elic.Method != "elicitation/create" || elic.ID == nil {
		t.Fatalf("expected elicitation/create, got %+v", elic)
	}
	c.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(*elic.ID),
		"result": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}})
	text, isErr := contentText(t, c.recv())
	if isErr || !strings.Contains(text, "entered by MCP Human") {
		t.Fatalf("sign after accept: isErr=%v %s", isErr, text)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "records/v1.0.0/RECORD.md")),
		"- uat (UAT validation record): scans/uat.pdf — entered by MCP Human") {
		t.Error("the stamp must carry the server-side identity")
	}
}
