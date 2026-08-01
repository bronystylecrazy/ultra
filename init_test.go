package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oldMain is the shape init exists for: a product that requires the framework
// and wires nothing else — the tree an older scaffold left behind.
const oldMain = `package main

import (
	"github.com/bronystylecrazy/ultrastack/di"
	"github.com/bronystylecrazy/ultrastack/stack"
)

var App = di.Options(stack.Product("shop"))

func main() {}
`

// fullMain wires the two presets init reads: auth gates the e2e spec's
// expectation, i18n earns the message catalogs.
const fullMain = `package main

import (
	"github.com/bronystylecrazy/ultrastack/contrib/auth"
	"github.com/bronystylecrazy/ultrastack/contrib/i18n"
	"github.com/bronystylecrazy/ultrastack/di"
	"github.com/bronystylecrazy/ultrastack/stack"
)

var App = di.Options(
	stack.Product("shop"),
	auth.Use(),
	i18n.Use(),
)

func main() {}
`

// apiMain is the shape the CONTRACT GATE exists for: the typed edge wired, so
// `openapi`/`client` are verbs this product has and TestContractDrift is a
// gate it can actually satisfy.
const apiMain = `package main

import (
	"github.com/bronystylecrazy/ultrastack/contrib/api"
	"github.com/bronystylecrazy/ultrastack/contrib/i18n"
	"github.com/bronystylecrazy/ultrastack/di"
	"github.com/bronystylecrazy/ultrastack/stack"
)

var App = di.Options(
	stack.Product("shop"),
	api.Use(),
	i18n.Use(),
)

func main() {}
`

