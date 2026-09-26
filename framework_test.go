package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
)

// The framework lives in its own repository, so the tests that read or build
// against it find it one of two ways: ULTRASTACK_DIR names a checkout (a
// framework change and a CLI change proven together), or — without one — the
// release this CLI scaffolds, scaffoldVersion, out of the module cache.

// frameworkDir is the source of one framework module ("" is the root module,
// "contrib" the contrib module, …).
func frameworkDir(t *testing.T, sub string) string {
	t.Helper()
	if root := os.Getenv("ULTRASTACK_DIR"); root != "" {
		abs, err := filepath.Abs(filepath.Join(root, sub))
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	cmd := exec.Command("go", "mod", "download", "-json", path.Join(modulePath, sub)+"@"+scaffoldVersion)
	cmd.Dir = t.TempDir() // outside this module: nothing here is required by it
	out, err := cmd.Output()
	var m struct{ Dir, Error string }
	if jerr := json.Unmarshal(out, &m); err != nil || jerr != nil || m.Dir == "" {
		t.Skipf("no ULTRASTACK_DIR and %s@%s is not downloadable: %v %s", path.Join(modulePath, sub), scaffoldVersion, err, m.Error)
	}
	return m.Dir
}

// frameworkCheckout is the ULTRASTACK_DIR checkout, for the tests that only
// mean something on one (the docs drift gate).
func frameworkCheckout(t *testing.T) string {
	t.Helper()
	root := os.Getenv("ULTRASTACK_DIR")
	if root == "" {
		t.Skip("needs a framework checkout: set ULTRASTACK_DIR")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// checkoutReplaces is the `go mod edit` that points a scaffolded product at
// the ULTRASTACK_DIR checkout. Without one it is nil: the product already
// requires scaffoldVersion, which is the release under test.
func checkoutReplaces(t *testing.T) []string {
	t.Helper()
	root := os.Getenv("ULTRASTACK_DIR")
	if root == "" {
		return nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"mod", "edit", "-replace=" + modulePath + "=" + root}
	for _, m := range []string{"contrib", "web", "mqtt"} {
		args = append(args, "-replace="+modulePath+"/"+m+"="+filepath.Join(root, m))
	}
	return args
}
