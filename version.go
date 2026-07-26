package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// `ultra version` answers the first question every bug report needs and the
// binary was, until now, the only thing that knew: which ultra is this?
//
// There is no -ldflags stamp here on purpose. The Go toolchain already records
// the answer three different ways depending on how the binary came to exist,
// and a stamp would be a fourth that is wrong whenever someone forgets it:
//
//  1. `go install …/cmd/ultra@v0.9.16` — the module version is in Main.Version.
//  2. `go build ./cmd/ultra` in a checkout — no module version, but the VCS
//     settings carry the revision, and vcs.modified says whether the tree was
//     clean. A hash plus -dirty is the honest answer for a local build.
//  3. Neither (a tarball, `go run` from a cache) — devel, and saying so is
//     better than printing a version that means nothing.
//
// readBuildInfo is a var so the chain above is testable: none of the three
// cases is reachable from a normal `go test` binary, whose own build info
// describes the TEST.
var readBuildInfo = debug.ReadBuildInfo

// versionFacts is what `ultra version` knows.
type versionFacts struct {
	Version string // v0.9.16 | 4f2a1c9-dirty | devel
	Go      string // go1.26.3
	OSArch  string // darwin/arm64
}

// ultraVersion walks the fallback chain. It takes the build info rather than
// reading it so a test can hand it each of the three shapes.
func ultraVersion(bi *debug.BuildInfo, ok bool) versionFacts {
	f := versionFacts{Version: "devel", Go: runtime.Version(), OSArch: runtime.GOOS + "/" + runtime.GOARCH}
	if !ok || bi == nil {
		return f
	}
	if bi.GoVersion != "" {
		f.Go = bi.GoVersion
	}
	// (devel) is what the toolchain writes when there is no module version —
	// it is the absence of an answer, not an answer.
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		f.Version = v
		return f
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		f.Version = rev
		if dirty {
			f.Version += "-dirty"
		}
	}
	return f
}

func cmdVersion(out, errW io.Writer) int {
	f := ultraVersion(readBuildInfo())
	fmt.Fprintf(out, "ultra %s\n%s %s\n", f.Version, f.Go, f.OSArch)
	verdict(errW, "version", "ultra "+f.Version)
	return 0
}
