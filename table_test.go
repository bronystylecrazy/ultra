package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// columnStarts returns the DISPLAY column at which each cell of a rendered row
// begins — the only thing an "aligned table" actually means. Runes, not bytes:
// a module path is ASCII but "v0.9.1→v0.9.20" is not, and counting bytes would
// call a correctly aligned table crooked. ANSI is stripped first, because an
// escape sequence occupies bytes and no columns at all.
func columnStarts(line string) []int {
	runes := []rune(stripANSI(line))
	var starts []int
	inGap := true
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == ' ':
			// Two spaces open a gap; one is inside a cell.
			if i+1 < len(runes) && runes[i+1] == ' ' {
				inGap = true
			}
		case inGap:
			starts = append(starts, i)
			inGap = false
		}
	}
	return starts
}

func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

// assertAligned fails unless every row starts its columns at the same offsets
// as the header. This is the whole of ORDER 4, and the regression it pins is
// real: a 60-character module path in a "%-32s" column pushed every later
// column of THAT row 28 places right, so DRIFT — the column the table exists
// to be scanned for — landed somewhere different on every line.
func assertAligned(t *testing.T, what, out string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("%s: nothing to align:\n%s", what, out)
	}
	want := columnStarts(lines[0])
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		got := columnStarts(l)
		if len(got) < len(want) {
			continue // a row with fewer cells than the header is not misaligned
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: column %d starts at %d in the header and %d in %q\n%s",
					what, i, want[i], got[i], l, out)
				break
			}
		}
	}
}

// A module path is data, and data has no width. The fixture is deliberately
// past every fixed width the old code guessed at.
const longModule = "github.com/acme-industrial/platform/services/telemetry-ingest"

// noProbe stops fleetStatus from booting a product per row: this is a
// RENDERING test, and the fixture already carries the numbers.
func noProbe(t *testing.T) {
	t.Helper()
	old := fingerprintOf
	fingerprintOf = func(*fleetProduct) {}
	t.Cleanup(func() { fingerprintOf = old })
}

func TestFleetStatusTableAligns(t *testing.T) {
	repos := []fleetProduct{
		{Dir: "a", Module: "github.com/acme/tiny", Version: "v0.9.1", Fingerprint: "abc123def456", Components: 7},
		{Dir: "b", Module: longModule, Version: "v0.9.20", Fingerprint: "0011223344556677", Components: 412,
			Drift: "DRIFT (was aabbccddeeff)"},
		{Dir: "c", Module: "github.com/acme/mid", Version: "v0.9.20", Replaced: true, Err: "graph --json timed out"},
	}
	noProbe(t)
	var out, errW bytes.Buffer
	fleetStatus(repos, t.TempDir(), false, false, &out, &errW)
	assertAligned(t, "fleet status", out.String())
	if !strings.Contains(out.String(), longModule) {
		t.Fatalf("the long module never made it into the table:\n%s", out.String())
	}
	// The verdict counts what the table showed.
	if !strings.Contains(errW.String(), "✗ fleet status — 3 products, 1 drifted, 1 unreadable") {
		t.Errorf("verdict = %q", errW.String())
	}
}

// A product that cannot answer `graph --json` reports the failure in its OWN
// voice: coloured, multi-line, arbitrarily long. That text lands in the
// FINGERPRINT cell, which is padded — so row() panicked and `ultra fleet
// status` crashed for any fleet holding one unbootable product (the common
// case: a product whose database is absent). The old fixture used a plain
// one-line error and never reached it.
func TestFleetStatusSurvivesAProductsOwnDiagnostic(t *testing.T) {
	realDiagnostic := "\x1b[31merror[PG0103]\x1b[0m: postgres has no database named \"acme\"\n" +
		"  \x1b[2mnote:\x1b[0m  cause: dial tcp [::1]:5432: connect: connection refused\n" +
		"  \x1b[2mmore:\x1b[0m  ./app explain PG0103"
	repos := []fleetProduct{
		{Dir: "a", Module: "github.com/acme/tiny", Version: "v0.9.1", Fingerprint: "abc123def456", Components: 7},
		{Dir: "b", Module: longModule, Version: "v0.9.30", Err: plainLine(realDiagnostic)},
	}
	noProbe(t)
	var out, errW bytes.Buffer
	fleetStatus(repos, t.TempDir(), false, false, &out, &errW) // panicked before the fix

	body := out.String()
	if strings.Contains(body, "\x1b[") {
		t.Errorf("a padded cell carries ANSI:\n%q", body)
	}
	if strings.Count(body, "\n") != 3 { // header + two rows
		t.Errorf("the diagnostic's newlines survived into the table:\n%s", body)
	}
	if !strings.Contains(body, "error: error[PG0103]") {
		t.Errorf("the cell lost the diagnostic it exists to report:\n%s", body)
	}
	assertAligned(t, "fleet status (a product's own diagnostic)", body)
}

