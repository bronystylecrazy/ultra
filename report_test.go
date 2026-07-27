package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinClock freezes the stamp so a round-trip test asserts on the line it wrote
// rather than on when the suite happened to run.
func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

// reportFleet lays down a fleet: the marker at the top, a product two levels
// down. Reports filed from inside the product must climb to the marker —
// fifteen products' friction in one file is the whole point.
func reportFleet(t *testing.T) (root, deep string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, fleetMarker), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep = filepath.Join(root, "shop", "internal", "app", "orders")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/shop\n\ngo 1.26.3\n\nrequire github.com/bronystylecrazy/ultrastack v0.9.19\n"
	if err := os.WriteFile(filepath.Join(root, "shop", "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, deep
}

func TestReportRoundTripAtTheFleetRoot(t *testing.T) {
	root, deep := reportFleet(t)
	t.Chdir(deep)
	pinClock(t, time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC))

	code, out, errOut := runCLI(t, "report", "friction",
		"--code", "di0001", "--pkg", "contrib/pg",
		"the error named the provider but not the consumer")
	if code != 0 {
		t.Fatalf("file: code=%d err=%s", code, errOut)
	}
	path := filepath.Join(root, reportFile)
	if !strings.Contains(out, path) {
		t.Errorf("stdout must name the file it wrote: %q", out)
	}
	if !strings.Contains(errOut, "✓ report — friction recorded at the fleet root") {
		t.Errorf("verdict must say WHICH root: %q", errOut)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the inbox is not at the fleet root: %v", err)
	}
	var e reportEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &e); err != nil {
		t.Fatalf("the line is not JSON: %v (%s)", err, raw)
	}
	// Everything but the message is STAMPED — the agent spent its tokens on
	// the finding, not on re-typing facts the tool was standing in.
	if e.Kind != "friction" || e.Code != "DI0001" || e.Pkg != "contrib/pg" {
		t.Errorf("entry = %+v", e)
	}
	if e.Product != "example.com/shop" || e.Framework != "v0.9.19" {
		t.Errorf("product/framework not stamped: %+v", e)
	}
	if e.Path != "shop/internal/app/orders" {
		t.Errorf("path must be relative to the root that holds the file, got %q", e.Path)
	}
	if e.Time != "2026-07-28T09:30:00Z" {
		t.Errorf("time = %q", e.Time)
	}
	if !strings.Contains(e.Message, "not the consumer") {
		t.Errorf("message = %q", e.Message)
	}

	// ...and list reads back exactly what file wrote.
	code, out, errOut = runCLI(t, "report", "list")
	if code != 0 {
		t.Fatalf("list: code=%d err=%s", code, errOut)
	}
	for _, want := range []string{"WHEN", "KIND", "friction", "example.com/shop",
		"shop/internal/app/orders", "contrib/pg: DI0001"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "1 report at the fleet root") {
		t.Errorf("list verdict = %q", errOut)
	}
}

// No fleet marker anywhere above: the product root files it, and the verdict
// says so rather than silently picking a directory nobody will find again.
func TestReportFallsBackToTheProductRoot(t *testing.T) {
	product := writeProduct(t, "package main\n\nfunc main() {}\n", nil)
	deep := filepath.Join(product, "internal", "app")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)

	code, _, errOut := runCLI(t, "report", "bug", "the pool dialed the wrong port")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(errOut, "recorded at the product root") {
		t.Errorf("verdict must name the fallback root: %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(product, reportFile)); err != nil {
		t.Fatalf("the inbox is not at the product root: %v", err)
	}
}

func TestReportListFilters(t *testing.T) {
	root, deep := reportFleet(t)
	t.Chdir(deep)

	// Three reports, two kinds, one of them old.
	pinClock(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runCLI(t, "report", "docs", "the preset page still shows Product()")
	pinClock(t, time.Now())
	runCLI(t, "report", "friction", "the drift gate rewrote a file I had staged")
	runCLI(t, "report", "idea", "brief could carry the route table too")

	code, out, _ := runCLI(t, "report", "list", "--kind", "friction")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out, "drift gate") || strings.Contains(out, "preset page") {
		t.Errorf("--kind did not filter:\n%s", out)
	}

	code, out, _ = runCLI(t, "report", "list", "--since", "14d")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(out, "preset page") {
		t.Errorf("--since kept a report from January:\n%s", out)
	}
	// Newest first: an inbox is read from the top.
	if i, j := strings.Index(out, "brief could carry"), strings.Index(out, "drift gate"); i < 0 || j < 0 || i > j {
		t.Errorf("newest must come first:\n%s", out)
	}

	code, out, _ = runCLI(t, "report", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var entries []reportEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, out)
	}
	if len(entries) != 3 || entries[0].Kind != "idea" {
		t.Fatalf("entries = %+v", entries)
	}
	if _, err := os.Stat(filepath.Join(root, reportFile)); err != nil {
		t.Fatal(err)
	}
}

// A half-written line from a killed process must not cost a human the other
// reports — the listing skips what it cannot read and shows the rest.
func TestReportListSkipsUnreadableLines(t *testing.T) {
	root, deep := reportFleet(t)
	body := `{"time":"2026-07-01T00:00:00Z","kind":"bug","message":"real"}
{"time":"2026-07-02T00:00:0
{"time":"2026-07-03T00:00:00Z","kind":"idea","message":"also real"}
`
	if err := os.WriteFile(filepath.Join(root, reportFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)
	code, out, errOut := runCLI(t, "report", "list")
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	if !strings.Contains(out, "real") || !strings.Contains(out, "also real") {
		t.Errorf("a torn line ate the readable ones:\n%s", out)
	}
	if !strings.Contains(errOut, "2 reports") {
		t.Errorf("verdict = %q", errOut)
	}
}

func TestReportRefusals(t *testing.T) {
	_, deep := reportFleet(t)
	t.Chdir(deep)

	// An unknown kind is not a message: the closed set is what keeps the inbox
	// triageable, so it is enforced and named.
	code, _, errOut := runCLI(t, "report", "grumble", "something")
	if code != 2 || !strings.Contains(errOut, "is not a kind") {
		t.Fatalf("bad kind: code=%d err=%q", code, errOut)
	}
	code, _, errOut = runCLI(t, "report", "bug")
	if code != 2 || !strings.Contains(errOut, "needs a message") {
		t.Fatalf("no message: code=%d err=%q", code, errOut)
	}
	code, _, errOut = runCLI(t, "report", "list", "--since", "soon")
	if code != 2 || !strings.Contains(errOut, "is not a duration") {
		t.Fatalf("bad --since: code=%d err=%q", code, errOut)
	}
	// An empty inbox is a fact, not a crash, and it says how to fill one.
	code, _, errOut = runCLI(t, "report", "list")
	if code != 1 || !strings.Contains(errOut, "no reports yet") {
		t.Fatalf("empty inbox: code=%d err=%q", code, errOut)
	}
}

func TestParseSince(t *testing.T) {
	ref := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"72h", "2026-07-25", true},
		{"14d", "2026-07-14", true},
		{"2026-07-01", "2026-07-01", true},
		{"soon", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := parseSince(c.in, ref)
		if ok != c.ok {
			t.Errorf("parseSince(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got.Format("2006-01-02") != c.want {
			t.Errorf("parseSince(%q) = %s, want %s", c.in, got.Format("2006-01-02"), c.want)
		}
	}
}
