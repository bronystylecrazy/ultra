package main

import (
	"os"
	"path/filepath"
	"sort"
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
