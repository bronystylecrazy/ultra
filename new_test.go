package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// combos are the shapes the scaffold promises: the paved road, the core, and
// the core plus one capability. Everything the covenant test proves, it
// proves for each of them.
var combos = []struct {
	name string
	args []string
	data scaffoldData
}{
	{"default", nil, scaffoldData{DB: true, Web: true, Auth: true}},
	{"bare", []string{"--bare"}, scaffoldData{}},
	{"bare+db", []string{"--bare", "--db"}, scaffoldData{DB: true}},
	// The combination whose boot smoke actually RUNS: no database to dial,
	// so Start happens for real and the auth-guards-ops assertion is proven
	// rather than skipped.
	{"bare+web+auth", []string{"--bare", "--web", "--auth"}, scaffoldData{Web: true, Auth: true}},
}

func testData(name string, d scaffoldData) scaffoldData {
	d.Name, d.Module = name, "example.com/"+name
	d.Version, d.GoVersion = scaffoldVersion, scaffoldGoVersion
	return d
}

func TestScaffoldValidatesInput(t *testing.T) {
	dir := t.TempDir()
	if err := scaffold(filepath.Join(dir, "x"), testData("Bad Name", scaffoldData{})); err == nil {
		t.Fatal("invalid names must be rejected")
	}
	target := filepath.Join(dir, "taken")
	os.MkdirAll(target, 0o755)
	if err := scaffold(target, testData("taken", scaffoldData{})); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("existing dirs must be refused: %v", err)
	}
}

// TestFlagsShapeTheTree pins the doctrine's structure: the core is the core,
// every other file is earned by a flag, and the layout the framework killed
// never comes back.
func TestFlagsShapeTheTree(t *testing.T) {
	core := []string{
		".gitignore", "Taskfile.yml", "config.toml", "go.mod",
		"internal/app/app.go", "main.go", "main_test.go",
	}
	web := []string{
		"spa.go", "spa_embed.go",
		"web/.gitignore", "web/package.json", "web/src/app.html",
		"web/src/lib/api/.gitkeep", "web/src/lib/api/vite.proxy.json",
		"web/src/routes/+layout.ts", "web/src/routes/+page.svelte",
		"web/svelte.config.js", "web/tsconfig.json", "web/vite.config.ts",
	}
	db := []string{
		"internal/db/migrations/00001_init.sql",
		"internal/db/queries/.gitkeep",
		"internal/db/sqlc.yaml",
	}

	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			d := testData("speedcheck", c.data)
			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}
			want := append([]string{}, core...)
			if d.Web {
				want = append(want, web...)
			}
			if d.DB {
				want = append(want, db...)
			}
			sort.Strings(want)
			got := treeOf(t, dir)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("tree mismatch\n got:\n%s\nwant:\n%s",
					strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for _, f := range got {
				b, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(b, []byte("{{")) {
					t.Errorf("%s has unrendered template markers", f)
				}
				if len(bytes.TrimSpace(b)) == 0 {
					t.Errorf("%s is empty — a file exists only when it has content", f)
				}
			}
			// The dead v0.2.0 layout, gone for good.
			for _, dead := range []string{"modules.go", "cmd", "internal/ui", "SKILL.md",
				"README.md", ".github", "e2e", "references"} {
				if _, err := os.Stat(filepath.Join(dir, dead)); err == nil {
					t.Errorf("%s must not be scaffolded", dead)
				}
			}
		})
	}
}

// TestGoModWiring: the module path, the current framework version, and the
// commented replace pair that makes a local checkout one uncomment away.
func TestGoModWiring(t *testing.T) {
	d := testData("speedcheck", scaffoldData{DB: true, Web: true, Auth: true})
	d.Module = "github.com/acme/speedcheck"
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"module github.com/acme/speedcheck",
		"go " + scaffoldGoVersion,
		"ultrastack " + scaffoldVersion,
		"ultrastack/contrib " + scaffoldVersion,
		"// replace github.com/bronystylecrazy/ultrastack => ../ultrastack",
		"// replace github.com/bronystylecrazy/ultrastack/contrib => ../ultrastack/contrib",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("go.mod missing %q:\n%s", want, b)
		}
	}
}

// TestScaffoldVersionTracksRelease keeps scaffoldVersion honest: it is the
// version a new product requires, so it must be the version the repo itself
// releases. contrib/go.mod's requirement on the kernel is bumped by the
// same release commit, which makes it the in-repo source of truth.
func TestScaffoldVersionTracksRelease(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "contrib", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`github\.com/bronystylecrazy/ultrastack (v\d+\.\d+\.\d+)`).
		FindStringSubmatch(string(b))
	if m == nil {
		t.Skip("contrib/go.mod does not pin a released kernel version")
	}
	if m[1] != scaffoldVersion {
		t.Fatalf("scaffoldVersion is %s but the repo releases %s — bump the const "+
			"in cmd/ultra/new.go with the release commit", scaffoldVersion, m[1])
	}
}

