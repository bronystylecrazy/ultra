package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// `ultra upgrade [dir] [--to vX.Y.Z] [--all] [--dry]` moves ONE product onto a
// framework release from INSIDE it — the single-product form of `ultra fleet
// bump`, which does the same thing to a whole workspace from above. The
// difference is who is driving: fleet bump is a fleet operation (a branch and a
// commit per repo), upgrade is you in your own product, so it never touches
// git — it edits go.mod, verifies, and hands you a diff to review.
//
// The loop is: true the framework pins → `go mod tidy` → `go build ./...` →
// `go test ./...`. A failure restores go.mod and go.sum from the bytes read
// before anything was written — not `git checkout`: a product need not be a
// repository, and a dirty worktree is the user's business, not ours.
//
// The one failure that is not a failure: a framework upgrade may legitimately
// change GENERATED artifacts, and the scaffolded drift gate exists to notice
// exactly that. When TestContractDrift/TestClientDrift are the only failing
// tests, upgrade runs the refresh those gates name, reruns the suite, and
// reports the artifacts it changed — the diff is the upgrade's blast radius.

// upgradeModules are the pins upgrade trues — the kernel, the presets, and the
// analyzer, whichever of them a go.mod actually requires. They ship from one
// repository under one tag, so they move together or the build is a lie.
var upgradeModules = []string{
	frameworkModule,
	frameworkModule + "/contrib",
	frameworkModule + "/analyzer",
}

// The committed derived artifacts, named exactly as the scaffolded gate in
// templates/contract_test.go.tmpl names them — upgrade's refresh must be the
// same command the failure message tells a human to run.
const (
	contractFile = "openapi.json"
	clientDir    = "web/src/lib/api"
)

// latestFrameworkVersion resolves the newest published release. It is a var so
// tests can stub the one step that needs a network; the production path is the
// real `go list`, run inside the module so the module's own environment
// (GOPRIVATE, GOPROXY, a private proxy's credentials) applies.
var latestFrameworkVersion = func(dir string) (string, error) {
	out, err := runIn(dir, "go", "list", "-m", "-versions", frameworkModule)
	if v := latestOf(out); err == nil && v != "" {
		return v, nil
	}
	// A module with a live `replace` (or a go.mod the toolchain dislikes) can
	// answer with no version list at all — ask again from outside any module,
	// where the same environment still selects the same proxy.
	out2, err2 := runIn(os.TempDir(), "go", "list", "-m", "-versions", frameworkModule)
	if v := latestOf(out2); err2 == nil && v != "" {
		return v, nil
	}
	if err == nil {
		err = errors.New(firstLineOf(out))
	}
	return "", err
}

// latestOf picks the highest release tag out of `go list -m -versions` output
// ("<module> v0.1.0 v0.2.0 …"). Pre-releases are skipped: an upgrade target is
// a clean release tag, the same rule fleet bump follows.
func latestOf(listOutput string) string {
	var best semver
	found := ""
	for _, f := range strings.Fields(listOutput) {
		if !strings.HasPrefix(f, "v") || strings.ContainsAny(f, "-+") {
			continue
		}
		sv, ok := parseSemver(f)
		if !ok {
			continue
		}
		if found == "" || best.less(sv) {
			best, found = sv, f
		}
	}
	return found
}

