package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The opt-in act: the class-2 templates land, requirements/ starts existing
// (which is what activates trace), and the records doctrine is stated where
// the records will live.
func TestComplianceInitScaffolds(t *testing.T) {
	fixedToday(t)
	dir := writeProduct(t, oldMain, map[string]string{
		"Taskfile.yml": "version: '3'\n\ntasks:\n  test:\n    cmds:\n      - go test ./...\n",
		"AGENTS.md":    "# shop\n",
	})

	var out, errW strings.Builder
	if code := cmdComplianceInit([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("exit %d\n%s", code, errW.String())
	}
	t.Logf("ultra compliance init\n%s%s", out.String(), errW.String())

	for _, rel := range []string{"AGREEMENT.md", "DECISIONS.md", "BACKUP.md",
		"requirements/README.md", "requirements/EXAMPLE.md", "requirements/exempt.txt",
		"records/README.md"} {
		if !exists(t, dir, rel) {
			t.Errorf("compliance init did not write %s", rel)
		}
	}
	if !hasRequirements(dir) {
		t.Error("the opt-in must create requirements/ — presence is what activates trace")
	}
	// Deliberately NO empty record templates: records are created BY the acts.
	if entries, _ := os.ReadDir(filepath.Join(dir, "records")); len(entries) != 1 {
		t.Errorf("records/ must hold README.md alone, got %d entries", len(entries))
	}
	records := readFile(t, filepath.Join(dir, "records/README.md"))
	if !strings.Contains(records, "no empty record templates") {
		t.Errorf("records/README.md must state the no-blank-records law:\n%s", records)
	}
	// The seeded decisions log carries the adoption decision, dated.
	if !strings.Contains(readFile(t, filepath.Join(dir, "DECISIONS.md")), "2026-08-02") {
		t.Error("DECISIONS.md should be seeded with the dated adoption entry")
	}
	// The hand-owned files get refuse-and-instruct notes, never edits.
	for _, want := range []string{"go test -json ./... | tee .ultra/test.json",
		"outputFile: '../.ultra/e2e.json'", "ultra init --force AGENTS.md"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report is missing the wiring instruction %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "Taskfile.yml")), "test.json") {
		t.Error("compliance init rewrote a hand-owned Taskfile.yml")
	}

	// IDEMPOTENCE + never-touch: the second run writes nothing, and a
	// hand-edited AGREEMENT.md keeps its bytes.
	const mine = "# my agreement, signed in blood\n"
	must(t, os.WriteFile(filepath.Join(dir, "AGREEMENT.md"), []byte(mine), 0o644))
	var out2, errW2 strings.Builder
	if code := cmdComplianceInit([]string{dir}, &out2, &errW2); code != 0 {
		t.Fatalf("second run: exit %d", code)
	}
	if !strings.Contains(errW2.String(), "0 written, 7 kept") {
		t.Errorf("the second run must write nothing:\n%s", errW2.String())
	}
	if got := readFile(t, filepath.Join(dir, "AGREEMENT.md")); got != mine {
		t.Errorf("compliance init touched an existing file:\n%s", got)
	}
}

func TestComplianceInitDryWritesNothing(t *testing.T) {
	dir := writeProduct(t, oldMain, nil)
	var out, errW strings.Builder
	if code := cmdComplianceInit([]string{dir, "--dry"}, &out, &errW); code != 0 {
		t.Fatalf("--dry exit %d\n%s", code, errW.String())
	}
	if exists(t, dir, "AGREEMENT.md") || hasRequirements(dir) {
		t.Fatal("--dry wrote files")
	}
	for _, want := range []string{"would write", "nothing was written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan is missing %q:\n%s", want, out.String())
		}
	}
}

func TestComplianceInitRefusesANonProduct(t *testing.T) {
	var out, errW strings.Builder
	if code := cmdComplianceInit([]string{t.TempDir()}, &out, &errW); code != 2 {
		t.Errorf("no go.mod must exit 2, got %d", code)
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/other\n\ngo 1.26\n"), 0o644))
	var out2, errW2 strings.Builder
	if code := cmdComplianceInit([]string{dir}, &out2, &errW2); code != 1 {
		t.Errorf("a module that does not require the framework must exit 1, got %d", code)
	}
}

// After the opt-in, `ultra init --force AGENTS.md` sees the shape and
// renders the requirements law block — and a product WITHOUT requirements/
// keeps rendering without it (the dormancy covenant on the template side).
func TestAgentsTemplateComplianceBlock(t *testing.T) {
	d := scaffoldData{Name: "shop", Module: "example.com/shop", Version: "v0.9.0", GoVersion: "1.26.3"}
	plain, err := renderTemplate("AGENTS.md.tmpl", d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "Requirements & traceability") {
		t.Error("a product that never opted in must not carry the requirements laws")
	}
	d.Compliance = true
	opted, err := renderTemplate("AGENTS.md.tmpl", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Requirements & traceability", "ultra req ask",
		"Only a HUMAN runs `ultra req approve`", "ultra trace", "Statuses are derived"} {
		if !strings.Contains(opted, want) {
			t.Errorf("the opted-in AGENTS.md is missing %q", want)
		}
	}
	// And initShape reads the flag off the tree.
	dir := writeProduct(t, oldMain, map[string]string{"requirements/README.md": "x\n"})
	shape, _, err := initShape(dir, "example.com/shop")
	if err != nil {
		t.Fatal(err)
	}
	if !shape.Compliance {
		t.Error("initShape must detect requirements/ as the compliance shape")
	}
}
