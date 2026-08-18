package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestReferencesUpToDate is the drift gate: regenerate every managed reference
// file into memory and compare it against what is on disk. A mismatch means
// the docs fell behind the code (a changed signature, a new/edited lesson, an
// updated example) and the fix is always the same one command — no CI config,
// just a failing test.
func TestReferencesUpToDate(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := skillGen(root)
	if err != nil {
		t.Fatalf("skill gen failed: %v", err)
	}
	var stale []string
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(got) != want {
			stale = append(stale, rel)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("generated references are out of date: %v\n"+
			"run `ultra skill gen` (or `go run ./cmd/ultra skill gen`) from the repo root to regenerate.",
			stale)
	}
}

// handWrittenGoBlocks pins how many ```go blocks in each doctrine reference are
// NOT generated from compiled code. The policy: a new Go snippet in these files
// is a compiled example — an Example body (`ultra:gen example`) or a snip of a
// real declaration (`ultra:gen snip`) — because hand-written wiring rots
// silently while compiled wiring fails the build. The remaining ones are
// deliberate FRAGMENTS that no compiler can hold:
//
//	kernel.md  the parameter list without a function around it, the
//	           Start/Stop/Healthy convention sketch, and the PerKey / Family
//	           sketches whose real signatures are already `go doc` blocks below
//	           them.
//
//	product.md  the v3 assembly and the v3 feature shape. Their compiled
//	            twins live in the web module's own tests (web/e2e_test.go),
//	            which this repo's snip machinery cannot reach — web/ is a
//	            NESTED Go module the root toolchain does not compile — so the
//	            two blocks are hand-written and the scaffold covenant compiles
//	            the real thing on every run instead.
//
// If you add a deliberate fragment, bump the count here and say why — a
// regression should be a conscious act, not drift.
var handWrittenGoBlocks = map[string]int{
	"kernel.md":  4,
	"product.md": 2,
}

func TestReferenceSnippetsAreCompiled(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range handWrittenGoBlocks {
		src, err := os.ReadFile(filepath.Join(root, "references", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := countHandWrittenGo(string(src)); got != want {
			t.Errorf("references/%s: %d hand-written ```go blocks, pinned at %d\n"+
				"convert the new snippet to a compiled example (see cmd/ultra/skillgen.go), "+
				"or bump the pin in handWrittenGoBlocks with a reason.", name, got, want)
		}
	}
}

// countHandWrittenGo counts ```go fences that live outside `ultra:gen` blocks.
func countHandWrittenGo(content string) int {
	n, generated := 0, false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case genOpenRe.MatchString(trimmed):
			generated = true
		case trimmed == genEndMarker:
			generated = false
		case trimmed == "```go" && !generated:
			n++
		}
	}
	return n
}
