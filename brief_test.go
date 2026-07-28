package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// briefFixture is a scaffold-shaped product: the canonical ultra.New root, four
// wired presets, a committed contract and a config.toml — the exact shape an
// agent lands in, so what brief renders here is what it renders in the field.
func briefFixture(t *testing.T) string {
	t.Helper()
	return writeProduct(t, `package main

import (
	"github.com/bronystylecrazy/ultrastack/contrib/api"
	"github.com/bronystylecrazy/ultrastack/contrib/i18n"
	"github.com/bronystylecrazy/ultrastack/contrib/jobs"
	"github.com/bronystylecrazy/ultrastack/contrib/pg"
	"github.com/bronystylecrazy/ultrastack/contrib/ultra"
	"github.com/bronystylecrazy/ultrastack/di"

	"example.com/shop/internal/app"
)

var App = di.Options(
	ultra.New(
		api.Use(api.Info{Title: "shop", Version: "0.1.0"}),
		i18n.Use(),
		pg.Use(),
		jobs.Use(),
		app.Modules(),
	),
)

func main() {}
`, map[string]string{
		"config.toml": `[http]
addr = ":8080"

[log]
level = "info"

[i18n]
locales = ["en"]

[postgres]
url = "postgres://localhost/shop"
`,
		"openapi.json": `{
  "openapi": "3.1.0",
  "info": {"title": "shop", "version": "0.1.0"},
  "paths": {
    "/v1/orders": {
      "get": {"operationId": "listOrders", "responses": {"200": {"description": "ok"}}},
      "post": {"operationId": "createOrder", "responses": {"201": {"description": "made"}}}
    },
    "/v1/orders/{id}": {
      "get": {"operationId": "getOrder", "responses": {"200": {"description": "ok"}}}
    }
  }
}
`,
	})
}

// The human render is the contract with the agent that reads it: every section
// present, every fact joined. A missing section here is five commands the
// agent has to run again.
func TestBriefRendersEverySection(t *testing.T) {
	dir := briefFixture(t)
	code, out, errOut := runCLI(t, "brief", dir)
	if code != 0 {
		t.Fatalf("brief: code=%d err=%s", code, errOut)
	}
	for _, want := range []string{
		"shop — example.com/shop",
		"framework  github.com/bronystylecrazy/ultrastack v0.9.19",
		"root       ultra.New at main.go:",
		"WIRED (4)",
		"api.Use(api.Info{Title: \"shop\", Version: \"0.1.0\"})",
		"OPERATIONS (3)",
		"GET", "/v1/orders", "listOrders",
		"POST", "createOrder",
		"/v1/orders/{id}", "getOrder",
		"CONFIG (4)",
		"[postgres]", "[i18n]",
		"ARTIFACTS",
		"openapi.json", "present",
		"drift",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("brief is missing %q:\n%s", want, out)
		}
	}
	// The join both ways: a section the root bundle binds is attributed to the
	// bundle, and a wired preset whose section nobody wrote is called out —
	// that MISSING line is a boot failure an agent should see before it starts.
	if !strings.Contains(out, "ultra.New") || !strings.Contains(out, "MISSING [jobs]") {
		t.Errorf("config join drifted:\n%s", out)
	}
	// No network, no build: the two costly answers stay opt-in and SAY so.
	if !strings.Contains(out, "--drift") {
		t.Errorf("drift must state it was not checked:\n%s", out)
	}
	if !strings.Contains(errOut, "✓ brief — shop") || !strings.Contains(errOut, "4 presets") {
		t.Errorf("verdict = %q", errOut)
	}
}

func TestBriefJSONParses(t *testing.T) {
	dir := briefFixture(t)
	code, out, _ := runCLI(t, "brief", dir, "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var p briefPack
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, out)
	}
	if p.Product != "shop" || p.Module != "example.com/shop" {
		t.Errorf("identity = %q / %q", p.Product, p.Module)
	}
	if len(p.Wired) != 4 || len(p.Operations) != 3 || len(p.Config) != 4 {
		t.Errorf("wired=%d ops=%d config=%d", len(p.Wired), len(p.Operations), len(p.Config))
	}
	if p.APITitle != "shop" {
		t.Errorf("apiTitle = %q", p.APITitle)
	}
	if len(p.Framework) == 0 || p.Framework[0].Version != "v0.9.19" {
		t.Errorf("framework = %+v", p.Framework)
	}
	if !strings.HasPrefix(p.Root, "ultra.New at main.go:") {
		t.Errorf("root = %q", p.Root)
	}
	// The operation table is uncapped in --json even though the human render
	// caps it: a machine asked for all of it.
	if p.Operations[0].Method != "GET" || p.Operations[0].ID != "listOrders" {
		t.Errorf("first op = %+v", p.Operations[0])
	}
	if len(p.ConfigMissing) != 1 || !strings.Contains(p.ConfigMissing[0], "jobs") {
		t.Errorf("configMissing = %v", p.ConfigMissing)
	}
	// stdout is a byte contract: the verdict must not be in it.
	if strings.Contains(out, "✓") {
		t.Errorf("--json stdout carries the verdict:\n%s", out)
	}
}

