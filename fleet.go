package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The fleet layer: one team, ~120 products under MA. Every product is a
// toolbox binary with machine-readable answers (graph --json, doctor,
// ultravet) — fleet commands walk many repos and aggregate.
//
//	ultra fleet status [dir]      version + graph fingerprint per product,
//	                              drift vs the saved baseline
//	ultra fleet status --save     record today's fingerprints as baseline
//	ultra fleet vet [dir]         ultravet across every product
//
// State lives in .ultra-fleet.json at the fleet root.

const frameworkModule = "github.com/bronystylecrazy/ultrastack"

type fleetProduct struct {
	Dir         string `json:"dir"`
	Module      string `json:"module"`
	Version     string `json:"version"`               // framework require
	Replaced    bool   `json:"replaced,omitempty"`    // local replace active
	Fingerprint string `json:"fingerprint,omitempty"` // from `graph --json`
	Components  int    `json:"components,omitempty"`
	Drift       string `json:"drift,omitempty"` // vs baseline
	Err         string `json:"error,omitempty"`
}

func cmdFleet(args []string, out, errW io.Writer) int {
	if len(args) == 0 {
		ultraTree().find("fleet").help(errW)
		return 2
	}
	sub := args[0]
	// bump and profiles carry value flags (--to vX.Y.Z) and their own
	// semantics, so they parse and discover for themselves.
	switch sub {
	case "bump":
		return fleetBump(args[1:], out, errW)
	case "profiles":
		return fleetProfiles(args[1:], out, errW)
	}

	root := "."
	save, jsonOut := false, false
	for _, a := range args[1:] {
		switch a {
		case "--save":
			save = true
		case "--json":
			jsonOut = true
		default:
			if !strings.HasPrefix(a, "-") {
				root = a
			}
		}
	}

	repos := discoverFleet(root)
	if len(repos) == 0 {
		fmt.Fprintf(errW, "no ultrastack products under %s (looked for go.mod requiring %s)\n", root, frameworkModule)
		failVerdict(errW, "fleet "+sub, "no products found under "+root)
		return 1
	}

	switch sub {
	case "status":
		return fleetStatus(repos, root, save, jsonOut, out, errW)
	case "vet":
		return fleetVet(repos, out, errW)
	default:
		ultraTree().find("fleet").unknown(errW, sub)
		return 2
	}
}

// discoverFleet finds product repos: go.mod requiring the framework
// (the framework repo itself is not a product).
func discoverFleet(root string) []fleetProduct {
	var out []fleetProduct
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if (strings.HasPrefix(name, ".") && len(name) > 1 && path != root) || name == "node_modules" || name == "testdata" || name == "vendor" ||
				name == "Library" || name == "Applications" || name == "Movies" || name == "Music" || name == "Pictures" || name == "pkg" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		mod := string(data)
		p := fleetProduct{Dir: filepath.Dir(path)}
		for line := range strings.SplitSeq(mod, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "module "); ok {
				p.Module = strings.TrimSpace(v)
			}
			if strings.HasPrefix(line, frameworkModule+" v") || strings.HasPrefix(line, "require "+frameworkModule+" v") {
				fields := strings.Fields(line)
				p.Version = fields[len(fields)-1]
			}
			if strings.HasPrefix(line, "replace "+frameworkModule+" ") ||
				strings.HasPrefix(line, frameworkModule+" =>") {
				p.Replaced = true
			}
		}
		if strings.HasPrefix(p.Module, frameworkModule) || !strings.Contains(mod, frameworkModule) {
			return filepath.SkipDir // the framework and its submodules are not products
		}
		if p.Version == "" && !p.Replaced {
			return filepath.SkipDir
		}
		out = append(out, p)
		return filepath.SkipDir // one product per module
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// fingerprintOf is the seam fleetStatus goes through, so a rendering test can
// supply the numbers instead of booting a product per row.
var fingerprintOf = fingerprint

// fingerprint asks the product itself: `go run . graph --json` — every
// product is a toolbox, so the fleet needs no special hooks.
func fingerprint(p *fleetProduct) {
	g, err := fetchGraph(p.Dir)
	if err != nil {
		// A product answers a failed graph with its OWN diagnostic: coloured,
		// several lines, and destined for a padded table cell. Flatten it here
		// so every consumer — the table and --json alike — gets plain text.
		p.Err = plainLine(err.Error())
		return
	}
	p.Fingerprint = g.Fingerprint
	p.Components = len(g.Components)
}

// productGraph is the parsed shape of a product's `graph --json` output — the
// single fetch that serves both the fingerprint (fleet status) and the
// capability inventory (fleet profiles).
type productGraph struct {
	Fingerprint string `json:"fingerprint"`
	Components  []struct {
		Type   string `json:"type"`
		Module string `json:"module"`
	} `json:"components"`
}

// fetchGraph runs a product's own `graph --json` toolbox command once and
// parses it. Every fleet reader goes through here so a product is asked for
// its structure exactly one way.
func fetchGraph(dir string) (*productGraph, error) {
	out, err := goRunProduct(dir, "graph", "--json")
	if err != nil {
		return nil, err
	}
	var g productGraph
	if err := json.Unmarshal([]byte(out), &g); err != nil {
		return nil, errors.New("unparseable graph output")
	}
	return &g, nil
}

