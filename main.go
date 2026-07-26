// ultra is the ultrastack companion CLI.
//
//	ultra explain DI0101          # the mini-lesson behind any diagnostic code
//	ultra codes                   # list every code
//	ultra diff old.graph new.graph  # semantic diff of two GraphSummary files
//
// Graph files come from the app itself: write app.GraphSummary() to a file
// (a one-line test or ops endpoint) and commit it; `ultra diff` then makes
// architecture drift reviewable — and exits non-zero on change for CI.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bronystylecrazy/ultrastack/di/diag"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errW io.Writer) int {
	if len(args) == 0 {
		usage(errW)
		return 2
	}
	switch args[0] {
	case "explain":
		if len(args) != 2 {
			fmt.Fprintln(errW, "usage: ultra explain DI0001")
			return 2
		}
		lesson, ok := diag.Lesson(diag.Code(strings.ToUpper(args[1])))
		if !ok {
			// A preset code (PG0101, AUTH0201) is not a typo and not missing —
			// it belongs to a package this companion deliberately does not
			// link. Say where it CAN be explained instead of "unknown".
			if code := diag.Code(strings.ToUpper(args[1])); isPresetCode(code) {
				fmt.Fprintf(errW, "%s is a preset code — ultra links no preset, so it cannot explain one.\n"+
					"Run `./app explain %s` from the product binary that wires it "+
					"(`./app codes` lists what that binary knows).\n", code, code)
				return 1
			}
			fmt.Fprintf(errW, "unknown code %q — run `ultra codes` for the registry\n", args[1])
			return 1
		}
		fmt.Fprintln(out, lesson)
		return 0

	case "codes":
		fmt.Fprint(out, codesText())
		return 0

	case "new":
		return cmdNew(args[1:], out, errW)

	case "dev":
		return cmdDev(args[1:], out, errW)

	case "contrib":
		return cmdContrib(args[1:], out, errW)

	case "vet":
		return cmdVet(args[1:], out, errW)

	case "upgrade":
		return cmdUpgrade(args[1:], out, errW)

	case "fleet":
		return cmdFleet(args[1:], out, errW)

	case "mcp":
		return cmdMCP(os.Stdin, out, errW)

	case "skill":
		return cmdSkill(args[1:], out, errW)

	case "diff":
		if len(args) != 3 {
			fmt.Fprintln(errW, "usage: ultra diff old.graph new.graph")
			return 2
		}
		oldB, err := os.ReadFile(args[1])
		if err != nil {
			fmt.Fprintln(errW, err)
			return 1
		}
		newB, err := os.ReadFile(args[2])
		if err != nil {
			fmt.Fprintln(errW, err)
			return 1
		}
		report, changed := diffGraphs(string(oldB), string(newB))
		fmt.Fprint(out, report)
		if changed {
			return 1 // CI-friendly: drift fails the step
		}
		return 0

	case "help", "-h", "--help":
		usage(out)
		return 0
	}
	fmt.Fprintf(errW, "unknown command %q\n", args[0])
	usage(errW)
	return 2
}

