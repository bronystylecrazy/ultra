package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldValidatesInput(t *testing.T) {
	dir := t.TempDir()
	if err := scaffold(filepath.Join(dir, "x"), "Bad Name", "example.com/x"); err == nil {
		t.Fatal("invalid names must be rejected")
	}
	target := filepath.Join(dir, "taken")
	os.MkdirAll(target, 0o755)
	if err := scaffold(target, "taken", "example.com/taken"); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("existing dirs must be refused: %v", err)
	}
}

func TestScaffoldRendersAllFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "speedcheck")
	if err := scaffold(dir, "speedcheck", "github.com/acme/speedcheck"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"main.go", "modules.go", "config.toml", "main_test.go", "go.mod", "SKILL.md", ".gitignore"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if strings.Contains(string(b), "{{") {
			t.Errorf("%s has unrendered template markers", f)
		}
	}
	gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	if !strings.Contains(string(gomod), "module github.com/acme/speedcheck") ||
		!strings.Contains(string(gomod), "ultrastack "+scaffoldVersion) {
		t.Fatalf("go.mod wrong:\n%s", gomod)
	}
}

// The acceptance bar: a generated product compiles, its wiring test (the
// covenant) passes, and its boot e2e serves — proven against the local
// checkout so the scaffold's own CI guards every template change.
func TestGeneratedProductPassesItsOwnTests(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a full product; skipped in -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "speedcheck")
	if err := scaffold(dir, "speedcheck", "example.com/speedcheck"); err != nil {
		t.Fatal(err)
	}

	sh := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	// Point the generated product at this checkout instead of the network.
	sh("go", "mod", "edit",
		"-replace=github.com/bronystylecrazy/ultrastack="+repoRoot,
		"-replace=github.com/bronystylecrazy/ultrastack/contrib="+filepath.Join(repoRoot, "contrib"))
	sh("go", "mod", "tidy")
	sh("go", "test", "./...")
}
