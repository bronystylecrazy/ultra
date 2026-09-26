package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureNotes = `# v0.6.0

The requirements spine, plus the healthz-shaped exemplar entry.

## Behavior changes

- /healthz answers probes unauthenticated now — a boot test pinning the old 401 breaks.

## Also in this release

- other work that moves nothing a product observes
`

// proxyShipNotes rewrites one version's zip in the fixture proxy so the
// module SHIPS releases/<ver>.md — proving the module-zip → module-cache
// read path for real, not through a seam.
func proxyShipNotes(t *testing.T, ver, notes string) {
	t.Helper()
	proxy := strings.TrimPrefix(os.Getenv("GOPROXY"), "file://")
	writeModuleZip(t, filepath.Join(proxy, frameworkModule, "@v", ver+".zip"), ver,
		map[string]string{"releases/" + ver + ".md": notes})
}

// The happy path, end to end: the notes travel inside the module zip, land
// in the module cache via `go mod download`, and print — behavior changes
// highlighted, review warning attached — before the gate runs. The fixture
// product has NO requirements/: notes are for every product, opted-in or
// not (the dormancy covenant does not gate release communication).
func TestUpgradeReadsReleaseNotesFromTheModuleZip(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")
	proxyShipNotes(t, "v0.6.0", fixtureNotes)
	stubLatest(t, "v0.6.0")

	dir := newBumpProduct(t, t.TempDir(), "p", "github.com/acme/p", "v0.1.0", map[string]string{
		"p_test.go": "package main\n\nimport \"testing\"\n\nfunc TestPasses(t *testing.T) {}\n",
	})
	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("upgrade: exit %d\n%s\n%s", code, out.String(), errW.String())
	}
	t.Logf("ultra upgrade\n%s", out.String())
	for _, want := range []string{
		"release notes — v0.6.0 (releases/v0.6.0.md, shipped in the module)",
		"a boot test pinning the old 401 breaks",
		"review them before trusting a",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("upgrade output missing %q:\n%s", want, out.String())
		}
	}
	// And the notes printed BEFORE the verification report.
	if strings.Index(out.String(), "release notes —") > strings.Index(out.String(), "verified") {
		t.Error("the notes must print before the gate's report")
	}
}

// When the gate then FAILS, the failure output points at the behavior
// changes first — a listed change may be exactly what the red test pins.
func TestUpgradeGateFailurePointsAtBehaviorChanges(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")
	proxyShipNotes(t, "v0.6.0", fixtureNotes)
	stubLatest(t, "v0.6.0")

	dir := newBumpProduct(t, t.TempDir(), "p", "github.com/acme/p", "v0.1.0", map[string]string{
		"p_test.go": "package main\n\nimport \"testing\"\n\nfunc TestPinsOldBehavior(t *testing.T) { t.Fatal(\"pinned\") }\n",
	})
	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir}, &out, &errW); code != 1 {
		t.Fatalf("a red gate must fail the upgrade, got %d\n%s", code, out.String())
	}
	for _, want := range []string{"releases/v0.6.0.md lists behavior changes", "your test pins"} {
		if !strings.Contains(errW.String(), want) {
			t.Errorf("the failure must point at the behavior changes (%q):\n%s", want, errW.String())
		}
	}
	if rl := requireLine(t, dir); !strings.Contains(rl, "v0.1.0") {
		t.Errorf("the failed upgrade must still restore go.mod: %q", rl)
	}
}

// A tag that shipped no notes gets one honest line — old releases have
// none, and that is never an error.
func TestUpgradeNoNotesShippedIsHonest(t *testing.T) {
	skipUnlessProductFixtures(t)
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")
	stubLatest(t, "v0.6.0")

	dir := newBumpProduct(t, t.TempDir(), "p", "github.com/acme/p", "v0.1.0", map[string]string{
		"p_test.go": "package main\n\nimport \"testing\"\n\nfunc TestPasses(t *testing.T) {}\n",
	})
	var out, errW strings.Builder
	if code := cmdUpgrade([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("no notes must never fail an upgrade: exit %d\n%s", code, errW.String())
	}
	if !strings.Contains(out.String(), "no release notes shipped with v0.6.0") {
		t.Errorf("the honest line is missing:\n%s", out.String())
	}
}

// The renderer's contract, fast: bullets in ## Behavior changes count and
// earn the warning; "None." prose lists nothing and earns silence.
func TestRenderReleaseNotesBehaviorDetection(t *testing.T) {
	var out strings.Builder
	if !renderReleaseNotes(&out, "v9.9.9", fixtureNotes) {
		t.Error("a bulleted Behavior changes section must report true")
	}
	if !strings.Contains(out.String(), "review them before trusting a") {
		t.Errorf("the warning is missing:\n%s", out.String())
	}

	var quiet strings.Builder
	none := "# v9.9.9\n\n## Behavior changes\n\nNone. Everything here is internal.\n"
	if renderReleaseNotes(&quiet, "v9.9.9", none) {
		t.Error("prose 'None.' must not count as a behavior change")
	}
	if strings.Contains(quiet.String(), "review them before trusting") {
		t.Errorf("no warning without entries:\n%s", quiet.String())
	}
}
