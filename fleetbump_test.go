package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// chmodTree makes a subtree writable again — the module cache is written
// read-only, which otherwise defeats t.TempDir cleanup.
func chmodTree(root string, mode os.FileMode) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			os.Chmod(p, mode)
		}
		return nil
	})
}

// setupFrameworkProxy stands up an offline file:// GOPROXY serving a trivial
// fake framework at the given versions and points the go toolchain at it
// (isolated module cache, checksum db off) — so a real `go mod tidy` + build
// resolve the framework without a network, no `replace` needed. That matters:
// bump SKIPS replaced repos, so the happy path must resolve the true require.
func setupFrameworkProxy(t *testing.T, versions ...string) {
	t.Helper()
	proxy := t.TempDir()
	vdir := filepath.Join(proxy, frameworkModule, "@v")
	must(t, os.MkdirAll(vdir, 0o755))
	var list strings.Builder
	for _, v := range versions {
		must(t, os.WriteFile(filepath.Join(vdir, v+".info"), []byte(`{"Version":"`+v+`"}`), 0o644))
		must(t, os.WriteFile(filepath.Join(vdir, v+".mod"), []byte("module "+frameworkModule+"\n\ngo 1.26\n"), 0o644))
		writeModuleZip(t, filepath.Join(vdir, v+".zip"), v)
		list.WriteString(v + "\n")
	}
	must(t, os.WriteFile(filepath.Join(vdir, "list"), []byte(list.String()), 0o644))

	modcache := t.TempDir()
	t.Cleanup(func() { chmodTree(modcache, 0o755) })
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(proxy))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOMODCACHE", modcache)
}

func writeModuleZip(t *testing.T, path, ver string) {
	t.Helper()
	f, err := os.Create(path)
	must(t, err)
	defer f.Close()
	zw := zip.NewWriter(f)
	add := func(name, body string) {
		w, err := zw.Create(frameworkModule + "@" + ver + "/" + name)
		must(t, err)
		w.Write([]byte(body))
	}
	add("go.mod", "module "+frameworkModule+"\n\ngo 1.26\n")
	add("lib.go", "package ultrastack\n\nfunc Version() string { return \""+ver+"\" }\n")
	must(t, zw.Close())
}

// neutralizes ambient git config and provides a commit identity so both the
// fixtures and bump's own commits work regardless of the host's ~/.gitconfig.
func setupGitEnv(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	must(t, os.WriteFile(empty, nil, 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_SYSTEM", empty)
	t.Setenv("GIT_AUTHOR_NAME", "ultra test")
	t.Setenv("GIT_AUTHOR_EMAIL", "ultra@test.local")
	t.Setenv("GIT_COMMITTER_NAME", "ultra test")
	t.Setenv("GIT_COMMITTER_EMAIL", "ultra@test.local")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newBumpProduct writes a product git repo requiring the framework at ver and
// importing it (so `go mod tidy` keeps the require and the proxy resolves it).
// extra files let a caller inject a compile error.
func newBumpProduct(t *testing.T, root, name, module, ver string, extra map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	must(t, os.MkdirAll(dir, 0o755))
	gomod := "module " + module + "\n\ngo 1.26\n\nrequire " + frameworkModule + " " + ver + "\n"
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644))
	main := "package main\n\nimport ust \"" + frameworkModule + "\"\n\nfunc main() { _ = ust.Version() }\n"
	must(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o644))
	for fn, body := range extra {
		must(t, os.WriteFile(filepath.Join(dir, fn), []byte(body), 0o644))
	}
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")
	return dir
}

