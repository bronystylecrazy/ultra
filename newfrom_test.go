package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixtureOps is the source document's translatable surface: every path+method
// in testdata/legacy.json that MUST appear in the product's own openapi.json,
// with the operationId it must carry. This table is the round-trip covenant —
// the whole promise of `--from` is that a document goes in and the same
// contract comes back out of the generated product.
var fixtureOps = map[string]string{
	"GET /depots":                 "listDepots",
	"POST /depots":                "createDepot",
	"GET /depots/{depotId}":       "getDepot",
	"PATCH /depots/{depotId}":     "updateDepot", // operationId "patch depot" is not an identifier
	"DELETE /depots/{depotId}":    "deleteDepot",
	"GET /shipments":              "listShipments", // no operationId in the source
	"POST /shipments":             "bookShipment",
	"POST /shipments/{id}/cancel": "cancelShipment",
}

// fixtureSkipped are the paths the translator must NOT scaffold — and must say
// so out loud.
var fixtureSkipped = []string{"/depots/{depotId}/photo", "/shipments/report.csv"}

func TestFromRefusesYAML(t *testing.T) {
	_, err := planFrom("api.yaml")
	if err == nil || !strings.Contains(err.Error(), "JSON only") {
		t.Fatalf("a YAML document must be refused with the conversion advice: %v", err)
	}
	if !strings.Contains(err.Error(), "yq") {
		t.Errorf("the refusal must carry a conversion one-liner:\n%v", err)
	}
}

func TestFromRefusesNonOpenAPI(t *testing.T) {
	dir := t.TempDir()
	swagger := filepath.Join(dir, "swagger.json")
	os.WriteFile(swagger, []byte(`{"swagger":"2.0","info":{}}`), 0o644)
	if _, err := planFrom(swagger); err == nil || !strings.Contains(err.Error(), "Swagger") {
		t.Fatalf("Swagger 2.0 must be refused by name: %v", err)
	}
	junk := filepath.Join(dir, "junk.json")
	os.WriteFile(junk, []byte(`{"hello":"world"}`), 0o644)
	if _, err := planFrom(junk); err == nil {
		t.Fatal("a document with no `openapi` field must be refused")
	}
}

