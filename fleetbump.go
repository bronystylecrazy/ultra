package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// `ultra fleet bump [dir] --to vX.Y.Z [--push] [--pr] [--full]` moves every
// discovered product whose framework require is below --to onto it — on a
// branch, verified by build + wiring test. A verify failure reverts the repo
// to how we found it: a half-bump never survives. A local `replace` marks a
// dev checkout (skipped, not bumpable); a dirty worktree is skipped, never
// stashed.

// ---- semver: enough to compare vMAJOR.MINOR.PATCH require lines ----

// semver is the numeric triple; pre-release/build metadata is ignored — a
// bump target is a clean release tag, and comparing release cores is what
// "is this repo behind?" needs.
type semver struct{ major, minor, patch int }

func parseSemver(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// Drop any pre-release/build suffix (v1.2.3-rc1, +meta, pseudo-versions).
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return semver{}, false
		}
		nums[i] = n
	}
	return semver{nums[0], nums[1], nums[2]}, true
}

// less reports a < b.
func (a semver) less(b semver) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}

// ---- fleet bump ----

type bumpResult struct {
	Dir    string `json:"dir"`
	Module string `json:"module"`
	From   string `json:"from"`
	To     string `json:"to"`
	// Status: bumped | already-current | skipped-replace | skipped-dirty | FAILED
	Status string `json:"status"`
	Branch string `json:"branch,omitempty"`
	Detail string `json:"detail,omitempty"`
	Pushed bool   `json:"pushed,omitempty"`
	PR     bool   `json:"pr,omitempty"`
}

func fleetBump(args []string, out, errW io.Writer) int {
	root := "."
	to := ""
	push, pr, full, jsonOut := false, false, false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--to":
			if i+1 < len(args) {
				to = args[i+1]
				i++
			}
		case "--push":
			push = true
		case "--pr":
			pr = true
		case "--full":
			full = true
		case "--json":
			jsonOut = true
		default:
			if strings.HasPrefix(a, "--to=") {
				to = strings.TrimPrefix(a, "--to=")
			} else if !strings.HasPrefix(a, "-") {
				root = a
			}
		}
	}
	if to == "" {
		fmt.Fprintln(errW, "Error: fleet bump needs a target version (--to vX.Y.Z)")
		fmt.Fprintln(errW)
		ultraTree().find("fleet").find("bump").help(errW)
		return 2
	}
	target, ok := parseSemver(to)
	if !ok {
		fmt.Fprintf(errW, "ultra fleet bump: --to %q is not a vMAJOR.MINOR.PATCH version\n", to)
		return 2
	}
	if !strings.HasPrefix(to, "v") {
		to = "v" + to
	}

	repos := discoverFleet(root)
	if len(repos) == 0 {
		fmt.Fprintf(errW, "no ultrastack products under %s (looked for go.mod requiring %s)\n", root, frameworkModule)
		failVerdict(errW, "fleet bump", "no products found under "+root)
		return 1
	}

	var results []bumpResult
	for _, p := range repos {
		results = append(results, bumpOne(p, to, target, push, pr, full))
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	} else {
		// tabwriter: DETAIL is a sentence and PRODUCT is a module path, so two
		// of the four columns are unbounded. See table.go.
		t := newTable(out)
		t.row("PRODUCT", "STATUS", "FROM→TO", "DETAIL")
		for _, r := range results {
			span := r.From
			if r.Status == "bumped" {
				span = r.From + "→" + r.To
			}
			t.row(r.Module, r.Status, span, r.Detail)
		}
		t.flush()
	}

	code, bumped, failed := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "bumped":
			bumped++
		case "FAILED":
			failed++
			code = 1
		}
	}
	verdictFor(errW, code == 0, "fleet bump",
		fmt.Sprintf("%s bumped to %s, %d failed, %d skipped",
			count(bumped, "product"), to, failed, len(results)-bumped-failed))
	return code
}

