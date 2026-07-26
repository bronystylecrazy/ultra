package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubLatest makes the one networked step of `ultra upgrade` answer a fixed
// version: the resolver is a var precisely so the rest of the command — the
// pins, the verify loop, the revert — runs for real against fixtures.
func stubLatest(t *testing.T, ver string) {
	t.Helper()
	orig := latestFrameworkVersion
	t.Cleanup(func() { latestFrameworkVersion = orig })
	latestFrameworkVersion = func(dir string) (string, error) { return ver, nil }
}

func skipUnlessProductFixtures(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go mod tidy/build/test against fixture products")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixtures assume POSIX shell/paths")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return string(b)
}

// TestUpgradeHappyPath: no --to, so the resolver picks the target; the pins
// move, the verify loop runs for real, and the report names FROM→TO, what was
// verified, and that nothing was committed.
func TestUpgradeHappyPath(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")
	stubLatest(t, "v0.6.0")

	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", map[string]string{
		"p_test.go": "package main\n\nimport \"testing\"\n\nfunc TestPasses(t *testing.T) {}\n",
	})

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("upgrade failed (%d)\n%s\n%s", code, out.String(), errW.String())
	}
	t.Logf("ultra upgrade\n%s", out.String())

	if rl := requireLine(t, dir); !strings.Contains(rl, "v0.6.0") {
		t.Errorf("require line not trued: %q", rl)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err != nil {
		t.Errorf("go mod tidy should have produced a go.sum: %v", err)
	}
	for _, want := range []string{
		"github.com/acme/p",
		frameworkModule,
		"v0.1.0 → v0.6.0",
		"1 test package",
		"contracts  unchanged",
		"nothing was committed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	// upgrade is not a fleet operation: it never commits, never branches.
	if br := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); br != "main" {
		t.Errorf("upgrade must not switch branches, on %q", br)
	}
	if msg := gitOut(t, dir, "log", "-1", "--pretty=%s"); msg != "init" {
		t.Errorf("upgrade must not commit, last commit is %q", msg)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); !strings.Contains(st, "go.mod") {
		t.Errorf("the bump must be left in the worktree for review, got: %q", st)
	}
}

// TestUpgradeAlreadyCurrent: at the target already, nothing runs.
func TestUpgradeAlreadyCurrent(t *testing.T) {
	setupGitEnv(t)
	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.6.0", nil)

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out, &errW); code != 0 {
		t.Fatalf("already-current must exit 0, got %d\n%s", code, errW.String())
	}
	if !strings.Contains(out.String(), "already at v0.6.0") {
		t.Errorf("report should say it is already current:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		t.Errorf("already-current must not run the toolchain (a go.sum appeared)")
	}
}

// TestUpgradeCheck: the read-only verb. Behind exits 1 and names the movement,
// current exits 0 — and neither runs the toolchain or writes a byte.
func TestUpgradeCheck(t *testing.T) {
	setupGitEnv(t)
	stubLatest(t, "v0.6.0")
	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", nil)
	before := readFile(t, filepath.Join(dir, "go.mod"))

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--check"}, &out, &errW); code != 1 {
		t.Fatalf("behind must exit 1 (CI watches this), got %d\n%s%s", code, out.String(), errW.String())
	}
	for _, want := range []string{frameworkModule, "v0.1.0", "v0.6.0", "nothing was written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	if got := readFile(t, filepath.Join(dir, "go.mod")); got != before {
		t.Errorf("--check must not touch go.mod")
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		t.Errorf("--check must not run the toolchain (a go.sum appeared)")
	}

	current := newBumpProduct(t, root, "q", "github.com/acme/q", "v0.6.0", nil)
	out.Reset()
	errW.Reset()
	if code := cmdUpgrade([]string{current, "--check"}, &out, &errW); code != 0 {
		t.Fatalf("current must exit 0, got %d\n%s", code, errW.String())
	}
	if !strings.Contains(out.String(), "(current)") {
		t.Errorf("report should mark the pin current:\n%s", out.String())
	}
}

