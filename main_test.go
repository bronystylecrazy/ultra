package main

import (
	"os"
	"os/exec"
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

// A preset code is not a typo: the companion deliberately links no preset, so
// the miss has to send the reader to the binary that CAN answer.
func TestExplainPresetCodeRedirectsToTheProductBinary(t *testing.T) {
	code, _, errOut := runCLI(t, "explain", "PG0101")
	if code != 1 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{"preset code", "./app explain PG0101"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("must mention %q, got: %s", want, errOut)
		}
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
	os.WriteFile(script, []byte(vetStub("#!/bin/sh\necho \"vet-args: $@\"\nexit 3\n")), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errW strings.Builder
	code := run([]string{"vet", "./...", "-fix"}, &out, &errW)
	if code != 3 {
		t.Fatalf("exit code must pass through: %d (%s)", code, errW.String())
	}
	if !strings.Contains(out.String(), "vet-args: ./... -fix") {
		t.Fatalf("args must pass through: %q", out.String())
	}

	// A bare flag list still means "this module": -fix alone is -fix ./...
	out.Reset()
	errW.Reset()
	run([]string{"vet", "-fix"}, &out, &errW)
	if !strings.Contains(out.String(), "vet-args: -fix ./...") {
		t.Fatalf("-fix alone must default the patterns: %q", out.String())
	}
}

// TestVetOutputFlagTranslation pins the wire contract between `ultra vet` and
// the analyzer. --json cannot pass through verbatim: on the analyzer's own
// command line -json belongs to the go/analysis flat driver that `go vet
// -vettool` and gopls speak, so the structured document rides -format=json.
// --format's value must also fold into one token, or the default-patterns rule
// would mistake "github" for a package.
func TestVetOutputFlagTranslation(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "ultravet")
	os.WriteFile(script, []byte(vetStub("#!/bin/sh\necho \"vet-args: $@\"\nexit 0\n")), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"vet", "--json"}, "vet-args: -format=json ./..."},
		{[]string{"vet", "--json", "./..."}, "vet-args: -format=json ./..."},
		{[]string{"vet", "-json", "./..."}, "vet-args: -format=json ./..."},
		{[]string{"vet", "--format", "github"}, "vet-args: -format=github ./..."},
		{[]string{"vet", "--format", "github", "./..."}, "vet-args: -format=github ./..."},
		{[]string{"vet", "-format=github", "./..."}, "vet-args: -format=github ./..."},
		// No format flag: the analyzer decides, so GITHUB_ACTIONS auto-detection
		// lives in exactly one place.
		{[]string{"vet", "./..."}, "vet-args: ./..."},
	}
	for _, c := range cases {
		var out, errW strings.Builder
		run(c.args, &out, &errW)
		if got := strings.TrimSpace(out.String()); got != c.want {
			t.Errorf("ultra %v → %q, want %q", c.args, got, c.want)
		}
	}
}

// TestVetFixRewritesFile is the CLI half of the -fix covenant: `ultra vet -fix`
// over a real temp module, with the real analyzer, leaves the file repaired.
func TestVetFixRewritesFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the analyzer binary")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Build the real ultravet (its own module) and put it first on PATH.
	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "ultravet"), "./cmd/ultravet")
	build.Dir = filepath.Join(root, "analyzer")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ultravet: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A throwaway product wired against this checkout: NewDB needs a *Config
	// nothing provides, and exactly one local constructor supplies it.
	mod := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(mod, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixprobe\n\ngo 1.27rc2\n\nrequire github.com/bronystylecrazy/ultrastack v0.0.0\n\nreplace github.com/bronystylecrazy/ultrastack => "+root+"\n")
	write("main.go", `package main

import "github.com/bronystylecrazy/ultrastack/di"

type Config struct{}
type DB struct{}

func NewConfig() *Config    { return &Config{} }
func NewDB(cfg *Config) *DB { return &DB{} }

func main() {
	_ = di.Validate(
		di.Provide(NewDB),
	)
}
`)

	var out, errW strings.Builder
	code := runVet(mod, []string{"-fix", "./..."}, &out, &errW)
	if code != 1 {
		t.Fatalf("a run that found (and fixed) a problem still exits 1, got %d\n%s%s", code, out.String(), errW.String())
	}
	fixed, err := os.ReadFile(filepath.Join(mod, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fixed), "di.Provide(NewConfig),") {
		t.Fatalf("ultra vet -fix did not register the missing provider:\n%s\n--- output ---\n%s%s",
			fixed, out.String(), errW.String())
	}
	if !strings.Contains(out.String(), "fixed 1 issue in 1 file") {
		t.Errorf("summary missing from output:\n%s", out.String())
	}

	// Idempotent: the graph is whole now, so a second run is clean.
	out.Reset()
	errW.Reset()
	if code := runVet(mod, []string{"./..."}, &out, &errW); code != 0 {
		t.Fatalf("the fixed module must vet clean, got %d:\n%s%s", code, out.String(), errW.String())
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
