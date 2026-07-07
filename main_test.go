package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestExplainKnownCode(t *testing.T) {
	code, out, _ := runCLI(t, "explain", "DI0101")
	if code != 0 || !strings.Contains(out, "captive") && !strings.Contains(out, "singleton") {
		t.Fatalf("explain DI0101: code=%d out=%q", code, out)
	}
}

func TestExplainUnknownCode(t *testing.T) {
	code, _, errOut := runCLI(t, "explain", "DI9999")
	if code != 1 || !strings.Contains(errOut, "unknown code") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestCodesListsRegistry(t *testing.T) {
	code, out, _ := runCLI(t, "codes")
	if code != 0 {
		t.Fatal("codes must succeed")
	}
	for _, want := range []string{"DI0001", "DI0101", "DI0203"} {
		if !strings.Contains(out, want) {
			t.Errorf("codes output missing %s", want)
		}
	}
}

func TestDiff(t *testing.T) {
	oldG := `*app.Checkout (singleton, module -) <- [app.Config, app.gateway]
app.Config (singleton, module config) <- []
*app.LegacyMailer (singleton, module mail) <- [app.Config]
`
	newG := `*app.Checkout (singleton, module -) <- [*stripe.Client, app.Config, app.gateway]
app.Config (singleton, module config) <- []
*stripe.Client (singleton, module payments) <- [app.Config]
`
	dir := t.TempDir()
	oldF, newF := filepath.Join(dir, "old.graph"), filepath.Join(dir, "new.graph")
	os.WriteFile(oldF, []byte(oldG), 0o644)
	os.WriteFile(newF, []byte(newG), 0o644)

	code, out, _ := runCLI(t, "diff", oldF, newF)
	if code != 1 {
		t.Fatalf("differing graphs must exit 1, got %d", code)
	}
	for _, want := range []string{
		"3 change(s)",
		"+ *stripe.Client (singleton, module payments)",
		"- *app.LegacyMailer (singleton, module mail)",
		"~ *app.Checkout (singleton, module -)",
		"+ [*stripe.Client, app.Config, app.gateway]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q:\n%s", want, out)
		}
	}

	code, out, _ = runCLI(t, "diff", oldF, oldF)
	if code != 0 || !strings.Contains(out, "identical") {
		t.Fatalf("identical graphs must exit 0: code=%d out=%q", code, out)
	}
}

// ultra vet delegates to an installed ultravet binary, passing args and
// exit code through.
func TestVetDelegatesToBinary(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "ultravet")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"vet-args: $@\"\nexit 3\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errW strings.Builder
	code := run([]string{"vet", "./...", "-fix"}, &out, &errW)
	if code != 3 {
		t.Fatalf("exit code must pass through: %d (%s)", code, errW.String())
	}
	if !strings.Contains(out.String(), "vet-args: ./... -fix") {
		t.Fatalf("args must pass through: %q", out.String())
	}
}

// Fleet discovery: products in, the framework and unrelated modules out.
func TestDiscoverFleet(t *testing.T) {
	root := t.TempDir()
	write := func(dir, mod string) {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
		os.WriteFile(filepath.Join(root, dir, "go.mod"), []byte(mod), 0o644)
	}
	write("speedcheck", "module github.com/acme/speedcheck\n\ngo 1.26\n\nrequire github.com/bronystylecrazy/ultrastack v0.2.0\n")
	write("legacy", "module github.com/acme/legacy\n\ngo 1.26\n\nrequire github.com/bronystylecrazy/ultrastack v0.1.0\n")
	write("framework", "module github.com/bronystylecrazy/ultrastack\n\ngo 1.26\n")
	write("framework-sub", "module github.com/bronystylecrazy/ultrastack/contrib\n\ngo 1.26\n\nrequire github.com/bronystylecrazy/ultrastack v0.2.0\n")
	write("unrelated", "module github.com/acme/other\n\ngo 1.26\n")
	write("dev", "module github.com/acme/dev\n\ngo 1.26\n\nrequire github.com/bronystylecrazy/ultrastack v0.0.0\n\nreplace github.com/bronystylecrazy/ultrastack => ../fw\n")

	got := discoverFleet(root)
	if len(got) != 3 {
		t.Fatalf("want 3 products, got %d: %+v", len(got), got)
	}
	byMod := map[string]fleetProduct{}
	for _, p := range got {
		byMod[p.Module] = p
	}
	if byMod["github.com/acme/speedcheck"].Version != "v0.2.0" ||
		byMod["github.com/acme/legacy"].Version != "v0.1.0" {
		t.Fatalf("versions: %+v", byMod)
	}
	if !byMod["github.com/acme/dev"].Replaced {
		t.Fatal("replace must be flagged")
	}
	if _, bad := byMod["github.com/bronystylecrazy/ultrastack/contrib"]; bad {
		t.Fatal("framework submodules are not products")
	}
}