// TestUpgradeReplaceActive: a live `replace` is not a refusal (fleet bump skips
// those; from inside the product, truing the pins under an active replace is a
// deliberate, legitimate act) — but the report must be honest that the build
// verified the checkout, not the tag.
func TestUpgradeReplaceActive(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")

	root := t.TempDir()
	// A local checkout of the framework, standing in for ../ultrastack.
	stub := filepath.Join(root, "fwstub")
	must(t, os.MkdirAll(stub, 0o755))
	must(t, os.WriteFile(filepath.Join(stub, "go.mod"),
		[]byte("module "+frameworkModule+"\n\ngo 1.26\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(stub, "lib.go"),
		[]byte("package ultrastack\n\nfunc Version() string { return \"devel\" }\n"), 0o644))

	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", nil)
	gomod := filepath.Join(dir, "go.mod")
	must(t, os.WriteFile(gomod, []byte(readFile(t, gomod)+
		"\nreplace "+frameworkModule+" => ../fwstub\n"), 0o644))
	gitRun(t, dir, "commit", "-am", "replace")

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out, &errW); code != 0 {
		t.Fatalf("upgrade under a replace must succeed (%d)\n%s\n%s", code, out.String(), errW.String())
	}
	t.Logf("ultra upgrade (replace active)\n%s", out.String())

	if rl := requireLine(t, dir); !strings.Contains(rl, "v0.6.0") {
		t.Errorf("the require pin must still be trued: %q", rl)
	}
	if !strings.Contains(readFile(t, gomod), "replace "+frameworkModule+" => ../fwstub") {
		t.Errorf("the replace directive must survive untouched:\n%s", readFile(t, gomod))
	}
	if !strings.Contains(out.String(), "replace active") ||
		!strings.Contains(out.String(), "not the tag") {
		t.Errorf("the replace note must be printed:\n%s", out.String())
	}
}

// TestUpgradeRevertsOnFailure: a test that fails after the bump restores
// go.mod and go.sum from the bytes read before anything was written — by bytes,
// not by git, because a product need not be a repository.
func TestUpgradeRevertsOnFailure(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")

	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", map[string]string{
		"boom_test.go": "package main\n\nimport \"testing\"\n\nfunc TestBoom(t *testing.T) { t.Fatal(\"boom\") }\n",
	})
	before := readFile(t, filepath.Join(dir, "go.mod"))

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out, &errW); code != 1 {
		t.Fatalf("a failing test must exit 1, got %d\n%s\n%s", code, out.String(), errW.String())
	}
	t.Logf("ultra upgrade (failure)\n%s%s", out.String(), errW.String())

	if got := readFile(t, filepath.Join(dir, "go.mod")); got != before {
		t.Errorf("go.mod must be restored byte-for-byte:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.sum")); err == nil {
		t.Errorf("a go.sum that did not exist before must not survive the revert")
	}
	if !strings.Contains(errW.String(), "go test ./... failed") ||
		!strings.Contains(errW.String(), "restored to v0.1.0") {
		t.Errorf("the failure must name the step and the revert:\n%s", errW.String())
	}
	if !strings.Contains(errW.String(), "TestBoom") {
		t.Errorf("the failure output head must be shown:\n%s", errW.String())
	}
}

// TestUpgradeRefreshesContracts: the drift gate is the ONE failure an upgrade
// is allowed to fix. The fixture stands in for a scaffolded product cheaply —
// its "contract" is derived from the framework version, so the upgrade itself
// changes it, the gate fails, and the refresh (the product's own `openapi`
// command) makes it pass.
func TestUpgradeRefreshesContracts(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")

	main := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\n\tust \"" + frameworkModule + "\"\n)\n\n" +
		"func contract() string { return \"openapi for \" + ust.Version() + \"\\n\" }\n\n" +
		"func main() {\n\tif len(os.Args) > 1 && os.Args[1] == \"openapi\" {\n\t\tfmt.Print(contract())\n\t}\n}\n"
	drift := "package main\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"func TestContractDrift(t *testing.T) {\n" +
		"\tb, err := os.ReadFile(\"openapi.json\")\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n" +
		"\tif string(b) != contract() {\n\t\tt.Fatalf(\"the API contract changed but openapi.json was not regenerated\\nrefresh:\\n  task contracts\")\n\t}\n}\n"

	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", map[string]string{
		"main.go":          main,
		"contract_test.go": drift,
		"openapi.json":     "openapi for v0.1.0\n",
	})

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0"}, &out, &errW); code != 0 {
		t.Fatalf("contract drift must be refreshed, not fatal (%d)\n%s\n%s", code, out.String(), errW.String())
	}
	t.Logf("ultra upgrade (drift refreshed)\n%s", out.String())

	if got := readFile(t, filepath.Join(dir, "openapi.json")); got != "openapi for v0.6.0\n" {
		t.Errorf("the contract artifact was not regenerated: %q", got)
	}
	for _, want := range []string{
		"contracts  refreshed by the upgrade — review the diff before committing",
		"M openapi.json",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
}

