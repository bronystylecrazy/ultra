package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vendored skill mirrors the product's pin: replace directives vendor
// the live checkout, the manifest marks machine ownership, and --check
// gates version skew.
func TestSkillInstall(t *testing.T) {
	root := repoRoot(t)
	dir := t.TempDir()
	gomod := "module example.com/p\n\ngo 1.24\n\nrequire github.com/bronystylecrazy/ultrastack v0.0.0\n\nreplace github.com/bronystylecrazy/ultrastack => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errW bytes.Buffer
	if code := cmdSkillInstall([]string{dir}, &out, &errW); code != 0 {
		t.Fatalf("install failed (%d): %s", code, errW.String())
	}
	for _, want := range []string{
		filepath.Join(dir, skillDest, "SKILL.md"),
		filepath.Join(dir, skillDest, "references", "product.md"),
		filepath.Join(dir, skillDest, skillManifest),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("missing %s after install", want)
		}
	}

	out.Reset()
	if code := cmdSkillInstall([]string{"--check", dir}, &out, &errW); code != 0 {
		t.Fatalf("--check should pass right after install: %s", errW.String())
	}

	// Version skew: pretend go.mod moved (replace removed, pin changed).
	gomod2 := "module example.com/p\n\ngo 1.24\n\nrequire github.com/bronystylecrazy/ultrastack v9.9.9\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod2), 0o644); err != nil {
		t.Fatal(err)
	}
	errW.Reset()
	if code := cmdSkillInstall([]string{"--check", dir}, &out, &errW); code == 0 {
		t.Fatal("--check must fail on version skew")
	}
	if !strings.Contains(errW.String(), "v9.9.9") {
		t.Fatalf("skew message must name the pin: %s", errW.String())
	}

	// Hand-managed dir (no manifest) refuses without --force.
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "go.mod"), []byte(gomod), 0o644)
	os.MkdirAll(filepath.Join(dir2, skillDest), 0o755)
	os.WriteFile(filepath.Join(dir2, skillDest, "SKILL.md"), []byte("mine"), 0o644)
	errW.Reset()
	if code := cmdSkillInstall([]string{dir2}, &out, &errW); code == 0 {
		t.Fatal("must refuse a manifest-less skill dir")
	}
	if code := cmdSkillInstall([]string{"--force", dir2}, &out, &errW); code != 0 {
		t.Fatalf("--force must replace: %s", errW.String())
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	root := filepath.Dir(filepath.Dir(wd)) // cmd/ultra -> repo root
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
		t.Skip("repo root SKILL.md not found")
	}
	return root
}
