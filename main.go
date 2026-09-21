// ultra is the ultrastack companion CLI.
//
//	ultra --help                  the command tree
//	ultra brief                   # the orientation pack for one product
//	ultra explain DI0101          # the mini-lesson behind any diagnostic code
//	ultra codes                   # list every code
//	ultra report friction "..."   # file a field report at the fleet root
//	ultra diff old.graph new.graph  # semantic diff of two GraphSummary files
//
// Graph files come from the app itself: write app.GraphSummary() to a file
// (a one-line test or ops endpoint) and commit it; `ultra diff` then makes
// architecture drift reviewable — and exits non-zero on change for CI.
//
// Every screen this binary prints comes from ONE tree (help.go) and every
// command ends with ONE verdict line on stderr (verdict.go). See those two
// files before adding a command.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bronystylecrazy/di/diag"
)

func main() {
	// sqlc's process plugins are exec'd, not typed: SQLC_VERSION in the
	// environment and a piped stdin mean the first argument is
	// "/plugin.CodegenService/Generate", not a command. Answer the protobuf
	// and exit before any of the argument parsing below sees it.
	if pluginMode(os.Stdin) {
		os.Exit(runPlugin(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errW io.Writer) int {
	root := ultraTree()

	// Bare `ultra` is a usage error, not a question: it exits 2 so a script
	// that lost its arguments fails. The screen is the same one --help prints,
	// on the stream a usage error belongs to.
	if len(args) == 0 {
		root.help(errW)
		return 2
	}

	// -h/--help and `help` at the ROOT.
	if args[0] == "-h" || args[0] == "--help" {
		root.help(out)
		return 0
	}
	if args[0] == "help" {
		// `ultra help --help` asks about help itself, not for the root screen
		// a second time.
		if wantsHelp(args[1:]) {
			root.find("help").help(out)
			return 0
		}
		return helpCommand(root, args[1:], out, errW)
	}

	node := root.find(args[0])
	if node == nil {
		root.unknown(errW, args[0])
		return 2
	}
	rest := args[1:]

	// -h/--help for THIS command, at every level: `ultra contrib --help`
	// renders contrib, `ultra contrib add --help` renders add.
	if wantsHelp(rest) {
		target := node
		if len(rest) > 0 {
			if sub := node.find(rest[0]); sub != nil {
				target = sub
			}
		}
		target.help(out)
		return 0
	}

	// A pure parent — one whose whole job is to hold subcommands — validates
	// the subcommand here, so the did-you-mean is worded once for the whole
	// binary instead of once per command file.
	if len(node.subs) > 0 && node.args == "" {
		if len(rest) == 0 {
			node.help(errW)
			return 2
		}
		if node.find(rest[0]) == nil {
			node.unknown(errW, rest[0])
			return 2
		}
	}

	switch node.name {
	case "explain":
		return cmdExplain(rest, out, errW)

	case "brief":
		return cmdBrief(rest, out, errW)

	case "report":
		return cmdReport(rest, out, errW)

	case "codes":
		text := codesText()
		fmt.Fprint(out, text)
		verdict(errW, "codes", count(strings.Count(text, "\n"), "code"))
		return 0

	case "version":
		return cmdVersion(out, errW)

	case "new":
		return cmdNew(rest, out, errW)

	case "add":
		return cmdAdd(rest, out, errW)

	case "init":
		return cmdInit(rest, out, errW)

	case "dev":
		return cmdDev(rest, out, errW)

	case "contrib":
		return cmdContrib(rest, out, errW)

	case "vet":
		return cmdVet(rest, out, errW)

	case "upgrade":
		return cmdUpgrade(rest, out, errW)

	case "fleet":
		return cmdFleet(rest, out, errW)

	case "mcp":
		return cmdMCP(os.Stdin, out, errW)

	case "trace":
		return cmdTrace(rest, out, errW)

	case "req":
		return cmdReq(rest, out, errW)

	case "records":
		return cmdRecords(rest, out, errW)

	case "compliance":
		return cmdCompliance(rest, out, errW)

	case "skill":
		return cmdSkill(rest, out, errW)

	case "diff":
		return cmdDiff(rest, out, errW)

	case "breaking":
		return cmdBreaking(rest, out, errW)
	}

	root.unknown(errW, args[0])
	return 2
}

// helpCommand implements `ultra help [command...]` — cobra's spelling of
// --help, and the one a reader who has not learned the flag reaches for.
func helpCommand(root *command, path []string, out, errW io.Writer) int {
	node := root
	for _, name := range path {
		sub := node.find(name)
		if sub == nil {
			node.unknown(errW, name)
			return 2
		}
		node = sub
	}
	node.help(out)
	return 0
}

func cmdExplain(args []string, out, errW io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(errW, "Error: explain takes exactly one diagnostic code")
		fmt.Fprintln(errW, "\nUsage:\n  ultra explain <code>")
		return 2
	}
	code := diag.Code(strings.ToUpper(args[0]))
	lesson, ok := diag.Lesson(code)
	if !ok {
		// A preset code (PG0101, AUTH0201) is not a typo and not missing —
		// it belongs to a package this companion deliberately does not
		// link. Say where it CAN be explained instead of "unknown".
		if isPresetCode(code) {
			fmt.Fprintf(errW, "%s is a preset code — ultra links no preset, so it cannot explain one.\n"+
				"Run `./app explain %s` from the product binary that wires it "+
				"(`./app codes` lists what that binary knows).\n", code, code)
			failVerdict(errW, "explain", string(code)+" — ask the product binary")
			return 1
		}
		fmt.Fprintf(errW, "unknown code %q — run `ultra codes` for the registry\n", args[0])
		failVerdict(errW, "explain", "unknown code "+string(code))
		return 1
	}
	fmt.Fprintln(out, lesson)
	verdict(errW, "explain", string(code))
	return 0
}

func cmdDiff(args []string, out, errW io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(errW, "Error: diff takes two GraphSummary files")
		fmt.Fprintln(errW, "\nUsage:\n  ultra diff <old> <new>")
		return 2
	}
	oldB, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "diff", "unreadable input")
		return 1
	}
	newB, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "diff", "unreadable input")
		return 1
	}
	report, changed, n := diffGraphs(string(oldB), string(newB))
	fmt.Fprint(out, report)
	if changed {
		failVerdict(errW, "diff", count(n, "change"))
		return 1 // CI-friendly: drift fails the step
	}
	verdict(errW, "diff", "graphs identical")
	return 0
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
