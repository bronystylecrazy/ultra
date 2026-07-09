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
		fmt.Fprintln(errW, "usage: ultra fleet status|vet [dir] [--save] [--json]")
		return 2
	}
	sub := args[0]
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
		return 1
	}

	switch sub {
	case "status":
		return fleetStatus(repos, root, save, jsonOut, out, errW)
	case "vet":
		return fleetVet(repos, out, errW)
	default:
		fmt.Fprintf(errW, "unknown fleet command %q (status|vet)\n", sub)
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

// fingerprint asks the product itself: `go run . graph --json` — every
// product is a toolbox, so the fleet needs no special hooks.
func fingerprint(p *fleetProduct) {
	out, err := goRunProduct(p.Dir, "graph", "--json")
	if err != nil {
		p.Err = err.Error()
		return
	}
	var g struct {
		Fingerprint string `json:"fingerprint"`
		Components  []any  `json:"components"`
	}
	if err := json.Unmarshal([]byte(out), &g); err != nil {
		p.Err = "unparseable graph output"
		return
	}
	p.Fingerprint = g.Fingerprint
	p.Components = len(g.Components)
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
	statePath := filepath.Join(root, ".ultra-fleet.json")
	baseline := map[string]string{}
	if data, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(data, &baseline)
	}

	for i := range repos {
		fingerprint(&repos[i])
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
			return 1
		}
		fmt.Fprintf(out, "baseline saved: %s (%d products)\n", statePath, len(next))
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(repos)
	} else {
		fmt.Fprintf(out, "%-32s %-12s %-14s %-6s %s\n", "PRODUCT", "FRAMEWORK", "FINGERPRINT", "COMPS", "DRIFT")
		for _, p := range repos {
			ver := p.Version
			if p.Replaced {
				ver += " (local)"
			}
			fp := short(p.Fingerprint)
			if p.Err != "" {
				fp = "error: " + p.Err
			}
			fmt.Fprintf(out, "%-32s %-12s %-14s %-6d %s\n", p.Module, ver, fp, p.Components, p.Drift)
		}
	}

	code := 0
	for _, p := range repos {
		if p.Err != "" || strings.HasPrefix(p.Drift, "DRIFT") {
			code = 1 // CI-friendly: drift or breakage fails the step
		}
	}
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
		return 1
	}
	worst := 0
	for _, p := range repos {
		cmd := exec.Command(vet, "./...")
		cmd.Dir = p.Dir
		outBuf := &strings.Builder{}
		cmd.Stdout = outBuf
		cmd.Stderr = outBuf
		err := cmd.Run()
		if err == nil {
			fmt.Fprintf(out, "✓ %s\n", p.Module)
			continue
		}
		worst = 1
		fmt.Fprintf(out, "✗ %s\n", p.Module)
		for line := range strings.SplitSeq(strings.TrimRight(outBuf.String(), "\n"), "\n") {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
	return worst
}
