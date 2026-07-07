package main

import (
	"fmt"
	"sort"
	"strings"
)

// diffGraphs compares two GraphSummary texts semantically: each line is
// "head <- [deps]", where head identifies the provider (type, kind,
// module). Providers are matched by head, so a changed dependency list
// shows as a modification rather than a remove+add.
func diffGraphs(oldS, newS string) (report string, changed bool) {
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
		return "graphs are identical\n", false
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
	return b.String(), true
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