// plainLine is what callers use to obey row()'s rule, so it owes them exactly
// two things: no escapes, and one line.
func TestPlainLine(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"strips a colour pair", "\x1b[31mDRIFT\x1b[0m", "DRIFT"},
		{"drops the sequence, not just the ESC", "\x1b[2mnote\x1b[0m: x", "note: x"},
		{"flattens newlines", "line one\nline two", "line one line two"},
		{"collapses the runs it leaves", "a\n\n  \tb", "a b"},
		{"keeps multi-byte characters", "✗ boot \x1b[31mfailed\x1b[0m", "✗ boot failed"},
		{"survives a bare ESC", "a\x1bb", "ab"},
		{"survives a truncated sequence", "a\x1b[3", "a"},
		{"leaves plain text alone", "graph --json timed out", "graph --json timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := plainLine(c.in); got != c.want {
				t.Errorf("plainLine(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Colour must not move a column: ANSI lands only in the last cell, where
// tabwriter does no padding.
func TestFleetStatusStaysAlignedWithColour(t *testing.T) {
	setEnv(t, map[string]string{"NO_COLOR": "", "FORCE_COLOR": "1", "CLICOLOR_FORCE": ""})
	repos := []fleetProduct{
		{Dir: "a", Module: "github.com/acme/tiny", Version: "v0.9.1", Fingerprint: "abc123def456", Components: 7,
			Drift: "—"},
		{Dir: "b", Module: longModule, Version: "v0.9.20", Fingerprint: "0011223344556677", Components: 412,
			Drift: "DRIFT (was aabbccddeeff)"},
	}
	noProbe(t)
	var out, errW bytes.Buffer
	fleetStatus(repos, t.TempDir(), false, false, &out, &errW)
	if !strings.Contains(out.String(), "\x1b[31m") {
		t.Fatalf("FORCE_COLOR must colour the DRIFT cell:\n%q", out.String())
	}
	assertAligned(t, "fleet status (coloured)", out.String())
}

func TestFleetBumpTableAligns(t *testing.T) {
	results := []bumpResult{
		{Module: "github.com/acme/tiny", Status: "already-current", From: "v0.9.20"},
		{Module: longModule, Status: "bumped", From: "v0.9.1", To: "v0.9.20", Detail: "branch bump/v0.9.20; verified"},
		{Module: "github.com/acme/mid", Status: "FAILED", From: "v0.9.1", Detail: "go build ./... failed"},
	}
	var out bytes.Buffer
	t2 := newTable(&out)
	t2.row("PRODUCT", "STATUS", "FROM→TO", "DETAIL")
	for _, r := range results {
		span := r.From
		if r.Status == "bumped" {
			span = r.From + "→" + r.To
		}
		t2.row(r.Module, r.Status, span, r.Detail)
	}
	t2.flush()
	assertAligned(t, "fleet bump", out.String())
}

// The WIRED listing in `contrib list`: two unbounded columns (the entry
// spelling and the file:line) and a preset name that is not.
func TestContribListTableAligns(t *testing.T) {
	var out bytes.Buffer
	tb := newTable(&out)
	tb.row("  api", `api.Use(api.Info{Title: "telemetry-ingest-gateway", Version: version})`, "main.go:17")
	tb.row("  pg", "(imported; no pg.Use() — not registered)", "internal/app/"+longModule+"/x.go:4")
	tb.row("  migrate", "migrate.Use()", "main.go:19")
	tb.flush()
	assertAligned(t, "contrib list", out.String())
}

// The rule the helper enforces, stated as a test: ANSI in a padded cell is a
// programming error, not a rendering quirk to be tolerated.
func TestTableRefusesANSIInAPaddedCell(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("ANSI in a non-final cell must panic")
		}
	}()
	tb := newTable(&bytes.Buffer{})
	tb.row("\x1b[31mRED\x1b[0m", "plain")
}

// ---- ORDER 2: verdicts, and machine-output purity ----

func TestDiffVerdicts(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.graph")
	b := filepath.Join(dir, "b.graph")
	os.WriteFile(a, []byte("app.Config (singleton, module config) <- []\n"), 0o644)
	os.WriteFile(b, []byte("app.Config (singleton, module config) <- [*pg.DB]\n"), 0o644)

	code, out, errOut := runCLI(t, "diff", a, a)
	if code != 0 || !strings.Contains(out, "identical") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if !strings.Contains(errOut, "✓ diff — graphs identical") {
		t.Errorf("clean diff verdict = %q", errOut)
	}
	code, _, errOut = runCLI(t, "diff", a, b)
	if code != 1 || !strings.Contains(errOut, "✗ diff — 1 change") {
		t.Errorf("drifted diff: code=%d verdict=%q", code, errOut)
	}
}

func TestCodesVerdict(t *testing.T) {
	_, out, errOut := runCLI(t, "codes")
	if !strings.Contains(errOut, "✓ codes — ") || !strings.HasSuffix(strings.TrimSpace(errOut), "codes") {
		t.Errorf("codes verdict = %q", errOut)
	}
	// stdout stays exactly the registry — something greps this.
	if strings.Contains(out, "✓") {
		t.Errorf("the verdict must not land on stdout:\n%s", out)
	}
}

