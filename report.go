package main

// `ultra report <kind> <message>` files a FIELD REPORT.
//
// Product agents already write these — "contrib/pg's pool refused a URL the
// docs say is valid", "DI0001 fired and the message did not name the consumer"
// — and today they land wherever that agent happened to be typing: a PR body,
// a scratch file, a chat message nobody re-reads. The signal is real and the
// inbox is imaginary, so the loop never closes.
//
// This is the inbox, and it is deliberately the smallest thing that can be
// one: a JSONL file at the fleet root. No network, no server, no daemon, no
// schema migration. A human opens .ultra-reports.jsonl, reads the lines, and
// deletes the ones they have acted on. `ultra report list` renders it as a
// table so that reading is one command rather than a jq incantation.
//
// The file lives at the FLEET root — the directory holding .ultra-fleet.json,
// found by walking up — because a friction report is fleet evidence: fifteen
// products hitting the same edge is the argument for changing the framework,
// and fifteen files in fifteen repos is fifteen arguments nobody adds up. With
// no fleet marker anywhere above, the product root files it instead, and the
// verdict says which root it picked.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	reportFile    = ".ultra-reports.jsonl"
	fleetMarker   = ".ultra-fleet.json"
	reportTimeFmt = "2006-01-02 15:04"
)

// reportKinds is the closed set. Four buckets a human can triage at a glance;
// a free-text kind would make the inbox a tag soup within a week.
var reportKinds = []string{"friction", "bug", "docs", "idea"}

// reportEntry is one JSONL line. Every field but the message is STAMPED rather
// than typed: an agent filing a report should spend its tokens on the finding,
// and "which product, which version, which directory" are facts the tool is
// standing in the middle of already.
type reportEntry struct {
	Time      string `json:"time"` // RFC3339, UTC
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Code      string `json:"code,omitempty"`
	Pkg       string `json:"pkg,omitempty"`
	Product   string `json:"product,omitempty"`   // the module path
	Framework string `json:"framework,omitempty"` // the framework pin
	Path      string `json:"path,omitempty"`      // where it was filed, relative to the root
}

// now is the clock, a var so the round-trip test can pin it.
var now = time.Now

func cmdReport(args []string, out, errW io.Writer) int {
	if len(args) == 0 {
		ultraTree().find("report").help(errW)
		return 2
	}
	if args[0] == "list" {
		return reportList(args[1:], out, errW)
	}

	kind := args[0]
	if !slices.Contains(reportKinds, kind) {
		fmt.Fprintf(errW, "ultra report: %q is not a kind — one of %s\n",
			kind, strings.Join(reportKinds, ", "))
		ultraTree().find("report").help(errW)
		return 2
	}

	code, pkg := "", ""
	var words []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--code" && i+1 < len(args):
			code, i = args[i+1], i+1
		case strings.HasPrefix(a, "--code="):
			code = strings.TrimPrefix(a, "--code=")
		case a == "--pkg" && i+1 < len(args):
			pkg, i = args[i+1], i+1
		case strings.HasPrefix(a, "--pkg="):
			pkg = strings.TrimPrefix(a, "--pkg=")
		case strings.HasPrefix(a, "-") && len(words) == 0:
			return badFlag(errW, ultraTree().find("report"), a)
		default:
			words = append(words, a)
		}
	}
	message := strings.TrimSpace(strings.Join(words, " "))
	if message == "" {
		fmt.Fprintln(errW, "ultra report: a report needs a message — say what happened, in one sentence")
		failVerdict(errW, "report", "no message")
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "report", err.Error())
		return 1
	}
	root, kindOfRoot := reportRoot(cwd)
	e := reportEntry{
		Time:    now().UTC().Format(time.RFC3339),
		Kind:    kind,
		Message: message,
		Code:    strings.ToUpper(code),
		Pkg:     pkg,
	}
	if rel, err := filepath.Rel(root, cwd); err == nil && rel != "." {
		e.Path = filepath.ToSlash(rel)
	}
	if gomod, err := os.ReadFile(filepath.Join(productRootOf(cwd), "go.mod")); err == nil {
		e.Product = moduleNameOf(string(gomod))
		if pins := modPins(string(gomod), upgradeModules); len(pins) > 0 {
			e.Framework = pins[0].Version
		}
	}

	path := filepath.Join(root, reportFile)
	if err := appendReport(path, e); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "report", err.Error())
		return 1
	}
	fmt.Fprintf(out, "%s filed → %s\n", kind, path)
	verdict(errW, "report", fmt.Sprintf("%s recorded at the %s (%s)", kind, kindOfRoot, path))
	return 0
}

// reportRoot walks UP for the fleet marker, exactly as a fleet command finds
// the workspace it is standing inside. No marker anywhere above means there is
// no fleet, and the product root is the honest second choice — the report still
// gets written and the verdict names where, so nobody has to guess later.
func reportRoot(dir string) (root, which string) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, fleetMarker)); err == nil {
			return d, "fleet root"
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if p := productRootOf(dir); p != "" {
		return p, "product root"
	}
	return dir, "current directory"
}