// bumpOne performs the whole per-repo dance and never leaves a repo in a
// broken half-bumped state: any verify failure reverts to the original branch
// and deletes the bump branch.
func bumpOne(p fleetProduct, to string, target semver, push, pr, full bool) bumpResult {
	r := bumpResult{Dir: p.Dir, Module: p.Module, To: to, From: p.Version}

	// A local `replace` means this is a dev checkout, not a versioned
	// consumer — bumping its require line would be meaningless.
	if p.Replaced {
		r.Status = "skipped-replace"
		r.Detail = "local replace active — dev checkout, not bumpable"
		return r
	}

	// Read every framework require line (root + contrib) and decide if the
	// repo is behind. The lowest require present drives From.
	data, err := os.ReadFile(p.Dir + "/go.mod")
	if err != nil {
		r.Status = "FAILED"
		r.Detail = "could not read go.mod"
		return r
	}
	pins := modPins(string(data), bumpModules)
	if len(pins) == 0 {
		r.Status = "already-current"
		r.Detail = "no framework require to bump"
		return r
	}
	// already-current iff no require is below the target; the lowest pin is From.
	lowest, behind := "", false
	for _, pn := range pins {
		cur, ok := parseSemver(pn.Version)
		if !ok {
			continue
		}
		if cur.less(target) {
			behind = true
		}
		if low, have := parseSemver(lowest); !have || cur.less(low) {
			lowest = pn.Version
		}
	}
	r.From = lowest
	if !behind {
		r.Status = "already-current"
		return r
	}

	// Refuse to touch a dirty worktree — never stash someone's work.
	if dirty, err := gitDirty(p.Dir); err != nil {
		r.Status = "FAILED"
		r.Detail = "git: " + err.Error()
		return r
	} else if dirty {
		r.Status = "skipped-dirty"
		r.Detail = "worktree has uncommitted changes"
		return r
	}

	orig, err := gitCurrentBranch(p.Dir)
	if err != nil {
		r.Status = "FAILED"
		r.Detail = "git: " + err.Error()
		return r
	}
	branch := "ultra-bump-" + to
	if _, err := git(p.Dir, "checkout", "-b", branch); err != nil {
		r.Status = "FAILED"
		r.Detail = "git checkout -b: " + err.Error()
		return r
	}
	r.Branch = branch

	// revert restores the repo to how we found it: discard tracked changes,
	// remove untracked files `go mod tidy` may have created (e.g. a fresh
	// go.sum) — safe because the worktree was verified clean above — return to
	// the original branch, and delete the bump branch.
	revert := func() {
		git(p.Dir, "reset", "--hard")
		git(p.Dir, "clean", "-fd")
		git(p.Dir, "checkout", orig)
		git(p.Dir, "branch", "-D", branch)
	}

	if err := rewriteRequires(p.Dir, to, bumpModules); err != nil {
		revert()
		r.Status = "FAILED"
		r.Detail = "rewrite go.mod: " + err.Error()
		return r
	}
	if out, err := runIn(p.Dir, "go", "mod", "tidy"); err != nil {
		revert()
		r.Status = "FAILED"
		r.Detail = "go mod tidy: " + firstLineOf(out)
		return r
	}
	if out, err := runIn(p.Dir, "go", "build", "./..."); err != nil {
		revert()
		r.Status = "FAILED"
		r.Detail = "go build: " + firstLineOf(out)
		return r
	}
	testArgs := []string{"test", "./...", "-run", "TestWiring", "-count=1"}
	if full {
		testArgs = []string{"test", "./...", "-count=1"}
	}
	if out, err := runIn(p.Dir, "go", testArgs...); err != nil {
		revert()
		r.Status = "FAILED"
		r.Detail = "go test: " + firstLineOf(out)
		return r
	}

	msg := fmt.Sprintf("chore: bump %s %s → %s", frameworkModule, lowest, to)
	if _, err := git(p.Dir, "add", "-A"); err != nil {
		revert()
		r.Status = "FAILED"
		r.Detail = "git add: " + err.Error()
		return r
	}
	if _, err := git(p.Dir, "commit", "-m", msg); err != nil {
		revert()
		r.Status = "FAILED"
		r.Detail = "git commit: " + err.Error()
		return r
	}
	r.Status = "bumped"

	// --push / --pr are best-effort side effects; a failure here is a note,
	// not a broken bump — the branch + commit already exist locally.
	var notes []string
	if push {
		if out, err := git(p.Dir, "push", "-u", "origin", branch); err != nil {
			notes = append(notes, "push failed: "+firstLineOf(out+err.Error()))
		} else {
			r.Pushed = true
		}
	}
	if pr {
		if _, err := exec.LookPath("gh"); err != nil {
			notes = append(notes, "gh not on PATH — skipped PR")
		} else if out, err := runIn(p.Dir, "gh", "pr", "create", "--fill", "--head", branch); err != nil {
			notes = append(notes, "gh pr create failed: "+firstLineOf(out))
		} else {
			r.PR = true
		}
	}
	if len(notes) > 0 {
		r.Detail = strings.Join(notes, "; ")
	}
	return r
}

// bumpModules are the pins fleet bump moves: the kernel and its contrib
// submodule, the two every product requires. (`ultra upgrade` adds the
// analyzer — a fleet bump verifies a build, not a lint toolchain.)
var bumpModules = []string{frameworkModule, frameworkModule + "/contrib"}

// requireEntry decodes one go.mod line as a require entry for one of mods:
// `<module> vX.Y.Z`, with or without a `require ` prefix, inside a block or
// standing alone. Two lines that LOOK like requires are not: a comment (every
// scaffolded go.mod ships a commented replace pair) and a `replace` arrow,
// whose right-hand side is a path — rewriting it would point the module at a
// directory named "v0.9.18".
func requireEntry(raw string, mods []string) (mod, ver string, ok bool) {
	body := strings.TrimPrefix(strings.TrimSpace(raw), "require ")
	if strings.HasPrefix(body, "//") || strings.Contains(body, "=>") {
		return "", "", false
	}
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return "", "", false
	}
	// fields[1], never the last field: `mod v1.2.3 // indirect` ends in a word.
	mod, ver = fields[0], fields[1]
	if !slices.Contains(mods, mod) || !strings.HasPrefix(ver, "v") {
		return "", "", false
	}
	if _, ok := parseSemver(ver); !ok {
		return "", "", false
	}
	return mod, ver, true
}

// rewriteRequires rewrites every require line for mods in the repo's go.mod to
// `to`, leaving all other lines byte-for-byte unchanged.
func rewriteRequires(dir, to string, mods []string) error {
	path := dir + "/go.mod"
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		_, ver, ok := requireEntry(raw, mods)
		if !ok {
			continue
		}
		lines[i] = strings.Replace(raw, ver, to, 1)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// ---- git / process helpers ----

func git(dir string, args ...string) (string, error) {
	return runIn(dir, "git", args...)
}

func runIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gitDirty(dir string) (bool, error) {
	out, err := git(dir, "status", "--porcelain")
	if err != nil {
		return false, errors.New(firstLineOf(out))
	}
	return strings.TrimSpace(out) != "", nil
}

func gitCurrentBranch(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", errors.New(firstLineOf(out))
	}
	return strings.TrimSpace(out), nil
}
