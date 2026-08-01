package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDerivePortIsStableAndRanged: the whole contract of the derivation —
// same input, same port, forever; always inside the documented range; never
// one of the ports a dev machine already owns.
func TestDerivePortIsStableAndRanged(t *testing.T) {
	names := []string{"speedcheck", "firefly", "toadstool", "a", "zzz-9"}
	for _, n := range names {
		p := derivePort(n, "http", httpPortLo, httpPortHi)
		if p != derivePort(n, "http", httpPortLo, httpPortHi) {
			t.Fatalf("%s: derivation is not stable", n)
		}
		if p < httpPortLo || p > httpPortHi {
			t.Errorf("%s: http port %d outside %d–%d", n, p, httpPortLo, httpPortHi)
		}
		if reservedPorts[p] {
			t.Errorf("%s: derived a reserved port %d", n, p)
		}
		// The salts must not collapse: one product's two ports are two ports.
		if e := derivePort(n, "e2e", e2ePortLo, e2ePortHi); e == p {
			t.Errorf("%s: http and e2e derived the same port %d", n, p)
		}
	}
	// A range that is entirely reserved must terminate rather than spin.
	if got := derivePort("x", "http", 8443, 8443); got != 8443 {
		t.Errorf("a fully reserved range must still answer: %d", got)
	}
}

// TestScaffoldDerivesPortsFromName is the collision fix end to end: two
// products scaffolded on one machine claim different ports, the same name
// twice claims the same ones, and the old shared defaults are gone.
func TestScaffoldDerivesPortsFromName(t *testing.T) {
	render := func(name string) (config, playwright string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), name)
		if err := scaffold(dir, testData(name, scaffoldData{Web: true, DS: dsBare})); err != nil {
			t.Fatal(err)
		}
		read := func(rel string) string {
			b, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		return read("config.toml"), read("web/playwright.config.ts")
	}

	oneCfg, onePw := render("firefly")
	twoCfg, twoPw := render("toadstool")
	if oneCfg == twoCfg || onePw == twoPw {
		t.Fatal("two products derived the same ports — the collision is back")
	}
	for _, c := range []struct{ what, body, dead string }{
		{"config.toml", oneCfg, ":8080"},
		{"playwright.config.ts", onePw, "8391"},
	} {
		if strings.Contains(c.body, c.dead) {
			t.Errorf("%s still carries the shared default %s:\n%s", c.what, c.dead, c.body)
		}
	}
	// Same name, same ports: regenerating a product is a no-op diff.
	againCfg, againPw := render("firefly")
	if againCfg != oneCfg || againPw != onePw {
		t.Error("the same product name must derive the same ports on every scaffold")
	}

	d := testData("firefly", scaffoldData{Web: true, DS: dsBare})
	d.fillPorts()
	if want := `addr = ":` + strconv.Itoa(d.HTTPPort) + `"`; !strings.Contains(oneCfg, want) {
		t.Errorf("config.toml missing %q:\n%s", want, oneCfg)
	}
	if !strings.Contains(onePw, "'"+strconv.Itoa(d.E2EPort)+"'") {
		t.Errorf("playwright.config.ts missing the derived e2e port %d:\n%s", d.E2EPort, onePw)
	}
}