// productRootOf is the nearest ancestor holding a go.mod, falling back to dir.
func productRootOf(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// appendReport adds ONE line. Append mode is the whole concurrency story: two
// products filing at once each write a single short line, and the inbox is
// never half-rewritten by a crash the way a read-modify-write JSON array is.
func appendReport(path string, e reportEntry) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func reportList(args []string, out, errW io.Writer) int {
	jsonOut := false
	kind, since := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--kind" && i+1 < len(args):
			kind, i = args[i+1], i+1
		case strings.HasPrefix(a, "--kind="):
			kind = strings.TrimPrefix(a, "--kind=")
		case a == "--since" && i+1 < len(args):
			since, i = args[i+1], i+1
		case strings.HasPrefix(a, "--since="):
			since = strings.TrimPrefix(a, "--since=")
		default:
			return badFlag(errW, ultraTree().find("report").find("list"), a)
		}
	}
	var cutoff time.Time
	if since != "" {
		var ok bool
		if cutoff, ok = parseSince(since, now()); !ok {
			fmt.Fprintf(errW, "ultra report list: --since %q is not a duration (72h, 14d) or a date (2026-07-01)\n", since)
			return 2
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "report list", err.Error())
		return 1
	}
	root, which := reportRoot(cwd)
	path := filepath.Join(root, reportFile)
	entries, err := readReports(path)
	if err != nil {
		fmt.Fprintf(errW, "ultra report list: %v\n", err)
		failVerdict(errW, "report list", "no inbox at the "+which)
		return 1
	}

	kept := entries[:0]
	for _, e := range entries {
		if kind != "" && e.Kind != kind {
			continue
		}
		if !cutoff.IsZero() {
			t, err := time.Parse(time.RFC3339, e.Time)
			if err != nil || t.Before(cutoff) {
				continue
			}
		}
		kept = append(kept, e)
	}
	// Newest first: an inbox is read from the top, and the report filed five
	// minutes ago is the one somebody is asking about.
	slices.Reverse(kept)

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if kept == nil {
			kept = []reportEntry{}
		}
		enc.Encode(kept)
	} else {
		fmt.Fprintf(out, "%s — %s\n", path, count(len(kept), "report"))
		t := newTable(out)
		t.row("WHEN", "KIND", "PRODUCT", "WHERE", "WHAT")
		for _, e := range kept {
			when := e.Time
			if parsed, err := time.Parse(time.RFC3339, e.Time); err == nil {
				when = parsed.Local().Format(reportTimeFmt)
			}
			what := e.Message
			if e.Code != "" {
				what = e.Code + " " + what
			}
			if e.Pkg != "" {
				what = e.Pkg + ": " + what
			}
			t.row(when, e.Kind, e.Product, e.Path, what)
		}
		t.flush()
	}
	verdict(errW, "report list", fmt.Sprintf("%s at the %s", count(len(kept), "report"), which))
	return 0
}

// readReports parses the inbox, skipping lines it cannot read rather than
// failing the listing: a half-written line from a killed process must not cost
// a human the other two hundred reports.
func readReports(path string) ([]reportEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("no reports yet (%s) — file one with `ultra report friction \"...\"`", path)
	}
	defer f.Close()
	var out []reportEntry
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scan.Scan() {
		var e reportEntry
		if json.Unmarshal(scan.Bytes(), &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out, scan.Err()
}

// parseSince accepts a Go duration (72h), a day count (14d) because nobody
// counts hours in a fortnight, or a plain date.
func parseSince(s string, ref time.Time) (time.Time, bool) {
	if days, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") {
		return ref.AddDate(0, 0, -days), true
	}
	if d, err := time.ParseDuration(s); err == nil {
		return ref.Add(-d), true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// fileReport is the mcp `report` tool's core — same discovery, same stamping,
// same line on disk as the CLI writes.
func fileReport(kind, message, code, pkg, dir string) (string, error) {
	if !slices.Contains(reportKinds, kind) {
		return "", fmt.Errorf("report: kind must be one of %s", strings.Join(reportKinds, ", "))
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("report: message is required")
	}
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	root, which := reportRoot(abs)
	e := reportEntry{
		Time:    now().UTC().Format(time.RFC3339),
		Kind:    kind,
		Message: strings.TrimSpace(message),
		Code:    strings.ToUpper(code),
		Pkg:     pkg,
	}
	if rel, err := filepath.Rel(root, abs); err == nil && rel != "." {
		e.Path = filepath.ToSlash(rel)
	}
	if gomod, err := os.ReadFile(filepath.Join(productRootOf(abs), "go.mod")); err == nil {
		e.Product = moduleNameOf(string(gomod))
		if pins := modPins(string(gomod), upgradeModules); len(pins) > 0 {
			e.Framework = pins[0].Version
		}
	}
	path := filepath.Join(root, reportFile)
	if err := appendReport(path, e); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s report filed at the %s: %s", kind, which, path), nil
}