// isPresetCode reports whether a code has the shape of a preset's — four
// digits behind an uppercase prefix that is not the kernel's DI or the
// analyzer's UV. It is a shape test, not a lookup: the whole point is that
// this binary does not have the preset.
func isPresetCode(c diag.Code) bool {
	s := string(c)
	if len(s) < 5 {
		return false
	}
	head, tail := s[:len(s)-4], s[len(s)-4:]
	for _, r := range tail {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range head {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return head != "DI" && head != "UV"
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ultra — the ultrastack companion

  ultra new <name> [--module m]   scaffold a product on the paved road:
                                  main.go + internal/app + config.toml +
                                  Taskfile + AGENTS.md. --db --web --auth are
                                  on by default; --bare strips to the core
  ultra new <name> --from doc.json   REVERSE scaffolding, the legacy-service
                                  on-ramp: an OpenAPI 3.x document in, a
                                  doctrine-shaped product out — a feature
                                  package per tag, one api.Handle per
                                  path+method, request/response structs from the
                                  schemas, and every handler a 501 stub
                                  (<feature>.<op>.not_implemented) so the product
                                  boots and answers on arrival. Prints a
                                  migration report: what came across, what was
                                  guessed, and — by path — what did not (JSON
                                  only; convert YAML first)
  ultra new feature <name>        one file at internal/app/<name>/<name>.go
                                  exporting Use() — then one line in app.go
  ultra dev [--no-web] [--no-infra]   the inner loop in one terminal: boots
                                  docker-compose.dev.yml, builds and serves,
                                  then on every .go change rebuilds and
                                  restarts GRACEFULLY and regenerates
                                  openapi.json + the typed client (changed
                                  bytes only). A red build keeps the previous
                                  binary serving. The frontend dev server runs
                                  beside it under the same Ctrl-C. On a
                                  terminal every child runs on a PTY, so vite
                                  and the API colour and buffer as if you had
                                  run them yourself (--no-pty, or NO_COLOR,
                                  opts out)
  ultra contrib list              this product's capabilities: WIRED (the
                                  presets it imports, with the entry spelling
                                  it used) and AVAILABLE (the rest, one line
                                  each)
  ultra contrib add <preset>      wire one: the argument goes into the
                                  assembly's bundle call (before app.Modules()
                                  when there is one), the import is added, the
                                  file is gofmt'd, and the refresh chain is
                                  printed. Refuses a non-canonical root and
                                  prints the manual one-liner instead. --dry
                                  shows the diff and writes nothing
  ultra contrib remove <preset>   the inverse, with a dependency guard: it
                                  refuses while product packages import the
                                  preset, or a wired preset needs it. --force
                                  proceeds anyway (and reports the build)
  ultra vet [packages] [-fix]     static wiring checks (DI0001-DI0106 graph
                                  family, UV0001-UV0006 lints incl. the
                                  product-structure laws) before anything
                                  runs; -fix applies the machine-safe edits
                                  the report marks [fixable] (the rest stay
                                  advisory — they need a decision)
       --json                     the findings as one JSON array (code,
                                  severity, span, secondary locations,
                                  fixable, fix edits) — the same document the
                                  mcp vet tool returns
       --format github            GitHub Actions annotations; selected
                                  automatically when GITHUB_ACTIONS=true, so
                                  a plain vet run annotates a PR unasked
  ultra upgrade [--to vX.Y.Z]     move THIS product onto a framework release
                                  (latest unless --to): trues the ultrastack
                                  pins, then go mod tidy → build → test. A
                                  failure restores go.mod/go.sum; contract
                                  drift is refreshed and reported, never
                                  committed. --all adds "go get -u ./..." for
                                  every other dependency; --dry shows the plan
                                  and writes nothing
  ultra fleet status|vet [dir]    the whole fleet: versions, graph
                                  fingerprints + drift (--save baselines),
                                  analyzer findings across every product
  ultra fleet bump [dir] --to vX.Y.Z   move products behind vX.Y.Z onto it on a
                                  branch (build + wiring test verify; a failure
                                  reverts); --push/--pr publish, --full full tests
  ultra fleet profiles [dir]      group products by capability set (the preset
                                  modules they wire) — descriptive discovery
  ultra explain <code>            mini-lesson for a diagnostic (e.g. DI0101).
                                  Kernel DIxxxx + analyzer UVxxxx only — a
                                  PRESET code (PG0101) is explained by the
                                  product binary: ./app explain PG0101
  ultra codes                     list every diagnostic code (./app codes adds
                                  the codes that binary's presets registered)
  ultra diff <old> <new>          semantic diff of two GraphSummary files
                                  (exit 1 when the graphs differ)
  ultra mcp                       Model Context Protocol server over stdio:
                                  the graph intelligence as agent tools
                                  (claude mcp add ultrastack -- ultra mcp)
  ultra skill install [--check]   vendor SKILL.md + references/ into
                                  .claude/skills/ultrastack at the EXACT
                                  version go.mod pins (replace wins: live
                                  checkout) — agents read doctrine matching
                                  the code; --check gates CI; ultra upgrade
                                  refreshes it with the bump
  ultra skill gen                 regenerate references/ (errors.md + go-doc
                                  and compiled-example marker blocks); run
                                  from the repo root — the drift test gates it
`)
}