func exists(t *testing.T, dir, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

// A bare old product gains exactly two files, and the DETECTION shows in their
// bytes: no web/ means AGENTS.md must carry none of its {{if .Web}} sections.
// A retrofit that writes the maximal template would tell every reader of that
// file to run `task e2e` against a frontend that does not exist.
func TestInitRetrofitsABareOldProduct(t *testing.T) {
	dir := writeProduct(t, oldMain, nil)

	var out, errW strings.Builder
	if code := cmdInit([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("init must exit 0, got %d\n%s", code, errW.String())
	}
	t.Logf("ultra init\n%s%s", out.String(), errW.String())

	for _, rel := range []string{"AGENTS.md", ".mcp.json"} {
		if !exists(t, dir, rel) {
			t.Errorf("init did not write %s", rel)
		}
	}
	// Nothing the shape did not earn — contract_test.go included: a product
	// with no api.Use() has no OpenAPI document to drift from, and the gate
	// would fail on its first run instead of bootstrapping.
	for _, rel := range []string{"messages/en.toml", "messages/th.toml",
		"contract_test.go", "web/scripts/check-i18n.ts",
		"web/playwright.config.ts", "web/e2e/golden.spec.ts"} {
		if exists(t, dir, rel) {
			t.Errorf("%s was written for a product with no web/ and no i18n", rel)
		}
	}
	agents := readFile(t, filepath.Join(dir, "AGENTS.md"))
	for _, unwanted := range []string{"task e2e", "web/src/lib/api/", "ultra new table"} {
		if strings.Contains(agents, unwanted) {
			t.Errorf("AGENTS.md carries %q for a product without that capability", unwanted)
		}
	}
	if !strings.Contains(agents, "# shop") {
		t.Errorf("AGENTS.md should be titled from the module name:\n%s", agents[:min(200, len(agents))])
	}
	if !strings.Contains(errW.String(), "2 written, 0 kept") {
		t.Errorf("verdict should count the write:\n%s", errW.String())
	}
	if !strings.Contains(out.String(), "shape      bare") {
		t.Errorf("the report must show the detection:\n%s", out.String())
	}

	// IDEMPOTENCE — the whole design in one assertion.
	var out2, errW2 strings.Builder
	if code := cmdInit([]string{dir}, &out2, &errW2); code != 0 {
		t.Fatalf("second run must exit 0, got %d", code)
	}
	if !strings.Contains(errW2.String(), "0 written, 2 kept") {
		t.Errorf("the second run must write nothing:\n%s", errW2.String())
	}
	if !strings.Contains(out2.String(), "kept (exists)") {
		t.Errorf("an existing file reports itself kept:\n%s", out2.String())
	}
}

// The full shape: every gate open, every gated section present, and the
// detection recovered from the tree rather than from flags nobody typed.
func TestInitDetectsTheFullShape(t *testing.T) {
	dir := writeProduct(t, fullMain, map[string]string{
		"web/package.json":            `{"dependencies":{"@connected/svelte-connected-design":"^1"}}`,
		"internal/db/sqlc.yaml":       "version: \"2\"\n",
		"internal/app/app.go":         "package app\n",
		"web/src/routes/+page.svelte": "<h1>shop</h1>\n",
	})

	var out, errW strings.Builder
	if code := cmdInit([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("init must exit 0, got %d\n%s", code, errW.String())
	}
	t.Logf("ultra init (full shape)\n%s%s", out.String(), errW.String())

	for _, rel := range []string{"AGENTS.md", ".mcp.json", "messages/en.toml",
		"messages/th.toml", "web/playwright.config.ts", "web/e2e/golden.spec.ts"} {
		if !exists(t, dir, rel) {
			t.Errorf("init did not write %s", rel)
		}
	}
	if !strings.Contains(out.String(), "shape      db+web+auth+i18n, ds connected") {
		t.Errorf("the shape line missed a detection:\n%s", out.String())
	}
	agents := readFile(t, filepath.Join(dir, "AGENTS.md"))
	for _, want := range []string{"task e2e", "web/src/lib/api/", "ultra new table"} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md is missing the %q section its shape earns", want)
		}
	}
	// auth.Use() is wired, so 401 IS the healthy anonymous answer — the flag
	// reached the template, not just the report.
	if spec := readFile(t, filepath.Join(dir, "web/e2e/golden.spec.ts")); !strings.Contains(spec, "toBe(401)") {
		t.Errorf("the e2e spec did not see the wired auth:\n%s", spec)
	}
	if cfg := readFile(t, filepath.Join(dir, "web/playwright.config.ts")); !strings.Contains(cfg, "./bin/shop-e2e") {
		t.Errorf("playwright.config.ts did not get the product name:\n%s", cfg)
	}
	if cat := readFile(t, filepath.Join(dir, "messages/en.toml")); !strings.Contains(cat, "Welcome to shop") {
		t.Errorf("the catalog did not get the product name:\n%s", cat)
	}
}

// The finding this fix comes from, reproduced: `ultra upgrade` regenerates
// AGENTS.md, which cites `task contracts` and TestContractDrift as THE
// covenant — and never creates the scaffold-only files a product born before
// them lacks. The gate is unsatisfiable on every upgraded-not-rescaffolded
// product on the fleet. init closes it, adding exactly the missing files.
func TestInitRetrofitsTheContractGate(t *testing.T) {
	dir := writeProduct(t, apiMain, map[string]string{
		// Everything a bump already gave this product...
		"AGENTS.md":                "# shop\n",
		".mcp.json":                "{}\n",
		"messages/en.toml":         "welcome = \"hi\"\n",
		"messages/th.toml":         "welcome = \"สวัสดี\"\n",
		"web/playwright.config.ts": "export default {}\n",
		"web/e2e/golden.spec.ts":   "// mine\n",
		// ...and the two files it hand-owns, neither of which knows about the
		// gate yet.
		"Taskfile.yml":     "version: '3'\n\ntasks:\n  test:\n    cmds:\n      - go test ./...\n",
		"web/package.json": `{"scripts":{"check":"svelte-check --tsconfig ./tsconfig.json"}}`,
	})

	var out, errW strings.Builder
	if code := cmdInit([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("init must exit 0, got %d\n%s", code, errW.String())
	}
	t.Logf("ultra init (contract gate)\n%s%s", out.String(), errW.String())

	// EXACTLY the missing files — the gate and the checks its `bun run check`
	// half needs.
	for _, rel := range []string{"contract_test.go", "web/scripts/check-i18n.ts",
		"web/scripts/check-api-usage.ts", "web/api-usage-allow.txt"} {
		if !exists(t, dir, rel) {
			t.Errorf("init did not retrofit %s", rel)
		}
	}
	if !strings.Contains(errW.String(), "4 written, 6 kept") {
		t.Errorf("init should have added exactly the four missing files:\n%s", errW.String())
	}
	if !strings.Contains(out.String(), "shape      web+i18n+api, ds bare") {
		t.Errorf("the shape line must show the api detection that earned the gate:\n%s", out.String())
	}
	// The artifacts are NOT rendered: contract_test.go writes openapi.json on
	// its first run and gates it from the second, and a document this verb
	// invented would be a contract nobody's code describes.
	if exists(t, dir, "openapi.json") {
		t.Error("init must not fabricate openapi.json — the test bootstraps it")
	}
	gate := readFile(t, filepath.Join(dir, "contract_test.go"))
	for _, want := range []string{"func TestContractDrift", "task contracts", "func TestClientDrift"} {
		if !strings.Contains(gate, want) {
			t.Errorf("contract_test.go is missing %q for a web-shaped product:\n%s", want, gate)
		}
	}
	// i18n-allow.txt is deliberately NOT retrofitted: its bytes are the
	// scaffold's own placeholder copy, and the checker treats a missing
	// allowlist as an empty one.
	if exists(t, dir, "web/i18n-allow.txt") {
		t.Error("web/i18n-allow.txt carries scaffold placeholder copy and must not be retrofitted")
	}

	// The two hand-owned files init REFUSES to rewrite, each with the exact
	// bytes to paste. Refuse and instruct — never a silent merge.
	for _, want := range []string{
		"there is no such\n             task in Taskfile.yml",
		"go run . openapi > openapi.json",
		"go run . client --out web/src/lib/api",
		`web/package.json is yours too`,
		"&& bun scripts/check-i18n.ts && bun scripts/check-api-usage.ts",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report is missing the wiring instruction %q:\n%s", want, out.String())
		}
	}
	if got := readFile(t, filepath.Join(dir, "Taskfile.yml")); strings.Contains(got, "contracts:") {
		t.Errorf("init rewrote a hand-owned Taskfile.yml:\n%s", got)
	}
	if got := readFile(t, filepath.Join(dir, "web/package.json")); strings.Contains(got, "check-i18n") {
		t.Errorf("init rewrote a hand-owned web/package.json:\n%s", got)
	}

	// IDEMPOTENCE, again: the retrofit is a no-op the second time.
	var out2, errW2 strings.Builder
	if code := cmdInit([]string{dir}, &out2, &errW2); code != 0 {
		t.Fatalf("second run must exit 0, got %d", code)
	}
	if !strings.Contains(errW2.String(), "0 written, 10 kept") {
		t.Errorf("the second run must write nothing:\n%s", errW2.String())
	}
}

// The load-bearing promise: a file that exists is not touched. Not merged, not
// reformatted, not "helpfully" updated — byte for byte the same file.
func TestInitNeverTouchesWhatExists(t *testing.T) {
	const mine = "# my own AGENTS.md, hand-written, and none of ultra's business\n"
	dir := writeProduct(t, oldMain, map[string]string{"AGENTS.md": mine})

	var out, errW strings.Builder
	if code := cmdInit([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("init must exit 0, got %d\n%s", code, errW.String())
	}
	if got := readFile(t, filepath.Join(dir, "AGENTS.md")); got != mine {
		t.Errorf("init rewrote a file that already existed:\n%s", got)
	}
	if !strings.Contains(errW.String(), "1 written, 1 kept") {
		t.Errorf("verdict should separate written from kept:\n%s", errW.String())
	}
}

// --dry is a plan, and a plan writes nothing.
func TestInitDryWritesNothing(t *testing.T) {
	dir := writeProduct(t, oldMain, nil)

	var out, errW strings.Builder
	if code := cmdInit([]string{dir, "--dry"}, &out, &errW); code != 0 {
		t.Fatalf("--dry must exit 0, got %d\n%s", code, errW.String())
	}
	t.Logf("ultra init --dry\n%s%s", out.String(), errW.String())
	for _, rel := range []string{"AGENTS.md", ".mcp.json"} {
		if exists(t, dir, rel) {
			t.Fatalf("--dry wrote %s", rel)
		}
	}
	for _, want := range []string{"would write", "nothing was written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan is missing %q:\n%s", want, out.String())
		}
	}
}