func TestExplainVerdicts(t *testing.T) {
	_, _, errOut := runCLI(t, "explain", "DI0101")
	if !strings.Contains(errOut, "✓ explain — DI0101") {
		t.Errorf("explain verdict = %q", errOut)
	}
	_, _, errOut = runCLI(t, "explain", "DI9999")
	if !strings.Contains(errOut, "✗ explain — unknown code DI9999") {
		t.Errorf("miss verdict = %q", errOut)
	}
	_, _, errOut = runCLI(t, "explain", "PG0101")
	if !strings.Contains(errOut, "✗ explain — PG0101 — ask the product binary") {
		t.Errorf("preset-code verdict = %q", errOut)
	}
}

// A clean vet used to print NOTHING and exit 0 — indistinguishable from a vet
// that never ran. The counts come from the analyzer's own trailer.
func TestVetVerdicts(t *testing.T) {
	bin := t.TempDir()
	script := func(body string) {
		if err := os.WriteFile(filepath.Join(bin, "ultravet"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	script("#!/bin/sh\nexit 0\n")
	code, _, errOut := runCLI(t, "vet", "./...")
	if code != 0 || !strings.Contains(errOut, "✓ vet — no findings") {
		t.Errorf("clean vet: code=%d verdict=%q", code, errOut)
	}

	script("#!/bin/sh\necho 'ultravet: 3 findings (1 fixable — re-run with -fix to apply)'\nexit 1\n")
	code, _, errOut = runCLI(t, "vet", "./...")
	if code != 1 || !strings.Contains(errOut, "✗ vet — 3 findings (1 fixable)") {
		t.Errorf("dirty vet: code=%d verdict=%q", code, errOut)
	}

	// A machine format has no trailer, and the verdict says only what is true.
	script("#!/bin/sh\necho '[]'\nexit 1\n")
	if _, _, errOut := runCLI(t, "vet", "--json"); !strings.Contains(errOut, "✗ vet — findings reported") {
		t.Errorf("json vet verdict = %q", errOut)
	}

	// A load error is neither clean nor a finding.
	script("#!/bin/sh\nexit 2\n")
	if code, _, errOut := runCLI(t, "vet", "./..."); code != 2 || !strings.Contains(errOut, "✗ vet — the analyzer exited 2") {
		t.Errorf("load error: code=%d verdict=%q", code, errOut)
	}
}

// MACHINE PURITY: --json and --format github are byte contracts. The verdict
// rides stderr; stdout is whatever the analyzer wrote, unchanged.
func TestVetMachineOutputStaysPure(t *testing.T) {
	bin := t.TempDir()
	const doc = `[{"code":"DI0001","severity":"error","message":"no provider","file":"main.go","line":9}]`
	os.WriteFile(filepath.Join(bin, "ultravet"), []byte("#!/bin/sh\ncat <<'EOF'\n"+doc+"\nEOF\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, out, errOut := runCLI(t, "vet", "--json")
	if strings.TrimRight(out, "\n") != doc {
		t.Errorf("--json stdout was rewritten:\n got %q\nwant %q", out, doc)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Errorf("--json stdout no longer parses: %v", err)
	}
	if !strings.Contains(errOut, "✗ vet") {
		t.Errorf("the verdict still belongs on stderr: %q", errOut)
	}

	// GitHub annotations go to STDOUT and are parsed line by line by the
	// runner — one stray line and an annotation lands on the wrong file.
	const ann = "::error file=main.go,line=9,title=DI0001::no provider"
	os.WriteFile(filepath.Join(bin, "ultravet"), []byte("#!/bin/sh\necho '"+ann+"'\nexit 1\n"), 0o755)
	_, out, errOut = runCLI(t, "vet", "--format", "github")
	if strings.TrimRight(out, "\n") != ann {
		t.Errorf("annotation stream was polluted:\n%q", out)
	}
	if !strings.Contains(errOut, "✗ vet") {
		t.Errorf("verdict missing from stderr: %q", errOut)
	}
}

// The up-to-date path used to print one line to stdout and nothing else; the
// verdict is now the thing that says it ran at all.
//
// This test never WRITES: it skips when the references are already stale
// (TestReferencesUpToDate is the test that reports that, and a test that
// quietly repaired the tree would hide it).
func TestSkillGenVerdict(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "references")); err != nil {
		t.Skip("not in the repo")
	}
	files, err := skillGen(root)
	if err != nil {
		t.Skip("skill gen cannot run here:", err)
	}
	for rel, want := range files {
		if got, err := os.ReadFile(filepath.Join(root, rel)); err != nil || string(got) != want {
			t.Skip("references are stale — TestReferencesUpToDate owns that report")
		}
	}
	t.Chdir(root)
	code, out, errOut := runCLI(t, "skill", "gen")
	if code != 0 || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	if !strings.Contains(errOut, "✓ skill gen — references up to date") {
		t.Errorf("an up-to-date run must still say so: %q", errOut)
	}
}
