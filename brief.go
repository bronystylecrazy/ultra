package main

// `ultra brief [dir]` is the ORIENTATION PACK — the one command an agent runs
// before it touches a product it has never seen.
//
// It exists for token economy. The five commands an agent runs today to learn
// the same things — `ultra contrib list`, `ultra upgrade --check`, `cat
// openapi.json`, `cat config.toml`, `git log -- openapi.json` — cost thousands
// of tokens and four round trips, and three of them answer in prose written
// for a human reading one screen. brief answers all five in one compact,
// grep-friendly page: name and module, the framework pin, the canonical root
// shape, the wired presets one line each, the operation table, the config
// sections and who binds them, the state of the committed artifacts, and the
// last commits that moved the contract.
//
// Every section reuses a core that already exists (scanWired, loadRoot,
// modPins, readSpec, goRunProduct, git) — brief composes, it never re-derives.
//
// The two answers that COST something are opt-in, and default to honest
// silence rather than a guess: --check resolves the latest release (network),
// --drift regenerates the contract to compare it (a product build). Everything
// else is a static read of files already on disk.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// briefOpLimit caps the operation table in the HUMAN render. A 400-operation
// product is exactly the one an agent must not paste whole into a context
// window; --json is uncapped, because a machine asked for all of it.
const briefOpLimit = 40