// TestFromTranslation is the translation table, pinned: features from tags,
// names through contrib/api's vocabulary, schemas into Go, security guessed
// from the method — and everything unsupported COLLECTED rather than guessed.
func TestFromTranslation(t *testing.T) {
	plan, err := planFrom("testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.PkgNames(); strings.Join(got, ",") != "depots,shipments" {
		t.Fatalf("features come from tags, sanitized: %v", got)
	}

	byPkg := map[string]*genFeature{}
	for _, f := range plan.Features {
		byPkg[f.Pkg] = f
	}

	// Names: the operationId when it is a legal identifier (so the contract
	// round-trips), the derived name otherwise — and api.Name pins the
	// difference only when there IS one.
	want := map[string]struct {
		dotted, sym string
		pin         bool
	}{
		"GET /depots":                 {"depots.list", "listDepots", false},
		"POST /depots":                {"depots.create", "createDepot", false},
		"GET /depots/{depotId}":       {"depots.get", "getDepot", false},
		"PATCH /depots/{depotId}":     {"depots.update", "updateDepot", false},
		"DELETE /depots/{depotId}":    {"depots.delete", "deleteDepot", false},
		"GET /shipments":              {"shipments.list", "listShipments", false},
		"POST /shipments":             {"shipments.create", "bookShipment", true},
		"POST /shipments/{id}/cancel": {"shipments.cancel", "cancelShipment", false},
	}
	seen := map[string]bool{}
	for _, f := range plan.Features {
		for _, op := range f.Ops {
			key := op.Method + " " + op.Path
			seen[key] = true
			w, ok := want[key]
			if !ok {
				t.Errorf("unexpected operation %s", key)
				continue
			}
			if op.Dotted != w.dotted || op.Sym != w.sym || op.PinName != w.pin {
				t.Errorf("%s: got (%s, %s, pin=%v), want (%s, %s, pin=%v)",
					key, op.Dotted, op.Sym, op.PinName, w.dotted, w.sym, w.pin)
			}
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("operation %s was not scaffolded", key)
		}
	}

	// Security: the document's global requirement guards everything except the
	// one operation that opted out with `security: []`, and the permission is
	// guessed read/write from the method.
	perms := map[string]string{}
	for _, f := range plan.Features {
		for _, op := range f.Ops {
			perms[op.Method+" "+op.Path] = op.Require
		}
	}
	if perms["GET /depots"] != "" {
		t.Errorf(`GET /depots declares "security": [] — it must be api.Public(): %q`, perms["GET /depots"])
	}
	if perms["GET /depots/{depotId}"] != "depots.read" {
		t.Errorf("a guarded GET guesses <feature>.read: %q", perms["GET /depots/{depotId}"])
	}
	if perms["POST /depots"] != "depots.write" {
		t.Errorf("a guarded write guesses <feature>.write: %q", perms["POST /depots"])
	}

	// Schemas → Go. The whole translation table in one struct.
	types := byPkg["depots"].TypeDefs
	for _, want := range []string{
		"type DepotsDepot struct {",                 // shared component, package-qualified
		"ID string `json:\"id\"`",                   // string, and the one initialism
		"Active bool `json:\"active\"`",             // boolean
		"Coordinates struct {",                      // a nested inline object
		"Lat float64 `json:\"lat\"`",                // number
		"Tags []string `json:\"tags\"`",             // array
		"Metadata map[string]string",                // additionalProperties
		"RetiredAt *time.Time `json:\"retiredAt\"`", // nullable date-time → *time.Time
		"Notes *string `json:\"notes\"`",            // 3.0 `nullable: true` → pointer
	} {
		if !strings.Contains(types, want) {
			t.Errorf("depots/types.go is missing %q:\n%s", want, types)
		}
	}
	ships := byPkg["shipments"].TypeDefs
	for _, want := range []string{
		"type Shipment struct {", // allOf of objects → one merged struct
		"WeightKg float32",       // number/float
		"// enum: draft | booked | delivered",
		"Depot ShipmentsDepot",    // the shared component, qualified per feature
		"Payload json.RawMessage", // oneOf → parked
		"oneOf composition",       // ...under a TODO that names the composition
	} {
		if !strings.Contains(ships, want) {
			t.Errorf("shipments/types.go is missing %q:\n%s", want, ships)
		}
	}

	// Requests: path template vars always bind, query params bind, a $ref body
	// embeds, an inline body flattens, and nothing to bind is api.None.
	h := byPkg["depots"].Handlers
	for _, want := range []string{
		"Region string `query:\"region\"`",
		"Limit int32 `query:\"limit\"`",
		"DepotID string `path:\"depotId\"`",
		"DepotInput\n}", // the $ref body, embedded
	} {
		if !strings.Contains(h, want) {
			t.Errorf("depots/handler.go is missing %q:\n%s", want, h)
		}
	}
	if !strings.Contains(byPkg["shipments"].Handlers, "func listShipments(ctx context.Context, _ api.None)") {
		t.Errorf("an operation with nothing to bind takes api.None:\n%s", byPkg["shipments"].Handlers)
	}
	// Every handler is a 501 stub, and the code it fails with is declared.
	for _, f := range plan.Features {
		for _, op := range f.Ops {
			if !strings.Contains(f.Handlers, "api.Fail(http.StatusNotImplemented, "+op.StubCode) {
				t.Errorf("%s must stub with %s", op.Dotted, op.StubCode)
			}
			if !strings.Contains(f.Wiring, "api.Codes("+op.StubCode) {
				t.Errorf("%s must declare %s with api.Codes", op.Dotted, op.StubCode)
			}
		}
	}
	// errors.go carries the codes the document's error responses imply.
	if !strings.Contains(byPkg["depots"].CodeDefs, `CodeNotFound = "depots.not_found"`) {
		t.Errorf("a declared 404 scaffolds <feature>.not_found:\n%s", byPkg["depots"].CodeDefs)
	}
	if !strings.Contains(byPkg["depots"].CodeDefs, `CodeConflict = "depots.conflict"`) {
		t.Errorf("a declared 409 scaffolds <feature>.conflict:\n%s", byPkg["depots"].CodeDefs)
	}

	// The report: everything not translated is NAMED, with its path.
	rep := plan.report.String()
	for _, want := range []string{
		"depotRetired",              // the 3.1 webhook
		"onDelivered",               // the callback
		"X-Legacy-Trace",            // the header parameter a bound request cannot read
		"/depots/{depotId}/photo",   // image/png request body
		"/shipments/report.csv",     // text/csv response
		"Shipment.payload",          // the parked oneOf
		"Depot → depots, shipments", // the shared component
		"8 — every one a 501 stub",
		"7 guarded (api.Require, GUESSED), 1 public",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("the migration report must name %q:\n%s", want, rep)
		}
	}
	for _, gone := range fixtureSkipped {
		for _, f := range plan.Features {
			if strings.Contains(f.Wiring, gone) {
				t.Errorf("%s must not be scaffolded — it was reported as unsupported", gone)
			}
		}
	}
}

