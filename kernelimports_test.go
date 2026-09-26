package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v0.9.43 moved the kernel to its own module: an upgrade across that line
// rewrites the import path (gofmt re-sorts the block), leaves nested modules
// and testdata alone, and hands back the originals for restore.
func TestRewriteKernelImports(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const old = `package main

import (
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/di"
	"github.com/bronystylecrazy/ultrastack/di/diag"
)

var _ = di.Provide
var _ = diag.Wrap
var _ = pg.Use
`
	write("main.go", old)
	write("internal/app/x.go", "package app\n\nimport \"github.com/bronystylecrazy/ultrastack/di\"\n\nvar _ = di.Provide\n")
	write("tools/go.mod", "module tools\n")
	write("tools/t.go", old)
	write("testdata/t.go", old)
	write("plain.go", "package main\n")

	orig, err := rewriteKernelImports(dir, "v0.9.43")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	want := `import (
	"github.com/bronystylecrazy/di"
	"github.com/bronystylecrazy/di/diag"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
)`
	if !strings.Contains(string(got), want) {
		t.Fatalf("main.go not rewritten and sorted:\n%s", got)
	}
	if len(orig) != 2 || string(orig[filepath.Join(dir, "main.go")]) != old {
		t.Fatalf("originals = %d files, want main.go and internal/app/x.go", len(orig))
	}
	for _, untouched := range []string{"tools/t.go", "testdata/t.go"} {
		if b, _ := os.ReadFile(filepath.Join(dir, untouched)); string(b) != old {
			t.Errorf("%s must be left alone", untouched)
		}
	}

	// Below the split, nothing moves.
	write("main.go", old)
	if orig, err := rewriteKernelImports(dir, "v0.9.42"); err != nil || len(orig) != 0 {
		t.Fatalf("an upgrade to v0.9.42 rewrote %d files (%v)", len(orig), err)
	}
}
