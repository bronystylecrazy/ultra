package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A profiles fixture product is a trivial runnable module: its go.mod requires
// the framework (so discovery finds it) via a local replace to a stub (so it
// builds offline), and its main echoes a canned `graph --json` payload — the
// synthetic graph the profiles command reduces to a capability set.
func writeProfileProduct(t *testing.T, root, name, module, graphJSON string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module " + module + "\n\ngo 1.26\n\nrequire " + frameworkModule +
		" v0.1.0\n\nreplace " + frameworkModule + " => ../fwstub\n"
	main := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n)\n\n" +
		"func main() {\n\tif strings.Join(os.Args[1:], \" \") == \"graph --json\" {\n" +
		"\t\tfmt.Println(`" + graphJSON + "`)\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFwStub drops a local stub module the products' replace directives point
// at, so `go run .` resolves the framework offline.
func writeFwStub(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "fwstub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+frameworkModule+"\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("package ultrastack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func graphJSON(caps ...string) string {
	var comps []string
	for _, c := range caps {
		comps = append(comps, `{"type":"*x.T","module":"`+c+`"}`)
	}
	// A little noise: a non-preset module that must NOT count as a capability.
	comps = append(comps, `{"type":"*app.Root","module":"-"}`)
	return `{"fingerprint":"fp","components":[` + strings.Join(comps, ",") + `]}`
}

// TestFleetProfiles: four products, two sharing a capability set and two
// unique → one two-member profile and two singletons; a fifth product with
// malformed graph output is skipped with a note.
func TestFleetProfiles(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run against real product fixtures")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixtures assume POSIX shell/paths")
	}
	root := t.TempDir()
	writeFwStub(t, root)
	// p1 and p2 share {contrib/otel, contrib/pg}.
	writeProfileProduct(t, root, "p1", "github.com/acme/p1", graphJSON("contrib/pg", "contrib/otel"))
	writeProfileProduct(t, root, "p2", "github.com/acme/p2", graphJSON("contrib/otel", "contrib/pg"))
	// p3 and p4 are unique.
	writeProfileProduct(t, root, "p3", "github.com/acme/p3", graphJSON("contrib/redis"))
	writeProfileProduct(t, root, "p4", "github.com/acme/p4", graphJSON("stack/http", "contrib/pg"))
	// p5 emits garbage → skipped.
	writeProfileProduct(t, root, "p5", "github.com/acme/p5", "not json at all")

	var out, errW strings.Builder
	code := fleetProfiles([]string{root, "--json"}, &out, &errW)
	if code != 0 {
		t.Fatalf("profiles exit=%d err=%s", code, errW.String())
	}

	var payload struct {
		Profiles []struct {
			Fingerprint  string   `json:"fingerprint"`
			Capabilities []string `json:"capabilities"`
			Members      []string `json:"members"`
		} `json:"profiles"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("profiles --json not parseable: %v\n%s", err, out.String())
	}

	if len(payload.Profiles) != 3 {
		t.Fatalf("want 3 profiles (1 shared + 2 unique), got %d: %+v", len(payload.Profiles), payload.Profiles)
	}
	// Sorted by member count desc: the shared profile is first.
	top := payload.Profiles[0]
	if len(top.Members) != 2 {
		t.Fatalf("top profile must have 2 members, got %d: %+v", len(top.Members), top)
	}
	members := strings.Join(top.Members, ",")
	if !strings.Contains(members, "github.com/acme/p1") || !strings.Contains(members, "github.com/acme/p2") {
		t.Fatalf("shared profile members wrong: %v", top.Members)
	}
	// Capability set is the sorted preset modules; "-" is not a capability.
	if strings.Join(top.Capabilities, ",") != "contrib/otel,contrib/pg" {
		t.Fatalf("capabilities wrong: %v", top.Capabilities)
	}
	for _, p := range payload.Profiles[1:] {
		if len(p.Members) != 1 {
			t.Fatalf("expected singletons after the shared profile, got %+v", p)
		}
	}
	if len(payload.Skipped) != 1 || !strings.Contains(payload.Skipped[0], "github.com/acme/p5") {
		t.Fatalf("malformed product must be skipped with a note: %v", payload.Skipped)
	}
}

// TestFleetProfilesText covers the human table path (descriptive framing).
func TestFleetProfilesText(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go run against real product fixtures")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fixtures assume POSIX shell/paths")
	}
	root := t.TempDir()
	writeFwStub(t, root)
	writeProfileProduct(t, root, "a", "github.com/acme/a", graphJSON("contrib/pg"))
	writeProfileProduct(t, root, "b", "github.com/acme/b", graphJSON("contrib/pg"))

	var out, errW strings.Builder
	if code := fleetProfiles([]string{root}, &out, &errW); code != 0 {
		t.Fatalf("exit=%d err=%s", code, errW.String())
	}
	s := out.String()
	if !strings.Contains(s, "descriptive") {
		t.Errorf("text output must state it is descriptive: %s", s)
	}
	if !strings.Contains(s, "contrib/pg") || !strings.Contains(s, "2 product(s)") {
		t.Errorf("text output missing the shared profile: %s", s)
	}
}