// --force is the one door out of "never touch what exists", and it is only
// honest if the diff comes first.
func TestInitForceShowsTheDiffThenRegenerates(t *testing.T) {
	dir := writeProduct(t, oldMain, map[string]string{
		".mcp.json": "{\"mcpServers\": {\"stale\": {\"command\": \"nope\"}}}\n",
	})

	// --force --dry: the diff, and nothing written.
	var dryOut, dryErr strings.Builder
	if code := cmdInit([]string{dir, "--force", ".mcp.json", "--dry"}, &dryOut, &dryErr); code != 0 {
		t.Fatalf("--force --dry must exit 0, got %d\n%s", code, dryErr.String())
	}
	t.Logf("ultra init --force .mcp.json --dry\n%s%s", dryOut.String(), dryErr.String())
	if !strings.Contains(dryOut.String(), `- {"mcpServers": {"stale"`) {
		t.Errorf("the diff must show the line being lost:\n%s", dryOut.String())
	}
	if !strings.Contains(dryOut.String(), `+ 	"mcpServers": {`) {
		t.Errorf("the diff must show what replaces it:\n%s", dryOut.String())
	}
	if strings.Contains(readFile(t, filepath.Join(dir, ".mcp.json")), `"ultra"`) {
		t.Fatalf("--force --dry wrote the file")
	}

	var out, errW strings.Builder
	if code := cmdInit([]string{dir, "--force", ".mcp.json"}, &out, &errW); code != 0 {
		t.Fatalf("--force must exit 0, got %d\n%s", code, errW.String())
	}
	got := readFile(t, filepath.Join(dir, ".mcp.json"))
	if !strings.Contains(got, `"ultra"`) || strings.Contains(got, "stale") {
		t.Errorf(".mcp.json was not regenerated:\n%s", got)
	}
	// AGENTS.md was NOT in the --force run: the verb is surgical.
	if exists(t, dir, "AGENTS.md") {
		t.Errorf("--force must touch only the file it names")
	}
	// And a second --force over a current file is a no-op with a verdict.
	var out2, errW2 strings.Builder
	if code := cmdInit([]string{dir, "--force", ".mcp.json"}, &out2, &errW2); code != 0 {
		t.Fatalf("a current --force must exit 0, got %d", code)
	}
	if !strings.Contains(errW2.String(), "already current") {
		t.Errorf("re-forcing a current file should say so:\n%s", errW2.String())
	}

	// A file init does not own is a usage error that names what it does.
	var badOut, badErr strings.Builder
	if code := cmdInit([]string{dir, "--force", "go.mod"}, &badOut, &badErr); code != 2 {
		t.Errorf("--force on a file init does not own must exit 2, got %d", code)
	}
	if !strings.Contains(badErr.String(), "AGENTS.md") {
		t.Errorf("the refusal should list what init owns:\n%s", badErr.String())
	}
}

