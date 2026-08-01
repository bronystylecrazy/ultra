package main

// `ultra compliance init [dir] [--dry]` scaffolds the class-2 compliance
// templates — the OPT-IN act that activates the requirements machinery.
//
// It is deliberately its own verb, not folded into `ultra init`: init is the
// dormancy-preserving retrofit every product can run forever without
// changing behavior, and writing requirements/ is exactly the opposite — a
// presence-activated covenant. Opting a fleet into an audit trail must be a
// sentence somebody typed, not a side effect of a doctrine refresh.
//
// The contract is init's, exactly: never touch what exists, --dry shows the
// plan, the second run writes nothing. Deliberately ABSENT: empty
// validation/acceptance record templates (records are created BY the acts —
// `ultra req approve` writes its stamp; records/README.md says so), and the
// WP.08/WP.18 generators (deferred by scope decision, named in iso29110.md).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// complianceFiles maps template → output, in report order.
var complianceFiles = []initEntry{
	{"compliance/AGREEMENT.md.tmpl", "AGREEMENT.md"},
	{"compliance/DECISIONS.md.tmpl", "DECISIONS.md"},
	{"compliance/BACKUP.md.tmpl", "BACKUP.md"},
	{"compliance/requirements-README.md.tmpl", "requirements/README.md"},
	{"compliance/EXAMPLE.md.tmpl", "requirements/EXAMPLE.md"},
	{"compliance/exempt.txt.tmpl", "requirements/exempt.txt"},
	{"compliance/records-README.md.tmpl", "records/README.md"},
}

func cmdCompliance(args []string, out, errW io.Writer) int {
	// The parent validated the subcommand; only `init` exists today.
	return cmdComplianceInit(args[1:], out, errW)
}

func cmdComplianceInit(args []string, out, errW io.Writer) int {
	dir, dry := ".", false
	for _, a := range args {
		switch {
		case a == "--dry", a == "--dry-run":
			dry = true
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("compliance").find("init"), a)
		default:
			dir = a
		}
	}

	// The same product gate init keeps: this verb writes records INTO a
	// product, and a product is a module that requires the framework.
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		fmt.Fprintf(errW, "ultra compliance init: no go.mod in %s — run it from inside a product module.\n", displayDir(dir))
		failVerdict(errW, "compliance init", "not a product module")
		return 2
	}
	if len(modPins(string(gomod), upgradeModules)) == 0 {
		fmt.Fprintf(errW, "ultra compliance init: %s does not require %s — nothing to opt in\n",
			filepath.Join(dir, "go.mod"), frameworkModule)
		failVerdict(errW, "compliance init", "this module does not require the framework")
		return 1
	}

	data := struct{ Name, Module, Date string }{productName(dir), moduleNameOf(string(gomod)), today()}
	fmt.Fprintf(out, "ultra compliance init — %s\n\n", data.Module)

	todo := map[string]string{}
	t := newTable(out)
	for _, e := range complianceFiles {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(e.out))); err == nil {
			t.row("  "+e.out, colorFor(out).dim("kept (exists)"))
			continue
		}
		todo[e.tmpl] = e.out
		t.row("  "+e.out, colorFor(out).green(map[bool]string{true: "would write", false: "written"}[dry]))
	}
	t.flush()
	kept := len(complianceFiles) - len(todo)

	if dry {
		fmt.Fprintf(out, "\n  --dry      nothing was written.\n")
		verdict(errW, "compliance init", fmt.Sprintf("--dry: %d would be written, %d kept, nothing written", len(todo), kept))
		return 0
	}
	if err := renderAll(dir, todo, data); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "compliance init", err.Error())
		return 1
	}
	if len(todo) > 0 {
		complianceWiring(dir, out)
	}
	verdict(errW, "compliance init", fmt.Sprintf("%d written, %d kept — requirements/ now activates ultra trace", len(todo), kept))
	return 0
}

// complianceWiring names the wiring this verb cannot do safely — the same
// refuse-and-instruct stance `ultra init` takes on hand-owned files.
func complianceWiring(dir string, out io.Writer) {
	col := colorFor(out)
	if task, _ := os.ReadFile(filepath.Join(dir, "Taskfile.yml")); !strings.Contains(string(task), testRecordFile) {
		fmt.Fprintf(out, "\n  %s       `ultra trace` reads RECORDED runs, and this Taskfile does not\n"+
			"             record them yet. Taskfile.yml is yours — change `task test` to:\n\n"+
			"      - mkdir -p .ultra\n"+
			"      - set -o pipefail; go test -json ./... | tee %s\n\n"+
			"             and add the playwright JSON reporter in web/playwright.config.ts:\n\n"+
			"      reporter: [['list'], ['json', { outputFile: '../%s' }]],\n\n"+
			"             (new `ultra new` products ship both; add .ultra/ to .gitignore)\n",
			col.yellow("note"), testRecordFile, e2eRecordFile)
	}
	if agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")); err == nil && !strings.Contains(string(agents), "requirements/") {
		fmt.Fprintf(out, "\n  %s       AGENTS.md predates the opt-in and carries no requirements laws.\n"+
			"             `ultra init --force AGENTS.md` regenerates it — the shape detection\n"+
			"             now sees requirements/ and renders the law block (diff shown first).\n",
			col.yellow("note"))
	}
	fmt.Fprintf(out, "\nNext:\n"+
		"  ultra new requirement <id> \"<title>\"   # the first REQ file\n"+
		"  ultra trace                             # the derived record, from now on\n"+
		"  AGREEMENT.md / DECISIONS.md / BACKUP.md are yours to fill — the templates\n"+
		"  only mark where the human acts land.\n")
}
