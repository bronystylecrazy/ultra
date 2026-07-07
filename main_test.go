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
