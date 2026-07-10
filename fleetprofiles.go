package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// `ultra fleet profiles [dir] [--json]` is DESCRIPTIVE discovery, not a
// prescription: it groups discovered products by the set of preset modules
// (contrib/*, stack/*) they wire — the blessed capability combos the fleet
// already runs. It reports what the architecture looks like today; it
// recommends nothing.

// capabilities reduces a graph to its capability set: the sorted, de-duplicated
// preset modules it wires (contrib/* and stack/* module names). These are the
// blessed building blocks; a product's set is its architectural profile.
func (g *productGraph) capabilities() []string {
	seen := map[string]bool{}
	for _, c := range g.Components {
		m := c.Module
		if strings.HasPrefix(m, "contrib/") || strings.HasPrefix(m, "stack/") {
			seen[m] = true
		}
	}
	caps := make([]string, 0, len(seen))
	for m := range seen {
		caps = append(caps, m)
	}
	sort.Strings(caps)
	return caps
}

type fleetProfile struct {
	Fingerprint  string   `json:"fingerprint"`
	Capabilities []string `json:"capabilities"`
	Members      []string `json:"members"`
}

func fleetProfiles(args []string, out, errW io.Writer) int {
	root := "."
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		default:
			if !strings.HasPrefix(a, "-") {
				root = a
			}
		}
	}

	repos := discoverFleet(root)
	if len(repos) == 0 {
		fmt.Fprintf(errW, "no ultrastack products under %s (looked for go.mod requiring %s)\n", root, frameworkModule)
		return 1
	}

	// Group by capability fingerprint. Products whose graph output can't be
	// obtained/parsed are noted and skipped, not fatal.
	groups := map[string]*fleetProfile{}
	var order []string
	var skipped []string
	for _, p := range repos {
		g, err := fetchGraph(p.Dir)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", p.Module, err.Error()))
			continue
		}
		caps := g.capabilities()
		fp := capFingerprint(caps)
		grp, ok := groups[fp]
		if !ok {
			grp = &fleetProfile{Fingerprint: fp, Capabilities: caps}
			groups[fp] = grp
			order = append(order, fp)
		}
		grp.Members = append(grp.Members, p.Module)
	}

	profiles := make([]*fleetProfile, 0, len(groups))
	for _, fp := range order {
		profiles = append(profiles, groups[fp])
	}
	// Sort by member count (desc), then fingerprint for stability.
	sort.SliceStable(profiles, func(i, j int) bool {
		if len(profiles[i].Members) != len(profiles[j].Members) {
			return len(profiles[i].Members) > len(profiles[j].Members)
		}
		return profiles[i].Fingerprint < profiles[j].Fingerprint
	})

	if jsonOut {
		payload := struct {
			Profiles []*fleetProfile `json:"profiles"`
			Skipped  []string        `json:"skipped,omitempty"`
		}{profiles, skipped}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(payload)
		return 0
	}

	fmt.Fprintln(out, "capability profiles (descriptive: what the fleet already wires, not a prescription)")
	fmt.Fprintln(out)
	for _, prof := range profiles {
		kind := "profile"
		if len(prof.Members) == 1 {
			kind = "unique"
		}
		capsList := strings.Join(prof.Capabilities, ", ")
		if capsList == "" {
			capsList = "(no preset modules)"
		}
		fmt.Fprintf(out, "%s [%s] — %d product(s)\n", kind, prof.Fingerprint, len(prof.Members))
		fmt.Fprintf(out, "  capabilities: %s\n", capsList)
		for _, m := range prof.Members {
			fmt.Fprintf(out, "    - %s\n", m)
		}
		fmt.Fprintln(out)
	}
	for _, s := range skipped {
		fmt.Fprintf(out, "skipped: %s\n", s)
	}
	return 0
}

// capFingerprint is a short stable hash of a sorted capability set — the
// profile's identity.
func capFingerprint(caps []string) string {
	sum := sha256.Sum256([]byte(strings.Join(caps, "\n")))
	return hex.EncodeToString(sum[:4])
}
