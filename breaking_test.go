package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goldOld = "testdata/breaking/old.json"
const goldNew = "testdata/breaking/new.json"

// The golden pair is built to fire EVERY row of the taxonomy exactly where a
// reader would expect it. If a row stops firing, either the fixture or the
// classifier moved, and both are things this command cannot get quietly wrong.
func TestBreakingGoldenPairFiresEveryTaxonomyRow(t *testing.T) {
	code, out, errOut := runCLI(t, "breaking", goldOld, goldNew)
	if code != 1 {
		t.Fatalf("a breaking change must exit 1, got %d\n%s", code, out)
	}
	for _, kind := range []string{
		// operations
		"operation.removed", "operation.moved", "operation.id_changed", "operation.added",
		// auth
		"auth.required_added", "auth.required_removed",
		"auth.permission_added", "auth.permission_changed", "auth.permission_removed",
		// request body + params
		"request.body.required_added", "request.body.added", "request.body.removed",
		"request.param.required_added", "request.param.removed", "request.param.added",
		// request fields
		"request.field.required_added", "request.field.removed", "request.field.added",
		"request.field.optional_now", "request.field.nullable_removed", "request.field.nullable_added",
		"request.type_changed",
		"request.enum.value_removed", "request.enum.value_added",
		"request.enum.introduced", "request.enum.dropped",
		// responses
		"response.status_changed", "response.body.removed", "response.body.added",
		"response.field.removed", "response.field.added",
		"response.field.optional_now", "response.field.required_added",
		"response.field.nullable_added", "response.field.nullable_removed",
		"response.array.nullable_added", "response.type_changed",
		"response.enum.value_added", "response.enum.value_removed",
		"response.enum.introduced", "response.enum.dropped",
		// errors + composition
		"errors.code_removed", "errors.code_added",
		"errors.status_removed", "errors.status_added",
		"schema.composition_changed",
	} {
		if !strings.Contains(out, kind) {
			t.Errorf("the golden pair must exercise %s", kind)
		}
	}
	if !strings.Contains(errOut, "✗ breaking — 27 breaking changes, 8 warnings") {
		t.Errorf("verdict: %q", errOut)
	}
}