// TestFromDeterminism: two runs over one document write byte-identical Go.
// Map iteration order is the classic way a generator starts churning diffs.
func TestFromDeterminism(t *testing.T) {
	render := func() string {
		plan, err := planFrom("testdata/legacy.json")
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, f := range plan.Features {
			b.WriteString(f.Pkg + "\n" + f.Wiring + f.Handlers + f.TypeDefs + f.CodeDefs)
		}
		b.WriteString(plan.report.String())
		return b.String()
	}
	if a, b := render(), render(); a != b {
		t.Error("two runs over one document must write identical Go")
	}
}

// TestFromResolvesNameCollisions: two operations that want the same symbol get
// one disambiguated name, an api.Name pin, and a line in the report — never a
// silent merge and never a generation error downstream.
func TestFromResolvesNameCollisions(t *testing.T) {
	doc := `{"openapi":"3.0.3","info":{"title":"t","version":"1"},"paths":{
	  "/things":{"get":{"tags":["things"],"responses":{"200":{"description":"ok"}}}},
	  "/things/archive":{"get":{"operationId":"listThings","tags":["things"],
	    "responses":{"200":{"description":"ok"}}}}}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "openapi.json")
	os.WriteFile(path, []byte(doc), 0o644)

	plan, err := planFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	syms := map[string]bool{}
	for _, f := range plan.Features {
		for _, op := range f.Ops {
			if syms[op.Sym] {
				t.Fatalf("duplicate symbol %s — checkNames would refuse the contract", op.Sym)
			}
			syms[op.Sym] = true
		}
	}
	if !syms["listThings"] || !syms["listThings2"] {
		t.Fatalf("the collision must resolve deterministically: %v", syms)
	}
	if rep := plan.report.String(); !strings.Contains(rep, "api.Name(\"listThings2\")") {
		t.Errorf("the report must tell the api.Name story:\n%s", rep)
	}
	if !strings.Contains(plan.Features[0].Wiring, `api.Name("listThings2")`) {
		t.Errorf("the disambiguated name must be PINNED in the wiring:\n%s", plan.Features[0].Wiring)
	}
}

// TestFromScaffoldShape: --from ADDS to the normal tree — it never replaces it.
// A product born from a document is the same product as any other, plus the
// feature packages and the one line each in app.go.
func TestFromScaffoldShape(t *testing.T) {
	plan, err := planFrom("testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	d := testData("depotreg", scaffoldData{})
	d.plan, d.Features = plan, plan.PkgNames()
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	got := treeOf(t, dir)
	for _, want := range []string{
		".gitignore", "AGENTS.md", "Taskfile.yml", "config.toml", "contract_test.go",
		"go.mod", "internal/app/app.go", "main.go", "main_test.go",
		"internal/app/depots/depots.go", "internal/app/depots/errors.go",
		"internal/app/depots/handler.go", "internal/app/depots/types.go",
		"internal/app/shipments/shipments.go", "internal/app/shipments/errors.go",
		"internal/app/shipments/handler.go", "internal/app/shipments/types.go",
	} {
		if !slicesContain(got, want) {
			t.Errorf("%s is missing from the tree:\n%s", want, strings.Join(got, "\n"))
		}
	}
	appGo, err := os.ReadFile(filepath.Join(dir, "internal", "app", "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"example.com/depotreg/internal/app/depots"`,
		"depots.Module,",
		"shipments.Module,",
	} {
		if !strings.Contains(string(appGo), want) {
			t.Errorf("app.go must carry one line per feature, missing %q:\n%s", want, appGo)
		}
	}
	// A forward scaffold is untouched by any of this.
	plainDir := filepath.Join(t.TempDir(), "plain")
	if err := scaffold(plainDir, testData("plain", scaffoldData{})); err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(filepath.Join(plainDir, "internal", "app", "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), `import "github.com/bronystylecrazy/ultrastack/di"`) ||
		!strings.Contains(string(plain), "var Modules = di.Group()") {
		t.Errorf("a scaffold without --from must be byte-for-byte what it always was:\n%s", plain)
	}
}

// TestFromRoundTripCovenant is the reverse scaffold dogfooding itself: build a
// product from the fixture document against THIS checkout, make it pass its own
// covenant (wiring, boot, contract drift), and then ask its `openapi` command
// whether the contract came back out — every translatable path, method, and
// operationId of the source document, and none of the ones the report said it
// was skipping.
func TestFromRoundTripCovenant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a full product; skipped in -short")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planFrom("testdata/legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	// --bare: the round trip is about the CONTRACT, and a database to dial or a
	// frontend to build proves nothing about it.
	d := testData("depotreg", scaffoldData{})
	d.plan, d.Features = plan, plan.PkgNames()
	dir := filepath.Join(t.TempDir(), d.Name)
	if err := scaffold(dir, d); err != nil {
		t.Fatal(err)
	}
	t.Logf("ultra new depotreg --bare --from legacy.json\n%s", plan.report.String())

	sh := func(name string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			if offline(out) {
				t.Skipf("%s %s needs the network: %s", name, strings.Join(args, " "), out)
			}
			t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return out
	}
	sh("go", "mod", "edit",
		"-replace="+modulePath+"="+repoRoot,
		"-replace="+modulePath+"/contrib="+filepath.Join(repoRoot, "contrib"))
	sh("go", "mod", "tidy")
	sh("go", "build", "./...")
	sh("go", "test", "./...") // the covenant + the contract-drift bootstrap

	// The round trip itself: the product's OWN openapi command. STDOUT only:
	// the document is a byte contract, and stderr carries the verdict line
	// every command now ends with (`task contracts` redirects stdout for
	// exactly this reason).
	openapi := func() []byte {
		t.Helper()
		cmd := exec.Command("go", "run", ".", "openapi")
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("go run . openapi failed: %v\n%s", err, stderr.String())
		}
		return stdout.Bytes()
	}
	out := openapi()
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Tags        []string `json:"tags"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("`go run . openapi` did not print a document: %v\n%s", err, out)
	}
	for key, wantID := range fixtureOps {
		method, path, _ := strings.Cut(key, " ")
		item, ok := doc.Paths[path]
		if !ok {
			t.Errorf("%s: the path did not round-trip into the emitted contract", key)
			continue
		}
		op, ok := item[strings.ToLower(method)]
		if !ok {
			t.Errorf("%s: the method did not round-trip", key)
			continue
		}
		if op.OperationID != wantID {
			t.Errorf("%s: operationId %q, want %q", key, op.OperationID, wantID)
		}
	}
	for _, gone := range fixtureSkipped {
		if _, ok := doc.Paths[gone]; ok {
			t.Errorf("%s was reported as NOT translated but appears in the contract", gone)
		}
	}

	// ...and the 501 stubs are the contract too: an unported endpoint answers
	// with a declared code, not a 404.
	if !strings.Contains(string(out), "depots.get.not_implemented") {
		t.Errorf("the stub codes must ride the contract (x-error-codes):\n%s", out)
	}

	// A second `go test` must be clean: the artifacts the first run bootstrapped
	// do not drift on their own.
	sh("go", "test", "./...")
}

func slicesContain(list []string, s string) bool {
	i := sort.SearchStrings(list, s)
	return i < len(list) && list[i] == s
}