// goRunProduct runs `go run . <args>` inside a product dir with a hard
// timeout, returning stdout. The admin socket is disabled so the toolbox
// answers from a fresh process rather than a listening instance. Shared by
// the fleet fingerprint and the mcp graph/blast tools.
func goRunProduct(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"run", "."}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ULTRA_ADMIN_SOCKET=off")
	outBuf := &strings.Builder{}
	errBuf := &strings.Builder{}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return "", errors.New(firstLineOf(errBuf.String()))
		}
		return outBuf.String(), nil
	case <-time.After(90 * time.Second):
		cmd.Process.Kill()
		return "", errors.New(strings.Join(args, " ") + " timed out")
	}
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	if s == "" {
		return "failed"
	}
	return s
}

func fleetStatus(repos []fleetProduct, root string, save, jsonOut bool, out, errW io.Writer) int {
	statePath := filepath.Join(root, fleetMarker)
	baseline := map[string]string{}
	if data, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(data, &baseline)
	}

	for i := range repos {
		fingerprintOf(&repos[i])
		if base, ok := baseline[repos[i].Module]; ok && repos[i].Fingerprint != "" {
			if base == repos[i].Fingerprint {
				repos[i].Drift = "—"
			} else {
				repos[i].Drift = "DRIFT (was " + short(base) + ")"
			}
		}
	}

	if save {
		next := map[string]string{}
		for _, p := range repos {
			if p.Fingerprint != "" {
				next[p.Module] = p.Fingerprint
			}
		}
		data, _ := json.MarshalIndent(next, "", "  ")
		if err := os.WriteFile(statePath, data, 0o644); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "fleet status", err.Error())
			return 1
		}
		fmt.Fprintf(out, "baseline saved: %s (%d products)\n", statePath, len(next))
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(repos)
	} else {
		// tabwriter, because a module path is data and data has no width: the
		// %-32s this used to be sheared every row of a fleet whose longest
		// module name ran past 32 characters, and DRIFT — the one column a
		// fleet table exists to be scanned for — was the column that moved.
		//
		// DRIFT is also deliberately LAST, which is what makes it the cell
		// allowed to carry colour (see table.go).
		col := colorFor(out)
		t := newTable(out)
		t.row("PRODUCT", "FRAMEWORK", "FINGERPRINT", "COMPS", "DRIFT")
		for _, p := range repos {
			ver := p.Version
			if p.Replaced {
				ver += " (local)"
			}
			fp := short(p.Fingerprint)
			if p.Err != "" {
				// A fingerprint column is 16 characters wide; a diagnostic is
				// not. Clip on runes — a flattened diagnostic still carries
				// multi-byte characters, and half a rune is not a character.
				e := p.Err
				if r := []rune(e); len(r) > 48 {
					e = string(r[:45]) + "..."
				}
				fp = "error: " + e
			}
			drift := p.Drift
			if strings.HasPrefix(drift, "DRIFT") {
				drift = col.red(drift)
			}
			t.row(p.Module, ver, fp, strconv.Itoa(p.Components), drift)
		}
		t.flush()
	}

	code, drifted, broken := 0, 0, 0
	for _, p := range repos {
		if p.Err != "" {
			broken++
		}
		if strings.HasPrefix(p.Drift, "DRIFT") {
			drifted++
		}
		if p.Err != "" || strings.HasPrefix(p.Drift, "DRIFT") {
			code = 1 // CI-friendly: drift or breakage fails the step
		}
	}
	detail := fmt.Sprintf("%s, %d drifted, %d unreadable", count(len(repos), "product"), drifted, broken)
	verdictFor(errW, code == 0, "fleet status", detail)
	return code
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// fleetVet runs the static analyzer across every product.
func fleetVet(repos []fleetProduct, out, errW io.Writer) int {
	vet, err := exec.LookPath("ultravet")
	if err != nil {
		fmt.Fprintln(errW, "ultra fleet vet: install the analyzer first:\n  GOPRIVATE=github.com/bronystylecrazy/* go install "+analyzerModule+"@latest")
		failVerdict(errW, "fleet vet", "the analyzer is not installed")
		return 1
	}
	worst, clean := 0, 0
	for _, p := range repos {
		cmd := exec.Command(vet, "./...")
		cmd.Dir = p.Dir
		outBuf := &strings.Builder{}
		cmd.Stdout = outBuf
		cmd.Stderr = outBuf
		err := cmd.Run()
		if err == nil {
			fmt.Fprintf(out, "✓ %s\n", p.Module)
			clean++
			continue
		}
		worst = 1
		fmt.Fprintf(out, "✗ %s\n", p.Module)
		for line := range strings.SplitSeq(strings.TrimRight(outBuf.String(), "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	verdictFor(errW, worst == 0, "fleet vet",
		fmt.Sprintf("%d of %s clean", clean, count(len(repos), "product")))
	return worst
}