// Each finding has to name WHO breaks. These are the sentences a reviewer
// actually reads, so they are pinned rather than merely counted.
func TestBreakingNamesWhoBreaks(t *testing.T) {
	_, out, _ := runCLI(t, "breaking", goldOld, goldNew)
	for _, want := range []string{
		`DELETE /v1/notes/{id} is gone — every consumer calling deleteNote gets 404`,
		`getStats moved from GET /v1/stats to GET /v1/metrics — deployed callers keep requesting the old route and get 404`,
		`the generated client renames healthCheck() to ping() — every call site stops compiling`,
		`readers of listNotes lose "cursor" — code reading that field gets undefined`,
		`the new field "priority" is required — no deployed sender of createNote sets it, so every call gets 422`,
		`"kind" no longer accepts "draft" — senders of createNote still passing it are rejected`,
		`adminPing was public and now requires credentials — anonymous callers get 401`,
		`updateNote moved from the "notes.write" permission to "notes.admin" — tokens granted the old one get 403`,
		`listNotes no longer declares "request.param"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing finding wording:\n  %s", want)
		}
	}
}

// THE load-bearing classification. contrib/api decodes request bodies with a
// plain json.Decoder (api.go:414 — no DisallowUnknownFields), so a field the
// server dropped is IGNORED, not rejected: removing a request field is a
// warning and removing a response field is breaking. If the binder ever gains
// strict decoding, this test is the one that has to be revisited.
func TestBreakingRequestRemovalIsAWarningBecauseUnknownFieldsAreIgnored(t *testing.T) {
	_, out, _ := runCLI(t, "breaking", "--json", goldOld, goldNew)
	var rep breakReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, f := range rep.Findings {
		seen[f.Kind] = f.Severity
	}
	if got := seen["request.field.removed"]; got != sevWarning {
		t.Errorf("removing a request field must be a warning (unknown fields are ignored), got %q", got)
	}
	if got := seen["response.field.removed"]; got != sevBreaking {
		t.Errorf("removing a response field must be breaking, got %q", got)
	}
	// The wording has to say what actually happens, or the warning teaches
	// nothing: the send is discarded in silence, with no 4xx.
	for _, f := range rep.Findings {
		if f.Kind == "request.field.removed" && !strings.Contains(f.Detail, "discards it silently") {
			t.Errorf("the warning must say the field is dropped without a 4xx: %q", f.Detail)
		}
	}
}

func TestBreakingJSONShape(t *testing.T) {
	code, out, _ := runCLI(t, "breaking", "--json", goldOld, goldNew)
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	var rep breakReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("--json must be one JSON document: %v", err)
	}
	if rep.Old != goldOld || rep.New != goldNew {
		t.Errorf("the document must name both inputs: %+v", rep)
	}
	if rep.Breaking != 27 || rep.Warnings != 8 || rep.Additive != 17 {
		t.Errorf("counts: breaking=%d warnings=%d additive=%d", rep.Breaking, rep.Warnings, rep.Additive)
	}
	if len(rep.Findings) != rep.Breaking+rep.Warnings+rep.Additive {
		t.Errorf("the counts must total the findings, got %d", len(rep.Findings))
	}
	for _, f := range rep.Findings {
		if f.Severity == "" || f.Kind == "" || f.Operation == "" || f.Location == "" || f.Detail == "" {
			t.Fatalf("every finding carries every field: %+v", f)
		}
		if f.Method == "" || f.Path == "" {
			t.Fatalf("every finding carries its route: %+v", f)
		}
	}
}

// A pure widening is not a breaking change, and a gate that says otherwise is
// a gate people learn to pass with --force.
func TestBreakingAdditiveOnlyExitsZero(t *testing.T) {
	dir := t.TempDir()
	oldF := writeSpec(t, dir, "old.json", `{
      "openapi":"3.1.0","info":{"title":"t","version":"1"},
      "paths":{"/v1/notes":{"get":{"operationId":"listNotes","responses":{"200":{"description":"OK",
        "content":{"application/json":{"schema":{"type":"object",
          "properties":{"id":{"type":"string"}},"required":["id"]}}}}}}}}}`)
	newF := writeSpec(t, dir, "new.json", `{
      "openapi":"3.1.0","info":{"title":"t","version":"2"},
      "paths":{"/v1/notes":{"get":{"operationId":"listNotes","responses":{"200":{"description":"OK",
        "content":{"application/json":{"schema":{"type":"object",
          "properties":{"id":{"type":"string"},"name":{"type":"string"}},"required":["id"]}}}}}},
        "post":{"operationId":"createNote","responses":{"201":{"description":"Created"}}}}}}`)

	code, out, errOut := runCLI(t, "breaking", oldF, newF)
	if code != 0 {
		t.Fatalf("additive-only must exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "no breaking changes") || !strings.Contains(out, "ADDITIVE") {
		t.Errorf("out=%q", out)
	}
	if strings.Contains(out, "BREAKING") {
		t.Errorf("nothing may be reported as breaking:\n%s", out)
	}
	if !strings.Contains(errOut, "✓ breaking — no breaking changes (2 additive changes)") {
		t.Errorf("verdict: %q", errOut)
	}
}

// Warnings are printed and deliberately do NOT gate: a gate that fails on
// advice stops being read as a gate.
func TestBreakingWarningsDoNotGate(t *testing.T) {
	dir := t.TempDir()
	oldF := writeSpec(t, dir, "old.json", `{
      "openapi":"3.1.0","info":{"title":"t","version":"1"},
      "paths":{"/v1/notes":{"post":{"operationId":"createNote","requestBody":{"required":true,
        "content":{"application/json":{"schema":{"type":"object",
          "properties":{"title":{"type":"string"},"legacy":{"type":"string"}}}}}},
        "responses":{"201":{"description":"Created"}}}}}}`)
	newF := writeSpec(t, dir, "new.json", `{
      "openapi":"3.1.0","info":{"title":"t","version":"2"},
      "paths":{"/v1/notes":{"post":{"operationId":"createNote","requestBody":{"required":true,
        "content":{"application/json":{"schema":{"type":"object",
          "properties":{"title":{"type":"string"}}}}}},
        "responses":{"201":{"description":"Created"}}}}}}`)

	code, out, errOut := runCLI(t, "breaking", oldF, newF)
	if code != 0 {
		t.Fatalf("a warning alone must not gate, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "WARNINGS (1)") {
		t.Errorf("out=%q", out)
	}
	if !strings.Contains(errOut, "✓ breaking — no breaking changes (1 warning)") {
		t.Errorf("verdict: %q", errOut)
	}
}

func TestBreakingIdenticalContracts(t *testing.T) {
	code, out, errOut := runCLI(t, "breaking", goldOld, goldOld)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "the two documents describe the same contract") {
		t.Errorf("out=%q", out)
	}
	if !strings.Contains(errOut, "✓ breaking — no breaking changes") {
		t.Errorf("verdict: %q", errOut)
	}
}

// --against is the CI line. It has to work against a real repository, because
// the whole point is `ultra breaking --against origin/main` in a pipeline.
func TestBreakingAgainstGitRef(t *testing.T) {
	setupGitEnv(t)
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")

	baseline, err := os.ReadFile(goldOld)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, contractFile), baseline, 0o644))
	gitRun(t, dir, "add", contractFile)
	gitRun(t, dir, "commit", "-qm", "contract")

	// The working tree moves; HEAD is the baseline.
	candidate, err := os.ReadFile(goldNew)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, contractFile), candidate, 0o644))

	code, out, errOut := runCLI(t, "breaking", "--against", "HEAD", dir)
	if code != 1 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "operation.removed") || !strings.Contains(out, "deleteNote") {
		t.Errorf("--against must produce the same findings as two files:\n%s", out)
	}
	if !strings.Contains(errOut, "✗ breaking —") {
		t.Errorf("verdict: %q", errOut)
	}

	// Committing the candidate makes HEAD and the tree agree again.
	gitRun(t, dir, "commit", "-qam", "move the contract")
	code, out, _ = runCLI(t, "breaking", "--against", "HEAD", dir)
	if code != 0 || !strings.Contains(out, "the two documents describe the same contract") {
		t.Fatalf("a committed tree matches its own HEAD: code=%d out=%s", code, out)
	}
}

// Three different mistakes with three different fixes. One shared "could not
// read" would send the reader looking in the wrong place.
func TestBreakingAgainstRefusals(t *testing.T) {
	setupGitEnv(t)

	t.Run("not a repository", func(t *testing.T) {
		dir := t.TempDir()
		code, _, errOut := runCLI(t, "breaking", "--against", "HEAD", dir)
		if code != 2 || !strings.Contains(errOut, "is not a git repository") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if !strings.Contains(errOut, "ultra breaking <old.json> <new.json>") {
			t.Errorf("the refusal must offer the two-file form: %q", errOut)
		}
	})

	t.Run("unknown ref", func(t *testing.T) {
		dir := t.TempDir()
		gitRun(t, dir, "init", "-q")
		must(t, os.WriteFile(filepath.Join(dir, contractFile), []byte(`{"openapi":"3.1.0"}`), 0o644))
		gitRun(t, dir, "add", contractFile)
		gitRun(t, dir, "commit", "-qm", "c")

		code, _, errOut := runCLI(t, "breaking", "--against", "origin/nope", dir)
		if code != 2 || !strings.Contains(errOut, `no ref "origin/nope"`) {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if !strings.Contains(errOut, "git fetch origin main") {
			t.Errorf("the refusal must name the CI fix: %q", errOut)
		}
	})

	t.Run("no contract at the ref", func(t *testing.T) {
		dir := t.TempDir()
		gitRun(t, dir, "init", "-q")
		must(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644))
		gitRun(t, dir, "add", "README.md")
		gitRun(t, dir, "commit", "-qm", "c")
		must(t, os.WriteFile(filepath.Join(dir, contractFile), []byte(`{"openapi":"3.1.0"}`), 0o644))

		code, _, errOut := runCLI(t, "breaking", "--against", "HEAD", dir)
		if code != 2 || !strings.Contains(errOut, "does not exist at HEAD") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		if !strings.Contains(errOut, "cannot break a consumer that never saw it") {
			t.Errorf("a brand-new contract is not a failure to explain away: %q", errOut)
		}
	})

	t.Run("no contract in the working tree", func(t *testing.T) {
		dir := t.TempDir()
		gitRun(t, dir, "init", "-q")
		must(t, os.WriteFile(filepath.Join(dir, contractFile), []byte(`{"openapi":"3.1.0"}`), 0o644))
		gitRun(t, dir, "add", contractFile)
		gitRun(t, dir, "commit", "-qm", "c")
		must(t, os.Remove(filepath.Join(dir, contractFile)))

		code, _, errOut := runCLI(t, "breaking", "--against", "HEAD", dir)
		if code != 2 || !strings.Contains(errOut, "regenerate it before gating") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
	})
}

func TestBreakingUsageRefusals(t *testing.T) {
	t.Run("one file", func(t *testing.T) {
		code, _, errOut := runCLI(t, "breaking", goldOld)
		if code != 2 || !strings.Contains(errOut, "two OpenAPI documents") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
	})
	t.Run("--against with no ref", func(t *testing.T) {
		code, _, errOut := runCLI(t, "breaking", "--against")
		if code != 2 || !strings.Contains(errOut, "--against needs a git ref") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
	})
	t.Run("unknown flag", func(t *testing.T) {
		code, _, errOut := runCLI(t, "breaking", "--nope", goldOld, goldNew)
		if code != 2 || !strings.Contains(errOut, "unknown flag") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
	})
	t.Run("unreadable input", func(t *testing.T) {
		code, _, errOut := runCLI(t, "breaking", goldOld, filepath.Join(t.TempDir(), "gone.json"))
		if code != 1 || !strings.Contains(errOut, "✗ breaking — unreadable input") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
	})
	t.Run("not an OpenAPI document", func(t *testing.T) {
		dir := t.TempDir()
		bad := writeSpec(t, dir, "bad.json", `{"paths":{}}`)
		code, _, errOut := runCLI(t, "breaking", bad, goldNew)
		if code != 1 || !strings.Contains(errOut, "has no `openapi` version field") {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
	})
}

// contrib/api spells a nullable field as anyOf[X, null], not `nullable: true`.
// Without normalizing that, every optional pointer in the fleet would report
// as a composition change and the real transitions would be invisible.
func TestBreakingUnderstandsTheAnyOfNullSpelling(t *testing.T) {
	dir := t.TempDir()
	oldF := writeSpec(t, dir, "old.json", `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/v1/n":{"get":{"operationId":"getN","responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string"}}}}}}}}}}}`)
	newF := writeSpec(t, dir, "new.json", `{"openapi":"3.1.0","info":{"title":"t","version":"2"},"paths":{"/v1/n":{"get":{"operationId":"getN","responses":{"200":{"description":"OK","content":{"application/json":{"schema":{"type":"object","properties":{"name":{"anyOf":[{"type":"string"},{"type":"null"}]}}}}}}}}}}}`)

	code, out, _ := runCLI(t, "breaking", oldF, newF)
	if code != 1 {
		t.Fatalf("a field going nullable in a response is breaking, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "response.field.nullable_added") {
		t.Errorf("the anyOf-null spelling must read as nullable:\n%s", out)
	}
	if strings.Contains(out, "composition_changed") {
		t.Errorf("anyOf-null is nullability, not a composition change:\n%s", out)
	}
}

func writeSpec(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	must(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}