func cmdUpgrade(args []string, out, errW io.Writer) int {
	dir, to := ".", ""
	all, dry := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--to":
			if i+1 >= len(args) {
				fmt.Fprintln(errW, "ultra upgrade: --to needs a version (e.g. --to v0.9.18)")
				return 2
			}
			to = args[i+1]
			i++
		case strings.HasPrefix(a, "--to="):
			to = strings.TrimPrefix(a, "--to=")
		case a == "--all":
			all = true
		case a == "--dry", a == "--dry-run":
			dry = true
		case strings.HasPrefix(a, "-"):
			node := ultraTree().find("upgrade")
			fmt.Fprintf(errW, "Error: unknown flag %q for %q\n\n", a, node.path())
			node.help(errW)
			return 2
		default:
			dir = a
		}
	}
	if to != "" {
		if !strings.HasPrefix(to, "v") {
			to = "v" + to
		}
		if _, ok := parseSemver(to); !ok {
			fmt.Fprintf(errW, "ultra upgrade: --to %q is not a vMAJOR.MINOR.PATCH version\n", to)
			return 2
		}
	}

	gomodPath := filepath.Join(dir, "go.mod")
	origMod, err := os.ReadFile(gomodPath)
	if err != nil {
		fmt.Fprintf(errW, "ultra upgrade: no go.mod in %s — run it from inside a product module.\n"+
			"a whole workspace at once is a fleet operation: ultra fleet bump <dir> --to vX.Y.Z\n",
			displayDir(dir))
		failVerdict(errW, "upgrade", "not a product module")
		return 2
	}
	module := moduleNameOf(string(origMod))
	pins := modPins(string(origMod), upgradeModules)
	if len(pins) == 0 {
		fmt.Fprintf(errW, "ultra upgrade: %s does not require %s — nothing to upgrade\n",
			gomodPath, frameworkModule)
		failVerdict(errW, "upgrade", "this module does not require the framework")
		return 1
	}
	replaces := modReplaces(string(origMod), upgradeModules)

	pinned := to != ""
	if to == "" {
		to, err = latestFrameworkVersion(dir)
		if err != nil || to == "" {
			fmt.Fprintf(errW, "ultra upgrade: could not resolve the latest %s release: %v\n"+
				"pin it yourself: ultra upgrade --to vX.Y.Z\n", frameworkModule, err)
			failVerdict(errW, "upgrade", "could not resolve the latest release")
			return 1
		}
	}
	target, _ := parseSemver(to)

	// A resolved target only moves pins that are BEHIND it. An explicit --to is
	// an instruction, so it also moves pins that are ahead — pinning back to an
	// older release is a legitimate thing to ask for, and refusing would send
	// the user back to editing go.mod by hand.
	from := pins[0].Version
	behind := false
	for _, p := range pins {
		if sv, ok := parseSemver(p.Version); ok {
			if sv.less(target) || (pinned && p.Version != to) {
				behind = true
			}
			if cur, ok2 := parseSemver(from); ok2 && sv.less(cur) {
				from = p.Version
			}
		}
	}

	col := colorFor(out)
	errCol := colorFor(errW)
	fmt.Fprintf(out, "ultra upgrade — %s\n\n", module)
	width := 0
	for _, p := range pins {
		width = max(width, len(p.Module))
	}
	for _, p := range pins {
		// The version you are LEAVING dims and the one you are arriving at
		// is green: the eye should land on the target, and a table of a dozen
		// pins should read as one movement rather than a dozen pairs.
		arrow := col.dim(p.Version) + " → " + col.green(to)
		if p.Version == to {
			arrow = col.dim(p.Version + " (already current)")
		}
		fmt.Fprintf(out, "  %-*s  %s\n", width, p.Module, arrow)
	}
	for _, r := range replaces {
		fmt.Fprintf(out, "\n  %s       replace active: %s\n"+
			"             the pins are trued, but the build verifies against the replace\n"+
			"             target — not the tag. Drop the replace to verify the release.\n",
			col.yellow("note"), r)
	}

	if dry {
		fmt.Fprintf(out, "\n  --dry      nothing was written. would run:\n")
		if all {
			fmt.Fprintf(out, "               go get -u ./...   (--all: every other dependency too)\n")
		}
		fmt.Fprintf(out, "               go mod tidy · go build ./... · go test ./...\n")
		if !behind {
			fmt.Fprintf(out, "  already at %s — the pins would not move\n", to)
			verdict(errW, "upgrade", "--dry: already at "+to+", the pins would not move")
			return 0
		}
		verdict(errW, "upgrade", fmt.Sprintf("--dry: %s would move %s → %s, nothing written",
			count(len(pins), "pin"), from, to))
		return 0
	}
	if !behind && !all {
		fmt.Fprintf(out, "\n  already at %s — nothing to do\n", to)
		verdict(errW, "upgrade", "already at "+to+" — nothing to do")
		return 0
	}

	// Everything below can fail, and a failure must leave the module exactly as
	// found. go.sum may not exist yet; restoring "absent" is part of the deal.
	sumPath := filepath.Join(dir, "go.sum")
	origSum, sumErr := os.ReadFile(sumPath)
	origDoc, docErr := os.ReadFile(filepath.Join(dir, contractFile))
	restore := func() {
		os.WriteFile(gomodPath, origMod, 0o644)
		if sumErr == nil {
			os.WriteFile(sumPath, origSum, 0o644)
		} else {
			os.Remove(sumPath)
		}
	}
	fail := func(step, output string) int {
		restore()
		fmt.Fprintf(errW, "\n%s: %s failed at %s — go.mod and go.sum restored to %s\n\n%s\n",
			errCol.red("ultra upgrade"), step, to, from, headOf(output, 20))
		failVerdict(errW, "upgrade", step+" failed at "+to+" — restored to "+from)
		return 1
	}

	// --all is the everything-else lever: other dependencies move first, then
	// the framework pins are trued on top, so --to always wins over -u.
	if all {
		if o, err := runIn(dir, "go", "get", "-u", "./..."); err != nil {
			return fail("go get -u ./...", o)
		}
	}
	if err := rewriteRequires(dir, to, upgradeModules); err != nil {
		return fail("rewriting go.mod", err.Error())
	}
	if o, err := runIn(dir, "go", "mod", "tidy"); err != nil {
		return fail("go mod tidy", o)
	}
	if o, err := runIn(dir, "go", "build", "./..."); err != nil {
		return fail("go build ./...", o)
	}

	testOut, testErr := runIn(dir, "go", "test", "./...")
	contracts := "unchanged"
	var touched []string
	if testErr != nil {
		if !driftOnly(testOut) {
			return fail("go test ./...", testOut)
		}
		// The gates that are allowed to fail an upgrade: the framework changed
		// what the product's own generators emit. Regenerate and ask again.
		undoRefresh := func() {
			if docErr == nil {
				os.WriteFile(filepath.Join(dir, contractFile), origDoc, 0o644)
			}
		}
		touched, err = refreshContracts(dir, slices.Contains(failedTests(testOut), "TestClientDrift"))
		if err != nil {
			undoRefresh()
			return fail("refreshing the contracts", err.Error())
		}
		testOut, testErr = runIn(dir, "go", "test", "./...")
		if testErr != nil {
			undoRefresh()
			code := fail("go test ./... (after refreshing the contracts)", testOut)
			// A regenerated client is a tree, not a file we saved: say so
			// rather than pretend the revert covered it.
			if slices.Contains(touched, clientDir) {
				fmt.Fprintf(errW, "\n%s: %s was regenerated before the rerun failed — check it with git\n",
					errCol.yellow("note"), clientDir)
			}
			return code
		}
		contracts = "refreshed by the upgrade — review the diff before committing"
	}

	fmt.Fprintf(out, "\n  %s   go mod tidy · go build ./... · go test ./... (%s)\n",
		col.green("verified"), packageCount(testOut))
	fmt.Fprintf(out, "  contracts  %s\n", contracts)
	for _, line := range changedPaths(dir, touched) {
		fmt.Fprintf(out, "               %s\n", line)
	}
	// A vendored skill mirrors the pin — refresh it with the bump so the
	// doctrine agents read never lags the code (manifest presence = opt-in).
	if _, err := os.Stat(filepath.Join(dir, skillDest, skillManifest)); err == nil {
		fmt.Fprintf(out, "\n")
		cmdSkillInstall([]string{dir}, out, errW)
	}
	fmt.Fprintf(out, "\n  nothing was committed — read the diff, then commit it yourself.\n")
	verdict(errW, "upgrade", fmt.Sprintf("%s → %s, verified, nothing committed", from, to))
	return 0
}