func requireLine(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	must(t, err)
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, frameworkModule) && strings.Contains(l, " v") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// TestFleetBump exercises the full bump matrix in a single run: a real bump, an
// already-current skip, a replace skip, a dirty skip, and a failed-build repo
// that must be reverted (branch deleted, go.mod restored, back on main).
func TestFleetBump(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go mod tidy/build against fixture repos")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixtures assume POSIX shell/paths")
	}
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")

	root := t.TempDir()
	// bumpable: behind, clean, builds → bumped.
	bumpDir := newBumpProduct(t, root, "bumpable", "github.com/acme/bumpable", "v0.1.0", nil)
	// current: already at target → already-current (returns before any build).
	newBumpProduct(t, root, "current", "github.com/acme/current", "v0.6.0", nil)
	// broken: behind but a type error fails the build → FAILED + revert.
	brokenDir := newBumpProduct(t, root, "broken", "github.com/acme/broken", "v0.1.0",
		map[string]string{"bad.go": "package main\n\nvar _ int = \"not an int\"\n"})
	// dirty: behind but uncommitted change → skipped-dirty.
	dirtyDir := newBumpProduct(t, root, "dirty", "github.com/acme/dirty", "v0.1.0", nil)
	must(t, os.WriteFile(filepath.Join(dirtyDir, "scratch.txt"), []byte("wip"), 0o644))
	// replaced: a local replace → skipped-replace (dev checkout).
	replDir := filepath.Join(root, "replaced")
	must(t, os.MkdirAll(replDir, 0o755))
	must(t, os.WriteFile(filepath.Join(replDir, "go.mod"),
		[]byte("module github.com/acme/replaced\n\ngo 1.26\n\nrequire "+frameworkModule+" v0.1.0\n\nreplace "+frameworkModule+" => ../fwstub\n"), 0o644))

	var out, errW strings.Builder
	code := fleetBump([]string{root, "--to", "v0.6.0", "--json"}, &out, &errW)
	// broken FAILED → non-zero exit.
	if code != 1 {
		t.Fatalf("a FAILED repo must make exit non-zero; got %d\n%s\n%s", code, out.String(), errW.String())
	}

	var results []bumpResult
	must(t, json.Unmarshal([]byte(out.String()), &results))
	byMod := map[string]bumpResult{}
	for _, r := range results {
		byMod[r.Module] = r
	}
	want := map[string]string{
		"github.com/acme/bumpable": "bumped",
		"github.com/acme/current":  "already-current",
		"github.com/acme/broken":   "FAILED",
		"github.com/acme/dirty":    "skipped-dirty",
		"github.com/acme/replaced": "skipped-replace",
	}
	for mod, status := range want {
		if byMod[mod].Status != status {
			t.Errorf("%s: want status %q, got %q (detail: %s)", mod, status, byMod[mod].Status, byMod[mod].Detail)
		}
	}

	// bumpable: branch + commit exist, require rewritten, tidy ran (go.sum).
	if br := gitOut(t, bumpDir, "rev-parse", "--abbrev-ref", "HEAD"); br != "ultra-bump-v0.6.0" {
		t.Errorf("bumpable should be on the bump branch, got %q", br)
	}
	if msg := gitOut(t, bumpDir, "log", "-1", "--pretty=%s"); !strings.Contains(msg, "v0.1.0") || !strings.Contains(msg, "v0.6.0") {
		t.Errorf("commit message should record old→new: %q", msg)
	}
	if rl := requireLine(t, bumpDir); !strings.Contains(rl, "v0.6.0") {
		t.Errorf("require line not rewritten: %q", rl)
	}
	if _, err := os.Stat(filepath.Join(bumpDir, "go.sum")); err != nil {
		t.Errorf("go mod tidy should have produced a go.sum: %v", err)
	}

	// broken: reverted — back on main, bump branch gone, go.mod untouched.
	if br := gitOut(t, brokenDir, "rev-parse", "--abbrev-ref", "HEAD"); br != "main" {
		t.Errorf("failed bump must revert to main, on %q", br)
	}
	branches := gitOut(t, brokenDir, "branch", "--list", "ultra-bump-v0.6.0")
	if strings.TrimSpace(branches) != "" {
		t.Errorf("failed bump must delete its branch, found: %q", branches)
	}
	if rl := requireLine(t, brokenDir); !strings.Contains(rl, "v0.1.0") {
		t.Errorf("failed bump must restore go.mod, got: %q", rl)
	}
	if st := gitOut(t, brokenDir, "status", "--porcelain"); st != "" {
		t.Errorf("failed bump must leave a clean worktree, got: %q", st)
	}
}

// TestFleetBumpPushReported: --push against a repo with no remote is a bump
// that succeeds locally and reports the push failure as a note, not a hard
// error (the branch + commit already exist). This exercises the push command
// path without needing a real remote.
func TestFleetBumpPushReported(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go mod tidy/build against fixture repos")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixtures assume POSIX shell/paths")
	}
	setupGitEnv(t)
	setupFrameworkProxy(t, "v0.1.0", "v0.6.0")

	root := t.TempDir()
	newBumpProduct(t, root, "p", "github.com/acme/p", "v0.1.0", nil)

	var out, errW strings.Builder
	code := fleetBump([]string{root, "--to", "v0.6.0", "--push", "--json"}, &out, &errW)
	if code != 0 {
		t.Fatalf("a clean bump with a failed push must still exit 0, got %d\n%s", code, out.String())
	}
	var results []bumpResult
	must(t, json.Unmarshal([]byte(out.String()), &results))
	if len(results) != 1 || results[0].Status != "bumped" {
		t.Fatalf("want one bumped result: %+v", results)
	}
	if results[0].Pushed {
		t.Errorf("push cannot have succeeded without a remote")
	}
	if !strings.Contains(results[0].Detail, "push failed") {
		t.Errorf("push failure must be reported as a note, got detail: %q", results[0].Detail)
	}
}

// TestFleetBumpUsage: --to is required; a bad version is a usage error.
func TestFleetBumpUsage(t *testing.T) {
	var out, errW strings.Builder
	if code := fleetBump([]string{"."}, &out, &errW); code != 2 {
		t.Errorf("missing --to must be a usage error, got %d", code)
	}
	errW.Reset()
	if code := fleetBump([]string{".", "--to", "1.2"}, &out, &errW); code != 2 {
		t.Errorf("malformed --to must be a usage error, got %d", code)
	}
}

// TestParseSemver guards the version comparison bump relies on.
func TestParseSemver(t *testing.T) {
	a, _ := parseSemver("v0.1.0")
	b, _ := parseSemver("v0.6.0")
	if !a.less(b) || b.less(a) {
		t.Fatalf("v0.1.0 must be < v0.6.0")
	}
	if _, ok := parseSemver("v1.2"); ok {
		t.Errorf("v1.2 is not a full triple")
	}
	c, ok := parseSemver("v1.2.3-rc1")
	if !ok || c.patch != 3 {
		t.Errorf("pre-release suffix should be tolerated: %+v ok=%v", c, ok)
	}
}