type briefPack struct {
	Product   string `json:"product"`
	Module    string `json:"module"`
	Dir       string `json:"dir"`
	Root      string `json:"root,omitempty"`      // "ultra.New at main.go:62"
	RootError string `json:"rootError,omitempty"` // ...or why it is not canonical

	Framework []briefPin `json:"framework"`
	Latest    string     `json:"latest,omitempty"` // --check only
	Behind    bool       `json:"behind,omitempty"`
	Replaces  []string   `json:"replaces,omitempty"`

	Wired []briefWired `json:"wired"`

	Contract   bool      `json:"contract"` // openapi.json is on disk and readable
	APITitle   string    `json:"apiTitle,omitempty"`
	APIVersion string    `json:"apiVersion,omitempty"`
	Operations []briefOp `json:"operations"`

	Config        []briefSection `json:"config"`
	ConfigMissing []string       `json:"configMissing,omitempty"`

	Artifacts []briefArtifact `json:"artifacts"`
	Drift     string          `json:"drift"`

	Contracts []string `json:"contractLog,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

type briefPin struct {
	Module  string `json:"module"`
	Version string `json:"version"`
}

type briefWired struct {
	Preset string `json:"preset"`
	Call   string `json:"call"`
	At     string `json:"at"` // file:line
	Doc    string `json:"doc"`
}

type briefOp struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	ID     string `json:"id,omitempty"`
}

// briefSection is one [section] in config.toml and the wiring that reads it —
// the join an agent otherwise makes by hand between two files.
type briefSection struct {
	Name    string `json:"name"`
	BoundBy string `json:"boundBy,omitempty"` // the preset, or the root bundle
}

type briefArtifact struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Note    string `json:"note,omitempty"`
}

// rootSections are the config sections a ROOT BUNDLE binds with no import of
// its own, so brief does not report them as unowned. ultra.New is fib.Product
// (conf + zerolog on [log] + Fiber on [http]) plus the otel waterfall on
// [otel] — see contrib/ultra/ultra.go's New.
var rootSections = map[string][]string{
	"ultra.New":     {"http", "log", "otel"},
	"ultra.Product": {"http", "log", "otel"},
	"fib.Product":   {"http", "log"},
}

func cmdBrief(args []string, out, errW io.Writer) int {
	dir := "."
	jsonOut, check, drift := false, false, false
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--check":
			check = true
		case a == "--drift":
			drift = true
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("brief"), a)
		default:
			dir = a
		}
	}

	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		fmt.Fprintf(errW, "ultra brief: no go.mod in %s — run it from a product root.\n"+
			"a whole workspace at once is a fleet operation: ultra fleet status <dir>\n", displayDir(dir))
		failVerdict(errW, "brief", "not a product module")
		return 2
	}

	p := buildBrief(dir, string(gomod), check, drift)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(p)
	} else {
		renderBrief(out, p)
	}
	verdict(errW, "brief", fmt.Sprintf("%s — %s, %s, %s",
		p.Product, count(len(p.Wired), "preset"), count(len(p.Operations), "operation"),
		count(len(p.Config), "config section")))
	return 0
}

func buildBrief(dir, gomod string, check, drift bool) *briefPack {
	p := &briefPack{
		Product: productName(dir),
		Module:  moduleNameOf(gomod),
		Dir:     displayDir(dir),
		Drift:   "not checked — ultra brief --drift regenerates and compares",
	}

	for _, pin := range modPins(gomod, upgradeModules) {
		p.Framework = append(p.Framework, briefPin{pin.Module, pin.Version})
	}
	p.Replaces = modReplaces(gomod, upgradeModules)
	if check && len(p.Framework) > 0 {
		latest, err := latestFrameworkVersion(dir)
		if err != nil || latest == "" {
			p.Notes = append(p.Notes, "could not resolve the latest release — the pin above is all brief knows")
		} else {
			p.Latest = latest
			target, _ := parseSemver(latest)
			for _, pin := range p.Framework {
				if sv, ok := parseSemver(pin.Version); ok && sv.less(target) {
					p.Behind = true
				}
			}
		}
	}

	root, rerr := loadRoot(dir)
	rootKind := ""
	if rerr != nil {
		p.RootError = rerr.Error()
	} else {
		rootKind = root.Kind
		p.Root = fmt.Sprintf("%s at main.go:%d", root.Kind, root.Fset.Position(root.Call.Pos()).Line)
	}

	wired, werr := scanWired(dir)
	if werr != nil {
		p.Notes = append(p.Notes, "the source scan stopped early: "+werr.Error())
	}
	names := make([]string, 0, len(wired))
	for name := range wired {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w := wired[name]
		call := w.Call
		if call == "" {
			call = "(imported; no " + name + ".Use() — not registered)"
		}
		doc := ""
		if pre := presetByName(name); pre != nil {
			doc = pre.Doc
		}
		p.Wired = append(p.Wired, briefWired{name, call, fmt.Sprintf("%s:%d", w.File, w.Line), doc})
	}

	briefOperations(p, dir)
	briefConfig(p, dir, rootKind, wired)
	briefArtifacts(p, dir, drift)

	if log, err := git(dir, "log", "--oneline", "-5", "--", contractFile); err == nil {
		for _, line := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
			if strings.TrimSpace(line) != "" {
				p.Contracts = append(p.Contracts, strings.TrimSpace(line))
			}
		}
	}
	return p
}

// briefOperations reads the COMMITTED openapi.json through the same reader
// `ultra new --from` uses. It is deliberately the file on disk, not a fresh
// generation: the committed document is what reviewers, the typed client and
// every consumer already see, and whether it still matches the code is the
// separate question the drift line answers.
func briefOperations(p *briefPack, dir string) {
	raw, err := os.ReadFile(filepath.Join(dir, contractFile))
	if err != nil {
		p.Operations = []briefOp{}
		return
	}
	spec, err := readSpec(contractFile, raw)
	if err != nil {
		p.Operations = []briefOp{}
		p.Notes = append(p.Notes, contractFile+" is present but unreadable: "+err.Error())
		return
	}
	p.Contract = true
	p.APITitle, p.APIVersion = spec.Title, spec.APIVersion
	p.Operations = []briefOp{}
	for _, item := range spec.Paths {
		for _, op := range item.Ops {
			p.Operations = append(p.Operations, briefOp{op.Method, op.Path, op.OperationID})
		}
	}
}

// briefConfig joins config.toml against the wiring: every section present,
// with the preset (or root bundle) that reads it, plus the inverse — a wired
// preset whose section nobody wrote, which is the boot failure an agent should
// see before it starts rather than after.
func briefConfig(p *briefPack, dir, rootKind string, wired map[string]*wiredUse) {
	binder := map[string]string{}
	for _, name := range rootSections[rootKind] {
		binder[name] = rootKind
	}
	for name := range wired {
		if pre := presetByName(name); pre != nil && pre.Section != "" {
			binder[pre.Section] = name
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		p.Config = []briefSection{}
		p.Notes = append(p.Notes, "no config.toml — sections are whatever env supplies")
		return
	}
	p.Config = []briefSection{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "["), "]")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, ".") // [auth.jwt] belongs to [auth]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		p.Config = append(p.Config, briefSection{name, binder[name]})
	}
	var missing []string
	for name, by := range binder {
		if !seen[name] && by != rootKind {
			missing = append(missing, fmt.Sprintf("[%s] — %s is wired and binds it", name, by))
		}
	}
	sort.Strings(missing)
	p.ConfigMissing = missing
}

// briefArtifacts inventories the committed derived files by the same names the
// scaffolded drift gate guards. Without --drift it reports PRESENCE only and
// says so — "the artifacts look fine" from a tool that never generated them is
// the one sentence brief must not print.
func briefArtifacts(p *briefPack, dir string, drift bool) {
	for _, path := range []string{contractFile, clientDir, devComposeFile} {
		a := briefArtifact{Path: path}
		if _, err := os.Stat(filepath.Join(dir, path)); err == nil {
			a.Present = true
		}
		p.Artifacts = append(p.Artifacts, a)
	}
	if status, err := git(dir, "status", "--short", "--", contractFile, clientDir, devComposeFile); err == nil {
		for i := range p.Artifacts {
			if strings.Contains(status, p.Artifacts[i].Path) {
				p.Artifacts[i].Note = "uncommitted changes"
			}
		}
	}
	if !drift {
		return
	}
	if !p.Artifacts[0].Present {
		p.Drift = contractFile + " is not committed yet — `go test ./...` bootstraps it"
		return
	}
	doc, err := goRunProduct(dir, "openapi")
	if err != nil {
		p.Drift = "could not regenerate: " + err.Error()
		return
	}
	committed, _ := os.ReadFile(filepath.Join(dir, contractFile))
	if normalizedJSON(committed) == normalizedJSON([]byte(doc)) {
		p.Drift = contractFile + " matches the build"
		return
	}
	p.Drift = contractFile + " is STALE — refresh: go run . openapi > " + contractFile
}

// normalizedJSON re-marshals a document so key order and whitespace cannot
// make two identical contracts look different — the same normalization the
// scaffolded gate applies before it compares.
func normalizedJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func renderBrief(out io.Writer, p *briefPack) {
	fmt.Fprintf(out, "%s — %s\n", p.Product, p.Module)
	for _, pin := range p.Framework {
		line := "framework  " + pin.Module + " " + pin.Version
		switch {
		case p.Latest == "":
		case pin.Version == p.Latest:
			line += " (current)"
		default:
			line += " → " + p.Latest + " available"
		}
		fmt.Fprintln(out, line)
	}
	for _, r := range p.Replaces {
		fmt.Fprintln(out, "replace    "+r)
	}
	if p.Root != "" {
		fmt.Fprintln(out, "root       "+p.Root)
	} else {
		fmt.Fprintln(out, "root       not canonical — "+p.RootError)
	}

	fmt.Fprintf(out, "\nWIRED (%d)\n", len(p.Wired))
	if len(p.Wired) == 0 {
		fmt.Fprintln(out, "  (nothing — this product imports no contrib preset)")
	}
	t := newTable(out)
	for _, w := range p.Wired {
		t.row("  "+w.Preset, w.Call, w.At)
	}
	t.flush()

	head := fmt.Sprintf("\nOPERATIONS (%d) — %s", len(p.Operations), contractFile)
	if p.APITitle != "" {
		head += fmt.Sprintf(" %q %s", p.APITitle, p.APIVersion)
	}
	fmt.Fprintln(out, head)
	// "nothing here" has two causes and they need different next steps: no
	// document at all, versus a document whose product declares no typed op.
	switch {
	case len(p.Operations) > 0:
	case !p.Contract:
		fmt.Fprintf(out, "  (no %s — `go test ./...` bootstraps it from the code)\n", contractFile)
	default:
		fmt.Fprintln(out, "  (the document declares none — no api.Handle op is registered yet)")
	}
	t = newTable(out)
	for i, op := range p.Operations {
		if i == briefOpLimit {
			break
		}
		t.row("  "+op.Method, op.Path, op.ID)
	}
	t.flush()
	if n := len(p.Operations) - briefOpLimit; n > 0 {
		fmt.Fprintf(out, "  … and %s — ultra brief --json for the whole table\n", count(n, "more"))
	}

	fmt.Fprintf(out, "\nCONFIG (%d) — config.toml\n", len(p.Config))
	t = newTable(out)
	for _, s := range p.Config {
		by := s.BoundBy
		if by == "" {
			by = "(no wired preset binds it)"
		}
		t.row("  ["+s.Name+"]", by)
	}
	t.flush()
	for _, m := range p.ConfigMissing {
		fmt.Fprintln(out, "  MISSING "+m)
	}

	fmt.Fprintln(out, "\nARTIFACTS")
	t = newTable(out)
	for _, a := range p.Artifacts {
		state := "absent"
		if a.Present {
			state = "present"
		}
		if a.Note != "" {
			state += ", " + a.Note
		}
		t.row("  "+a.Path, state)
	}
	t.row("  drift", p.Drift)
	t.flush()

	if len(p.Contracts) > 0 {
		fmt.Fprintf(out, "\nCONTRACTS — last %s touching %s\n", count(len(p.Contracts), "commit"), contractFile)
		for _, c := range p.Contracts {
			fmt.Fprintln(out, "  "+c)
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintln(out, "\nnote: "+n)
	}
}

// briefText renders the human pack into a string — the mcp `brief` tool hands
// an agent exactly what a developer reads, not a second format to maintain.
func briefText(dir string, check, drift bool) (string, error) {
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("brief: no go.mod in %s — dir must be a product root", dir)
	}
	var b strings.Builder
	renderBrief(&b, buildBrief(dir, string(gomod), check, drift))
	return b.String(), nil
}