// ---- go.mod reading ----

// pin is one framework require line: the module and the version it is held at.
type pin struct{ Module, Version string }

// modPins returns the pinned versions of mods found in a go.mod, in file order.
func modPins(gomod string, mods []string) []pin {
	var pins []pin
	for _, raw := range strings.Split(gomod, "\n") {
		if mod, ver, ok := requireEntry(raw, mods); ok {
			pins = append(pins, pin{mod, ver})
		}
	}
	return pins
}

// modReplaces returns the live `replace` directives pointing any of mods
// somewhere else, rendered as "<module> => <target>". Both the standalone and
// the block form count; a commented-out directive (the pair every scaffolded
// go.mod ships, one uncomment away) does not.
func modReplaces(gomod string, mods []string) []string {
	var out []string
	for _, raw := range strings.Split(gomod, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "//") {
			continue
		}
		line = strings.TrimPrefix(line, "replace ")
		lhs, rhs, ok := strings.Cut(line, "=>")
		if !ok {
			continue
		}
		fields := strings.Fields(lhs)
		if len(fields) == 0 || !slices.Contains(mods, fields[0]) {
			continue
		}
		out = append(out, fields[0]+" => "+strings.TrimSpace(rhs))
	}
	return out
}

// moduleNameOf reads the `module` line, falling back to the directory's own
// name only in the caller's rendering (an unnamed module is not a thing).
func moduleNameOf(gomod string) string {
	for _, raw := range strings.Split(gomod, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(raw), "module "); ok {
			return strings.TrimSpace(v)
		}
	}
	return "this module"
}

func displayDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// ---- go test output reading ----

// failedTests lists the top-level tests a `go test` run reported as failing.
// Subtests fold into their parent: TestContractDrift/openapi is still the
// contract gate.
func failedTests(testOutput string) []string {
	var names []string
	for _, raw := range strings.Split(testOutput, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "--- FAIL: ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		name, _, _ := strings.Cut(fields[0], "/")
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// driftOnly reports whether the ONLY thing wrong with a test run is the
// scaffolded contract drift gate — the one failure an upgrade is allowed to
// fix by regenerating. Best-effort by design: a package that failed to build
// or a run that named no test at all is never treated as drift, because then
// we cannot see what else broke.
func driftOnly(testOutput string) bool {
	if strings.Contains(testOutput, "[build failed]") || strings.Contains(testOutput, "[setup failed]") {
		return false
	}
	failed := failedTests(testOutput)
	if len(failed) == 0 {
		return false
	}
	for _, name := range failed {
		if name != "TestContractDrift" && name != "TestClientDrift" {
			return false
		}
	}
	return true
}

// packageCount renders how much the verification actually covered — "4 test
// packages" is the honest form of "the tests passed".
func packageCount(testOutput string) string {
	n := 0
	for _, raw := range strings.Split(testOutput, "\n") {
		if strings.HasPrefix(raw, "ok ") || strings.HasPrefix(raw, "ok\t") {
			n++
		}
	}
	if n == 0 {
		return "no test packages"
	}
	if n == 1 {
		return "1 test package"
	}
	return fmt.Sprintf("%d test packages", n)
}

// ---- the contract refresh ----

// refreshContracts regenerates the committed derived artifacts by running the
// product's own toolbox — the same two commands the drift gate's failure
// message names, minus the shell redirect.
func refreshContracts(dir string, wantClient bool) ([]string, error) {
	doc, err := goRunProduct(dir, "openapi")
	if err != nil {
		return nil, errors.New("go run . openapi: " + err.Error())
	}
	if err := os.WriteFile(filepath.Join(dir, contractFile), []byte(doc), 0o644); err != nil {
		return nil, err
	}
	touched := []string{contractFile}
	if _, err := os.Stat(filepath.Join(dir, clientDir)); err == nil {
		wantClient = true
	}
	if wantClient {
		if _, err := goRunProduct(dir, "client", "--out", clientDir); err != nil {
			return nil, errors.New("go run . client: " + err.Error())
		}
		touched = append(touched, clientDir)
	}
	return touched, nil
}

// changedPaths asks git which of the refreshed artifacts actually moved. Not a
// repository, or no git — no list, and the report simply says the contracts
// were refreshed.
func changedPaths(dir string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out, err := git(dir, append([]string{"status", "--short", "--"}, paths...)...)
	if err != nil {
		return nil
	}
	var lines []string
	for _, raw := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(raw) != "" {
			lines = append(lines, strings.TrimSpace(raw))
		}
	}
	return lines
}

// headOf caps failure output at n lines — enough to recognize the break,
// never a wall of scrollback.
func headOf(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… and %d more lines", len(lines)-n)
}
