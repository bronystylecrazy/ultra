package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// analyzerModule is where the real analyzer lives — its own repository, so
// the kernel keeps zero dependencies; `ultra vet` is a thin delegator.
const analyzerModule = "github.com/bronystylecrazy/ultravet/cmd/ultravet"

// cmdVet runs the static analyzer over the current directory and ends with the
// verdict line — the one thing a clean run used to leave out entirely. A clean
// `ultra vet` printed NOTHING and exited 0, which is indistinguishable from a
// vet that never ran; now it says so.
//
// The counts come from the analyzer's own trailer, sniffed out of the stream on
// its way through (vetTail) rather than recomputed here — one report, one set
// of numbers. The machine formats have no trailer, and their verdict says only
// what is true without one.
func cmdVet(args []string, out, errW io.Writer) int {
	tail := &vetTail{w: out}
	code, ran := runVetStatus("", args, tail, errW)
	tail.close()
	switch {
	case !ran:
		failVerdict(errW, "vet", "the analyzer could not run")
	case code == 0:
		verdict(errW, "vet", "no findings")
	case tail.trailer != "":
		failVerdict(errW, "vet", tail.trailer)
	case code == 1:
		failVerdict(errW, "vet", "findings reported")
	default:
		failVerdict(errW, "vet", fmt.Sprintf("the analyzer exited %d", code))
	}
	return code
}

// vetTail passes the analyzer's stdout through byte-for-byte — `ultra vet
// --json | jq` must see exactly what the analyzer wrote — while remembering
// its summary line. The rustc-style report ends with
//
//	ultravet: 3 findings (1 fixable — re-run with -fix to apply)
//
// and that sentence, minus its call to action, IS the verdict's detail.
type vetTail struct {
	w       io.Writer
	line    []byte
	trailer string
}

const vetTrailerPrefix = "ultravet: "

func (t *vetTail) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			t.take()
			continue
		}
		t.line = append(t.line, b)
	}
	return n, err
}

// close flushes a final line the analyzer left unterminated.
func (t *vetTail) close() { t.take() }

func (t *vetTail) take() {
	line := strings.TrimSpace(string(t.line))
	t.line = t.line[:0]
	rest, ok := strings.CutPrefix(line, vetTrailerPrefix)
	if !ok {
		return
	}
	// "(1 fixable — re-run with -fix to apply)" is advice, not a count; the
	// advice already went past the reader on stdout.
	if i := strings.Index(rest, " — re-run with"); i >= 0 {
		rest = rest[:i] + ")"
	}
	t.trailer = rest
}

// speaksAnalyzerProtocol reports whether a command is an analyzer this CLI can
// drive. It asks for -V=full, the handshake every go/analysis tool answers, and
// that is the check rather than a version comparison because a locally built
// analyzer reports "devel" — a string no CLI version can be compared against.
// What matters is not which version it is but whether it understands the flags
// we are about to pass.
func speaksAnalyzerProtocol(name string, prefix ...string) bool {
	out, err := exec.Command(name, append(prefix, "-V=full")...).Output()
	return err == nil && bytes.Contains(out, []byte("version"))
}

// runVet delegates to the analyzer: an installed ultravet binary if
// present, else `go run <module>@latest`. dir, when set, is the working directory
// (the mcp `vet` tool analyzes a named product); empty runs in place.
//
// Flags (-fix, --format) pass through after vetFlags normalizes them; the
// package patterns default to ./... so `ultra vet -fix` means the whole
// module, exactly as `ultra vet` does. Exit codes are the analyzer's: 1 with
// findings, 0 clean, 2 on a load error — every format, unchanged.
func runVet(dir string, args []string, out, errW io.Writer) int {
	code, _ := runVetStatus(dir, args, out, errW)
	return code
}

// runVetStatus is runVet plus the one fact a verdict needs and an exit code
// cannot carry: whether the analyzer ran at all. "Not installed and could not
// be fetched" also exits 1, and reporting that as "findings" would be a lie
// told by the tool about the code.
func runVetStatus(dir string, args []string, out, errW io.Writer) (code int, ran bool) {
	return runAnalyzer(dir, withPatterns(vetFlags(args)), out, errW)
}

// runAnalyzer executes the analyzer with argv exactly as given — no flag
// translation, no default patterns. runVet is the front door; this is the raw
// one, for callers that must speak the analyzer's own protocol (the mcp tool's
// `-json` fallback to the flat go/analysis driver on an older binary).
func runAnalyzer(dir string, args []string, out, errW io.Writer) (code int, ran bool) {
	run := func(name string, argv ...string) int {
		cmd := exec.Command(name, argv...)
		cmd.Dir = dir
		cmd.Stdout = out
		cmd.Stderr = errW
		cmd.Stdin = os.Stdin
		cmd.Env = append(os.Environ(), "GOPRIVATE=github.com/bronystylecrazy/*")
		if err := cmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			return -1 // could not start at all
		}
		return 0
	}

	if path, err := exec.LookPath("ultravet"); err == nil {
		if speaksAnalyzerProtocol(path) {
			return run(path, args...), true
		}
		// Do not hand our flags to a binary that cannot parse them. An
		// ultravet from before -format existed treats "-format=json" as a
		// package pattern, so the user got `malformed import path` from the go
		// toolchain, or — worse, with no flags at all — a green "no findings"
		// from an analyzer years behind the CLI.
		fmt.Fprintf(errW, `ultra vet: the ultravet on PATH is too old for this CLI (%s).

  it does not answer -V=full, so it predates the flags this CLI passes.
  go install %s@latest

falling back to the pinned analyzer for this run.
`, path, analyzerModule)
	}
	// Probe the fallback the same way. `go run` passes the program's exit code
	// through, so a module it cannot resolve exits 1 exactly like an analyzer
	// with findings — and reporting THAT as findings is the same lie in the
	// other direction. -V=full answers only if the analyzer actually built.
	if speaksAnalyzerProtocol("go", "run", analyzerModule+"@latest") {
		return run("go", append([]string{"run", analyzerModule + "@latest"}, args...)...), true
	}
	fmt.Fprintf(errW, `ultra vet: the analyzer is not installed and could not be fetched.

  go install %s@latest

then re-run: ultra vet ./...
`, analyzerModule)
	return 1, false
}

// vetFlags normalizes `ultra vet`'s output flags into the analyzer's own
// spelling, so one marshaling implementation serves the CLI, CI and the mcp
// `vet` tool:
//
//	--json                 → -format=json    the structured finding document
//	--format github        → -format=github  GitHub Actions annotations
//
// --json cannot pass through verbatim: on the ANALYZER's command line -json is
// reserved for the go/analysis flat driver, which `go vet -vettool` and gopls
// speak. Folding `--format X` into one token also keeps withPatterns from
// mistaking the value for a package pattern.
//
// Nothing else is touched — -fix, patterns and any future analyzer flag reach
// it unchanged. No format flag at all means the analyzer decides: the
// rustc-style report normally, GitHub annotations when GITHUB_ACTIONS=true, so
// `ultra vet ./...` in a workflow annotates the pull request with zero config.
func vetFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			out = append(out, "-format=json")
		case (a == "--format" || a == "-format") && i+1 < len(args):
			out = append(out, "-format="+args[i+1])
			i++
		default:
			out = append(out, a)
		}
	}
	return out
}

// withPatterns appends the default ./... when args carry only flags — so a
// bare `ultra vet -fix` analyzes the module rather than handing the analyzer
// a flag and no packages.
func withPatterns(args []string) []string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return args
		}
	}
	return append(append([]string{}, args...), "./...")
}
