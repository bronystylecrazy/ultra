package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bronystylecrazy/ultrastack/di/diag"
)

// codesText is the `ultra codes` registry: every code with its one-line
// summary. Shared by the CLI and the mcp `codes` tool.
//
// Through the table helper like every other column in this binary: kernel
// codes are all six characters today, so the old fixed gap happened to line
// up — but "happens to line up" is not a property, and the day a longer code
// is registered the whole listing shears.
func codesText() string {
	var b strings.Builder
	t := newTable(&b)
	for _, c := range diag.AllCodes() {
		lesson, _ := diag.Lesson(c)
		t.row(string(c), firstLine(lesson))
	}
	t.flush()
	return b.String()
}

// diffGraphs compares two GraphSummary texts semantically: each line is
// "head <- [deps]", where head identifies the provider (type, kind,
// module). Providers are matched by head, so a changed dependency list
// shows as a modification rather than a remove+add.
//
// It returns the change count as well as the rendered report so the caller's
// verdict line can say how many without re-parsing its own output.
func diffGraphs(oldS, newS string) (report string, changed bool, changes int) {
	oldM := parseSummary(oldS)
	newM := parseSummary(newS)

	var added, removed, modified []string
	for head, deps := range newM {
		oldDeps, existed := oldM[head]
		switch {
		case !existed:
			added = append(added, head+" <- "+deps)
		case oldDeps != deps:
			modified = append(modified, fmt.Sprintf("%s\n      - %s\n      + %s", head, oldDeps, deps))
		}
	}
	for head, deps := range oldM {
		if _, exists := newM[head]; !exists {
			removed = append(removed, head+" <- "+deps)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(modified)

	n := len(added) + len(removed) + len(modified)
	if n == 0 {
		return "graphs are identical\n", false, 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "graph diff (%d change(s)):\n", n)
	for _, l := range added {
		fmt.Fprintf(&b, "  + %s\n", l)
	}
	for _, l := range removed {
		fmt.Fprintf(&b, "  - %s\n", l)
	}
	for _, l := range modified {
		fmt.Fprintf(&b, "  ~ %s\n", l)
	}
	return b.String(), true, n
}

// parseSummary maps "head <- [deps]" lines to head → deps.
func parseSummary(s string) map[string]string {
	m := map[string]string{}
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		head, deps, found := strings.Cut(line, " <- ")
		if !found {
			m[line] = ""
			continue
		}
		m[head] = deps
	}
	return m
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	// Lessons open with "DI0001 — summary"; keep just the summary.
	if _, summary, found := strings.Cut(line, " — "); found {
		return summary
	}
	return line
}