// TestUpgradeDry writes nothing and says what it would do.
func TestUpgradeDry(t *testing.T) {
	setupGitEnv(t)
	root := t.TempDir()
	dir := newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", nil)
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(
		"module github.com/acme/p\n\ngo 1.26\n\nrequire (\n\t"+frameworkModule+" v0.1.0\n\t"+
			frameworkModule+"/contrib v0.1.0\n)\n\n// replace "+frameworkModule+" => ../ultrastack\n"), 0o644))
	before := readFile(t, filepath.Join(dir, "go.mod"))

	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir, "--to", "v0.6.0", "--all", "--dry"}, &out, &errW); code != 0 {
		t.Fatalf("--dry must exit 0, got %d\n%s", code, errW.String())
	}
	t.Logf("ultra upgrade --dry\n%s", out.String())

	if got := readFile(t, filepath.Join(dir, "go.mod")); got != before {
		t.Errorf("--dry must not write go.mod")
	}
	for _, want := range []string{
		frameworkModule + "/contrib  v0.1.0 → v0.6.0",
		"nothing was written",
		"go get -u ./...",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry plan is missing %q:\n%s", want, out.String())
		}
	}
	// A COMMENTED replace is the scaffold's default and must not be reported.
	if strings.Contains(out.String(), "replace active") {
		t.Errorf("a commented-out replace is not active:\n%s", out.String())
	}
}

// TestUpgradeNotAModule points at the fleet form instead of guessing.
func TestUpgradeNotAModule(t *testing.T) {
	var out, errW strings.Builder
	if code := cmdUpgrade([]string{t.TempDir()}, &out, &errW); code != 2 {
		t.Fatalf("a dir with no go.mod must be a usage error, got %d", code)
	}
	if !strings.Contains(errW.String(), "ultra fleet bump") {
		t.Errorf("the error must point at the fleet form:\n%s", errW.String())
	}
	// A module that does not use the framework has nothing to upgrade.
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.26\n"), 0o644))
	errW.Reset()
	if code := cmdUpgrade([]string{dir}, &out, &errW); code != 1 {
		t.Errorf("a non-ultrastack module must exit 1, got %d", code)
	}
	errW.Reset()
	if code := cmdUpgrade([]string{dir, "--to", "1.2"}, &out, &errW); code != 2 {
		t.Errorf("a malformed --to must be a usage error, got %d", code)
	}
}

// TestLatestOf: the resolver's parsing half, without a network.
func TestLatestOf(t *testing.T) {
	out := frameworkModule + " v0.1.0 v0.9.0 v0.10.0 v0.11.0-rc1\n"
	if got := latestOf(out); got != "v0.10.0" {
		t.Errorf("want the highest release tag (pre-releases skipped), got %q", got)
	}
	if got := latestOf(frameworkModule + "\n"); got != "" {
		t.Errorf("no versions must resolve to nothing, got %q", got)
	}
}

// TestRequireEntry guards the go.mod line reader both commands share: an
// indirect marker is not a version, and a replace arrow is not a require.
func TestRequireEntry(t *testing.T) {
	mods := upgradeModules
	cases := []struct{ line, mod, ver string }{
		{"require " + frameworkModule + " v0.1.0", frameworkModule, "v0.1.0"},
		{"\t" + frameworkModule + "/contrib v0.2.0 // indirect", frameworkModule + "/contrib", "v0.2.0"},
		{"\t" + frameworkModule + "/analyzer v0.3.0", frameworkModule + "/analyzer", "v0.3.0"},
		{"// replace " + frameworkModule + " => ../ultrastack", "", ""},
		{"replace " + frameworkModule + " => ../ultrastack", "", ""},
		{"\t" + frameworkModule + " => ../ultrastack", "", ""},
		{"\tgithub.com/other/dep v1.0.0", "", ""},
	}
	for _, c := range cases {
		mod, ver, ok := requireEntry(c.line, mods)
		if c.mod == "" {
			if ok {
				t.Errorf("%q must not read as a require (%s %s)", c.line, mod, ver)
			}
			continue
		}
		if !ok || mod != c.mod || ver != c.ver {
			t.Errorf("%q → %s %s (ok=%v), want %s %s", c.line, mod, ver, ok, c.mod, c.ver)
		}
	}
}

// TestDriftOnly: only the scaffolded contract gates may be refreshed away —
// any other failure, and anything that did not even build, is a real failure.
func TestDriftOnly(t *testing.T) {
	drift := "--- FAIL: TestContractDrift (0.01s)\n    contract_test.go:64: the API contract changed\nFAIL\n"
	if !driftOnly(drift) {
		t.Error("a lone contract gate failure is drift")
	}
	if !driftOnly(drift + "--- FAIL: TestClientDrift/api (0.00s)\n") {
		t.Error("both gates (subtests folded into the parent) are still drift")
	}
	if driftOnly(drift + "--- FAIL: TestWiring (0.00s)\n") {
		t.Error("a wiring failure alongside drift is a real failure")
	}
	if driftOnly("FAIL\tgithub.com/acme/p [build failed]\n") {
		t.Error("a build failure is never drift")
	}
	if driftOnly("ok  \tgithub.com/acme/p\t0.1s\n") {
		t.Error("a passing run is not drift")
	}
}