// A product with no contract and no config is the FIRST hour of a product, and
// brief must answer for it rather than error — honestly, with counts of zero.
func TestBriefBareProduct(t *testing.T) {
	dir := writeProduct(t, "package main\n\nfunc main() {}\n", nil)
	code, out, errOut := runCLI(t, "brief", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	for _, want := range []string{
		"WIRED (0)",
		"imports no contrib preset",
		"OPERATIONS (0)",
		"root       not canonical",
		"no config.toml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bare product missing %q:\n%s", want, out)
		}
	}
}

// briefStubFixture is a port half-done: four operations, two of them still the
// 501 stubs `ultra new --from` scaffolded. The x-error-codes shape is the one
// contrib/api really emits — the declared codes and the stub code share the
// operation-level list, so brief must key on the `.not_implemented` suffix and
// not merely on "this op declares codes".
func briefStubFixture(t *testing.T) string {
	t.Helper()
	return writeProduct(t, "package main\n\nfunc main() {}\n", map[string]string{
		"openapi.json": `{
  "openapi": "3.1.0",
  "info": {"title": "depotreg", "version": "0.1.0"},
  "paths": {
    "/depots": {
      "get": {"operationId": "listDepots",
        "x-error-codes": ["depots.list.not_implemented"],
        "responses": {"200": {"description": "ok"}}},
      "post": {"operationId": "createDepot",
        "x-error-codes": ["depots.conflict"],
        "responses": {"200": {"description": "ok"}}}
    },
    "/depots/{depotId}": {
      "get": {"operationId": "getDepot",
        "x-error-codes": ["depots.get.not_implemented", "depots.not_found"],
        "responses": {"200": {"description": "ok"}}},
      "delete": {"operationId": "deleteDepot",
        "x-error-codes": ["depots.not_found"],
        "responses": {"204": {"description": "gone"}}}
    }
  }
}
`,
	})
}

// The stub list IS the plan for a long port: brief counts what is left so a
// successor session resumes from the contract instead of a todo file.
func TestBriefCountsStubs(t *testing.T) {
	dir := briefStubFixture(t)
	code, out, errOut := runCLI(t, "brief", dir)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errOut)
	}
	for _, want := range []string{
		"OPERATIONS (4)",
		"STUBS (2 of 4 remaining)",
		"depots.list.not_implemented",
		"depots.get.not_implemented",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("brief is missing %q:\n%s", want, out)
		}
	}
	// A ported operation is GONE from the section — that disappearance is the
	// only progress signal the workflow has.
	if strings.Contains(out, "createDepot") && strings.Count(out, "createDepot") > 1 {
		t.Errorf("a ported op must appear in OPERATIONS only:\n%s", out)
	}
	if !strings.Contains(errOut, "2 stubs") {
		t.Errorf("the verdict must carry the remaining count: %q", errOut)
	}
}

// Zero stubs is silence, not "STUBS (0)": a finished product must not carry a
// section reminding it of nothing.
func TestBriefOmitsStubsWhenNone(t *testing.T) {
	dir := briefFixture(t)
	_, out, errOut := runCLI(t, "brief", dir)
	if strings.Contains(out, "STUBS") {
		t.Errorf("a stub-free product renders no STUBS section:\n%s", out)
	}
	if strings.Contains(errOut, "stub") {
		t.Errorf("the verdict must not mention stubs: %q", errOut)
	}
}

func TestBriefStubsJSON(t *testing.T) {
	dir := briefStubFixture(t)
	code, out, _ := runCLI(t, "brief", dir, "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var p briefPack
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, out)
	}
	if p.Stubs != 2 {
		t.Errorf("stubs = %d, want 2", p.Stubs)
	}
	got := map[string]string{}
	for _, op := range p.Operations {
		got[op.ID] = op.Stub
	}
	want := map[string]string{
		"listDepots":  "depots.list.not_implemented",
		"getDepot":    "depots.get.not_implemented",
		"createDepot": "",
		"deleteDepot": "",
	}
	for id, code := range want {
		if got[id] != code {
			t.Errorf("%s stub = %q, want %q", id, got[id], code)
		}
	}
}

func TestBriefNotAModule(t *testing.T) {
	code, _, errOut := runCLI(t, "brief", t.TempDir())
	if code != 2 || !strings.Contains(errOut, "no go.mod") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !strings.Contains(errOut, "ultra fleet status") {
		t.Errorf("must point a whole-workspace ask at the fleet layer: %q", errOut)
	}
}

func TestBriefUnknownFlag(t *testing.T) {
	code, _, errOut := runCLI(t, "brief", "--nope")
	if code != 2 || !strings.Contains(errOut, `unknown flag "--nope"`) {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}
