package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// vetStub makes a fake analyzer answer -V=full, the handshake runAnalyzer probes
// before trusting a binary on PATH. Every stub in this package needs it for the
// same reason a real analyzer does: a binary that cannot answer it is one whose
// flags we must not guess at, and rejecting those is the whole point of the probe.
func vetStub(body string) string {
	const handshake = "[ \"$1\" = \"-V=full\" ] && { echo 'ultravet version devel'; exit 0; }\n"
	if after, ok := strings.CutPrefix(body, "#!/bin/sh\n"); ok {
		return "#!/bin/sh\n" + handshake + after
	}
	return "#!/bin/sh\n" + handshake + body
}

// The normalization the analyzer's own command line requires: on ITS side -json
// is reserved for the go/analysis flat driver, so `ultra vet --json` has to
// arrive as -format=json.
func TestVetFlagsNormalizeFormat(t *testing.T) {
	for _, c := range []struct {
		name     string
		in, want []string
	}{
		{"--json becomes -format=json", []string{"--json"}, []string{"-format=json"}},
		{"-json too", []string{"-json"}, []string{"-format=json"}},
		{"--format folds its value", []string{"--format", "github"}, []string{"-format=github"}},
		{"patterns and -fix pass through", []string{"-fix", "./..."}, []string{"-fix", "./..."}},
		{"a trailing --format with no value is left alone", []string{"--format"}, []string{"--format"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := vetFlags(c.in)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("vetFlags(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// A bare `ultra vet -fix` must analyze the module, not hand the analyzer a flag
// and no packages.
func TestWithPatternsDefaultsToTheModule(t *testing.T) {
	if got := withPatterns([]string{"-fix"}); strings.Join(got, " ") != "-fix ./..." {
		t.Errorf("withPatterns(-fix) = %v", got)
	}
	if got := withPatterns([]string{"-fix", "./cmd/..."}); strings.Join(got, " ") != "-fix ./cmd/..." {
		t.Errorf("an explicit pattern must win: %v", got)
	}
}

// The bug this pins: `ultra vet` ran whatever `ultravet` was on PATH, with no
// check that it understood the flags being passed. An analyzer from before
// -format existed treats "-format=json" as a package pattern, so `ultra vet
// --json` failed with `malformed import path` from the go toolchain — and with
// no flags at all it printed a green "no findings" from an analyzer three weeks
// behind the CLI. -V=full is the handshake every go/analysis tool answers.
func TestSpeaksAnalyzerProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stubs assume POSIX")
	}
	dir := t.TempDir()

	stub := func(name, body string) string {
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
		return p
	}

	// A real analyzer: answers the handshake.
	good := stub("good", `[ "$1" = "-V=full" ] && echo "ultravet version devel buildID=abc" && exit 0
exit 1`)
	if !speaksAnalyzerProtocol(good) {
		t.Error("a binary answering -V=full must be accepted")
	}

	// The stale one: cannot parse a flag, so it reports the flag as a package.
	stale := stub("stale", `echo '-: malformed import path "-V=full": leading dash' >&2
exit 1`)
	if speaksAnalyzerProtocol(stale) {
		t.Error("a binary that cannot parse -V=full must be rejected")
	}

	// Exits 0 but says nothing useful: still not an analyzer.
	mute := stub("mute", `exit 0`)
	if speaksAnalyzerProtocol(mute) {
		t.Error("a silent binary must be rejected")
	}

	if speaksAnalyzerProtocol(filepath.Join(dir, "absent")) {
		t.Error("a missing binary must be rejected")
	}
}