// TestVersionResolution: --version wins, and the fallback is the pin.
func TestVersionResolution(t *testing.T) {
	if got := resolveVersion("v1.2.3"); got != "v1.2.3" {
		t.Fatalf("--version must win: %s", got)
	}
	// `go test` builds the main module as "(devel)", so the pin answers.
	if got := resolveVersion(""); got != scaffoldVersion {
		t.Fatalf("a devel build must fall back to the pin: %s", got)
	}
}

func TestNewFeatureScaffold(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if code := cmdNew([]string{"feature", "zones"}, &out, &errW); code != 0 {
		t.Fatalf("new feature failed (%d): %s", code, errW.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "internal", "app", "zones", "zones.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package zones", "func Use() di.Reg", "di.Pkg("} {
		if !strings.Contains(string(b), want) {
			t.Errorf("feature file missing %q:\n%s", want, b)
		}
	}
	// No manifest files: they appear when content demands them.
	entries, _ := os.ReadDir(filepath.Join(dir, "internal", "app", "zones"))
	if len(entries) != 1 {
		t.Errorf("a new feature is ONE file, got %d", len(entries))
	}
	if !strings.Contains(out.String(), "zones.Use()") {
		t.Errorf("the next step (one line in app.go) must be printed:\n%s", out.String())
	}

	// Re-running must refuse rather than clobber.
	out.Reset()
	errW.Reset()
	if code := cmdNew([]string{"feature", "zones"}, &out, &errW); code == 0 {
		t.Error("an existing feature must not be overwritten")
	}
	// A dashed name is not a Go package name.
	if code := cmdNew([]string{"feature", "my-zones"}, &out, &errW); code == 0 {
		t.Error("dashed feature names must be rejected")
	}
}

// TestScaffoldCovenant is the scaffold dogfooding itself: for every flag
// combination, generate a product against THIS checkout and make it prove
// its own covenant — it builds, and its wiring test plus boot smoke pass.
// The frontend is never built: a dev build serves no frontend by design, so
// bun is not on this path.
func TestScaffoldCovenant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three full products; skipped in -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range combos {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := testData("speedcheck", c.data)
			dir := filepath.Join(t.TempDir(), d.Name)
			if err := scaffold(dir, d); err != nil {
				t.Fatal(err)
			}
			t.Logf("ultra new speedcheck %s\nspeedcheck/\n%s",
				strings.Join(c.args, " "), strings.Join(treeOf(t, dir), "\n"))

			sh := func(name string, args ...string) {
				t.Helper()
				cmd := exec.Command(name, args...)
				cmd.Dir = dir
				out, err := cmd.CombinedOutput()
				if err != nil {
					if offline(out) {
						t.Skipf("%s %s needs the network: %s", name, strings.Join(args, " "), out)
					}
					t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
				}
				t.Logf("%s %s\n%s", name, strings.Join(args, " "), out)
			}
			// Point the generated product at this checkout instead of the
			// network — the same two lines the go.mod comment describes.
			sh("go", "mod", "edit",
				"-replace="+modulePath+"="+repoRoot,
				"-replace="+modulePath+"/contrib="+filepath.Join(repoRoot, "contrib"))
			sh("go", "mod", "tidy")
			sh("go", "build", "./...")
			sh("go", "test", "./...")

			if d.Web {
				// The prod half of the build-tag pair only compiles once the
				// frontend build exists — stand in for bun (the real build is
				// never on this path) and prove spa_embed.go itself is sound.
				if err := os.MkdirAll(filepath.Join(dir, "web", "build"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "web", "build", "index.html"),
					[]byte("<!doctype html><title>stand-in</title>"), 0o644); err != nil {
					t.Fatal(err)
				}
				sh("go", "build", "-tags", "embedspa", "-o", os.DevNull, ".")
			}
		})
	}
}

// offline reports whether a go command failed for want of a network rather
// than for want of correct code.
func offline(out []byte) bool {
	s := string(out)
	for _, sign := range []string{"dial tcp", "no such host", "connection refused",
		"proxy.golang.org", "i/o timeout", "TLS handshake timeout"} {
		if strings.Contains(s, sign) {
			return true
		}
	}
	return false
}

// treeOf lists every file under dir, slash-separated and sorted.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}