// The gate: this verb edits a product, and a product is a module that requires
// the framework. Both halves refuse in the voice `ultra upgrade` uses.
func TestInitRefusesANonProduct(t *testing.T) {
	var out, errW strings.Builder
	if code := cmdInit([]string{t.TempDir()}, &out, &errW); code != 2 {
		t.Errorf("a directory with no go.mod must exit 2, got %d", code)
	}
	if !strings.Contains(errW.String(), "ultra new <name>") {
		t.Errorf("the refusal should point at the verb that creates one:\n%s", errW.String())
	}

	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/other\n\ngo 1.26\n"), 0o644))
	var out2, errW2 strings.Builder
	if code := cmdInit([]string{dir}, &out2, &errW2); code != 1 {
		t.Errorf("a module that does not require the framework must exit 1, got %d", code)
	}
	if !strings.Contains(errW2.String(), "does not require the framework") {
		t.Errorf("the refusal should say why:\n%s", errW2.String())
	}
}

// `ultra upgrade` is where somebody is already reading what the framework
// learned since — so it is where the missing doctrine gets named. Once.
func TestUpgradeNudgesTowardInit(t *testing.T) {
	setupGitEnv(t)
	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.6.0", nil)

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out, &errW); code != 0 {
		t.Fatalf("already-current must exit 0, got %d\n%s", code, errW.String())
	}
	if !strings.Contains(out.String(), "no AGENTS.md and .mcp.json — `ultra init`") {
		t.Errorf("upgrade should name both missing files:\n%s", out.String())
	}

	// Retrofit, and the note is gone — the nudge tracks the tree, not a flag.
	must(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# p\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}\n"), 0o644))
	var out2, errW2 strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out2, &errW2); code != 0 {
		t.Fatalf("already-current must exit 0, got %d\n%s", code, errW2.String())
	}
	if strings.Contains(out2.String(), "ultra init") {
		t.Errorf("the nudge must disappear once the files exist:\n%s", out2.String())
	}
}

// The bump is where AGENTS.md is regenerated, so it is where the reader is
// promised `task contracts` and TestContractDrift. On a product that predates
// them the nudge must NAME the files, not just the verb — an unsatisfiable
// covenant is worse than a missing one.
func TestUpgradeNudgeNamesTheContractGate(t *testing.T) {
	setupGitEnv(t)
	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.6.0", map[string]string{
		"main.go":   apiMain,
		"AGENTS.md": "# p\n",
		".mcp.json": "{}\n",
	})

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out, &errW); code != 0 {
		t.Fatalf("already-current must exit 0, got %d\n%s", code, errW.String())
	}
	t.Logf("ultra upgrade\n%s", out.String())
	// The whole list, joined the way a sentence with more than two names is.
	if !strings.Contains(out.String(), "no contract_test.go, messages/en.toml and messages/th.toml — `ultra init`") {
		t.Errorf("the nudge must name every file init would add:\n%s", out.String())
	}

	must(t, os.WriteFile(filepath.Join(dir, "contract_test.go"), []byte("package main\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "messages"), 0o755))
	for _, f := range []string{"en.toml", "th.toml"} {
		must(t, os.WriteFile(filepath.Join(dir, "messages", f), []byte("k = \"v\"\n"), 0o644))
	}
	var out2, errW2 strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out2, &errW2); code != 0 {
		t.Fatalf("already-current must exit 0, got %d\n%s", code, errW2.String())
	}
	if strings.Contains(out2.String(), "ultra init") {
		t.Errorf("the nudge must disappear once the gate is there:\n%s", out2.String())
	}
}
