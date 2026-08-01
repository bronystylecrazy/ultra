package main

// Announced release notes — the healthz lesson. A framework release that
// moves what a running product OBSERVES ships releases/vX.Y.Z.md in the
// module itself; `ultra upgrade` prints it after resolving the target and
// BEFORE the gate runs. The point is the ## Behavior changes section: a
// green gate only proves the product still passes its own tests — a test
// can stay green while pinning the OLD behavior, and a red one may be
// pinning exactly what moved (three products' boot tests once pinned a 401
// that /healthz stopped answering). Notes print for EVERY product — they
// are release communication, not part of the opt-in compliance harness.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// printReleaseNotes reads releases/<version>.md out of the resolved module —
// the file ships in the module zip, and `go mod download -json` materializes
// the extracted tree in the module cache and names its Dir. Failures degrade
// to one honest line: old tags shipped no notes, and an unreadable cache is
// a note, never an upgrade failure. Returns whether behavior changes are
// listed, so a later gate failure can point back here.
func printReleaseNotes(out io.Writer, dir, version string) bool {
	dl, _ := runIn(dir, "go", "mod", "download", "-json", frameworkModule+"@"+version)
	var m struct{ Dir, Error string }
	// Progress lines ("go: downloading …") may precede the JSON object.
	i := strings.Index(dl, "{")
	if i < 0 || json.Unmarshal([]byte(dl[i:]), &m) != nil || m.Dir == "" {
		reason := firstLineOf(dl)
		if m.Error != "" {
			reason = firstLineOf(m.Error)
		}
		fmt.Fprintf(out, "\n  release notes for %s could not be read from the module cache (%s)\n", version, reason)
		return false
	}
	notes, err := os.ReadFile(filepath.Join(m.Dir, "releases", version+".md"))
	if err != nil {
		fmt.Fprintf(out, "\n  no release notes shipped with %s — releases/%s.md is not in the module.\n", version, version)
		return false
	}
	return renderReleaseNotes(out, version, string(notes))
}

// renderReleaseNotes prints one notes file with the ## Behavior changes
// section highlighted. An entry is a `- ` bullet inside that section; prose
// like "None." lists nothing and earns no warning.
func renderReleaseNotes(out io.Writer, version, notes string) bool {
	col := colorFor(out)
	fmt.Fprintf(out, "\n  release notes — %s (releases/%s.md, shipped in the module)\n\n", version, version)
	hasBehavior, inBehavior := false, false
	for _, line := range strings.Split(strings.TrimRight(notes, "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inBehavior = strings.EqualFold(trimmed, "## Behavior changes")
		}
		if inBehavior && (strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "- ")) {
			if strings.HasPrefix(trimmed, "- ") {
				hasBehavior = true
			}
			fmt.Fprintf(out, "    %s\n", col.yellow(line))
			continue
		}
		fmt.Fprintf(out, "    %s\n", line)
	}
	if hasBehavior {
		fmt.Fprintf(out, "\n  %s       this release lists BEHAVIOR CHANGES — review them before trusting a\n"+
			"             green gate: a test can stay green while pinning the old behavior,\n"+
			"             and a red one may be pinning exactly what moved.\n", col.yellow("note"))
	}
	return hasBehavior
}
