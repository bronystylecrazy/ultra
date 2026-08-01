package main

// `ultra records freeze [version]` renders the frozen evidence set of one
// release into records/<version>/ — the phase-3 half of the compliance
// harness (WP.09 progress, WP.20 test report, WP.21 traceability, WP.22
// approval stamps, WP.23 verification, plus the signature references):
//
//	trace.md / trace.json    the derived matrix, exactly as trace renders it
//	test.json / e2e.json     COPIES of the recorded runs, mtimes preserved —
//	                         a freeze of stale evidence SAYS so, it never
//	                         launders staleness into freshness
//	requirements.md          the requirements set as-of-now: statuses,
//	                         approval stamps, open questions, CR dispositions
//	frames/REQ-<id>.png      Figma exports for every REQ pinning a frame
//	RECORD.md                the manifest: freshness verdicts, what is
//	                         missing (stated, never implied green), and the
//	                         signature-references section
//
// Presence-activated like the rest of the harness; idempotent per version
// (re-freezing refuses without --force — evidence rewriting must be loud);
// the directory commits WITH the release, by a human.
//
// `ultra records sign <wp> --ref <ptr>` is the companion human act: a
// customer signature exists only in the customer's world, so the frozen
// record holds a POINTER to it, entered by a human. Over MCP it elicits the
// human or stays pending — same fails-safe rule as req approve.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const recordsDir = "records"

// signWPs are the (sign) work products the iso29110 map names as CUSTOMER
// signatures — external acts recorded by reference. The internal WP.22 sign
// is already data (the approve stamp in each REQ file).
var signWPs = []struct{ key, label string }{
	{"agreement", "WP.02 Agreement"},
	{"uat", "UAT validation record"},
	{"acceptance", "WP.01 Acceptance Record"},
}

// figmaAPIBase is a var so tests can point the frame exporter at a mock.
var figmaAPIBase = "https://api.figma.com"

func cmdRecords(args []string, out, errW io.Writer) int {
	// The parent validated the subcommand.
	switch args[0] {
	case "freeze":
		return cmdRecordsFreeze(args[1:], out, errW)
	case "sign":
		return cmdRecordsSign(args[1:], out, errW)
	}
	return 2
}

func cmdRecordsFreeze(args []string, out, errW io.Writer) int {
	dir, version := ".", ""
	force := false
	for _, a := range args {
		norm := a
		if !strings.HasPrefix(norm, "v") {
			norm = "v" + norm
		}
		switch {
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("records").find("freeze"), a)
		case version == "" && func() bool { _, ok := parseSemver(norm); return ok }():
			version = norm
		default:
			dir = a
		}
	}

	if !hasRequirements(dir) {
		fmt.Fprintf(errW, "no %s/ in %s — there is no evidence to freeze.\n"+
			"The compliance harness is presence-activated: opt in with ultra compliance init\n"+
			"(or ultra new requirement <id> \"<title>\"), do the work, then freeze the release.\n",
			requirementsDir, displayDir(dir))
		failVerdict(errW, "records freeze", "no requirements/ — nothing to freeze")
		return 1
	}
	if version == "" {
		tag, err := git(dir, "describe", "--tags", "--abbrev=0")
		if err != nil {
			fmt.Fprintf(errW, "ultra records freeze: no version named and no git tag to default from.\n"+
				"Name the release: ultra records freeze vX.Y.Z\n")
			failVerdict(errW, "records freeze", "no version")
			return 2
		}
		version = strings.TrimSpace(tag)
	}

	// A malformed requirement is refused, not frozen — a half-read record in
	// the frozen evidence would be worse than no freeze at all.
	p, parseErrs := buildTrace(dir, false)
	if len(parseErrs) > 0 {
		for _, e := range parseErrs {
			fmt.Fprintln(errW, e)
		}
		failVerdict(errW, "records freeze", count(len(parseErrs), "malformed requirement file")+" — fix them, then freeze")
		return 1
	}
	reqs, _ := loadRequirements(dir)

	dest := filepath.Join(dir, recordsDir, version)
	relDest := recordsDir + "/" + version
	var carried map[string]string
	if _, err := os.Stat(dest); err == nil {
		if !force {
			fmt.Fprintf(errW, "%s/ already exists — evidence freezes once per release.\n"+
				"Rewriting a frozen record must be loud: --force replaces it (signature\n"+
				"references already entered are carried over).\n", relDest)
			failVerdict(errW, "records freeze", relDest+" exists — refusing to rewrite evidence (--force overrides)")
			return 1
		}
		carried = signedRefs(dest)
		if err := os.RemoveAll(dest); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "records freeze", err.Error())
			return 1
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "records freeze", err.Error())
		return 1
	}

	col := colorFor(out)
	fmt.Fprintf(out, "ultra records freeze — %s at %s\n\n", p.Product, version)
	var written []string
	var notes []string
	write := func(rel, body string) bool {
		if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			fmt.Fprintln(errW, err)
			return false
		}
		written = append(written, rel)
		return true
	}

	// (a) the trace matrix, human render + JSON.
	var traceBuf strings.Builder
	renderTrace(&traceBuf, p, nil)
	traceJSON, _ := json.MarshalIndent(p, "", "  ")
	if !write("trace.md", traceBuf.String()) || !write("trace.json", string(traceJSON)+"\n") {
		failVerdict(errW, "records freeze", "could not write "+relDest)
		return 1
	}

	// (b) the recorded runs: copies with their real mtimes, and the staleness
	// verdict trace computed — stale evidence freezes AS stale, loudly.
	var evidence []string
	stale := 0
	for _, rec := range p.Records {
		if !rec.Present {
			notes = append(notes, rec.Path+" was MISSING at freeze — "+rec.Note)
			continue
		}
		src := filepath.Join(dir, filepath.FromSlash(rec.Path))
		raw, err := os.ReadFile(src)
		if err != nil {
			notes = append(notes, rec.Path+" became unreadable during the freeze: "+err.Error())
			continue
		}
		name := filepath.Base(rec.Path)
		if !write(name, string(raw)) {
			failVerdict(errW, "records freeze", "could not write "+relDest)
			return 1
		}
		if fi, err := os.Stat(src); err == nil {
			os.Chtimes(filepath.Join(dest, name), fi.ModTime(), fi.ModTime())
		}
		verdictStr := "FRESH at freeze"
		if !rec.Fresh {
			stale++
			verdictStr = "STALE at freeze — sources changed after this run; this freeze preserves stale evidence and says so (re-run it and re-freeze --force to replace)"
		}
		evidence = append(evidence, fmt.Sprintf("- %s (frozen as %s): recorded %s, %s", rec.Path, name, rec.Age, verdictStr))
	}

	// (c) the requirements set as-of-now — a rendered summary; git already
	// versions the files themselves.
	if !write("requirements.md", renderReqSummary(p, reqs, version)) {
		failVerdict(errW, "records freeze", "could not write "+relDest)
		return 1
	}

	// (d) frame exports, fail-safe: no token or no API is a stated gap in the
	// record, never a failed freeze.
	frames, frameNotes := exportFrames(dest, reqs)
	written = append(written, frames...)
	notes = append(notes, frameNotes...)

	if len(p.Errors) > 0 {
		notes = append(notes, fmt.Sprintf("the trace carried %s at freeze — this release's evidence is NOT clean; see trace.md", count(len(p.Errors), "gate error")))
	}

	// (e) the manifest, signature markers included.
	if !write("RECORD.md", renderRecordManifest(p, version, evidence, notes, carried)) {
		failVerdict(errW, "records freeze", "could not write "+relDest)
		return 1
	}

	t := newTable(out)
	for _, rel := range written {
		t.row("  "+relDest+"/"+rel, col.green("frozen"))
	}
	t.flush()
	if stale > 0 {
		fmt.Fprintf(out, "\n  %s       %s frozen STALE — the record says so; re-run and re-freeze --force to replace it.\n",
			col.yellow("note"), count(stale, "recorded run"))
	}
	for _, n := range notes {
		fmt.Fprintf(out, "\n  %s       %s\n", col.yellow("note"), n)
	}
	if len(carried) > 0 {
		fmt.Fprintf(out, "\n  %s       %s carried over from the previous freeze.\n",
			col.yellow("note"), count(len(carried), "signature reference"))
	}
	fmt.Fprintf(out, "\n  %s is release evidence — commit it WITH the release commit.\n"+
		"  nothing was committed; signature references land via ultra records sign.\n", relDest)
	detail := fmt.Sprintf("%s → %s (%s", version, relDest, count(len(written), "file"))
	if len(notes) > 0 {
		detail += ", " + count(len(notes), "note")
	}
	verdict(errW, "records freeze", detail+")")
	return 0
}

// renderReqSummary is the WP.13/22/03 snapshot: statuses, approval stamps,
// open questions and change-request dispositions, as of the freeze.
func renderReqSummary(p *tracePack, reqs []*reqFile, version string) string {
	derived := map[string]string{}
	for _, r := range p.Requirements {
		derived[r.ID] = r.Derived
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# requirements at freeze — %s (%s)\n", version, p.Product)
	for _, r := range reqs {
		fmt.Fprintf(&b, "\n## REQ-%s — %s\n\n", r.ID, r.Title)
		if r.Status == "approved" {
			fmt.Fprintf(&b, "- status: approved — by %s, %s (the WP.22 stamp)\n", r.ApprovedBy, r.ApprovedDate)
		} else {
			fmt.Fprintf(&b, "- status: draft — nobody has approved this requirement\n")
		}
		fmt.Fprintf(&b, "- derived at freeze: %s\n", derived[r.ID])
		if len(r.Open) == 0 {
			b.WriteString("- open questions: none\n")
		}
		for _, q := range r.Open {
			fmt.Fprintf(&b, "- OPEN QUESTION: %s\n", q)
		}
		if len(r.Changes) == 0 {
			b.WriteString("- change requests: none\n")
		}
		for _, c := range r.Changes {
			disp := "UNDECIDED — blocked on a human"
			if c.Disposition != "" {
				disp = c.Disposition + " (" + c.DecidedBy + ")"
			}
			fmt.Fprintf(&b, "- change %s (requested-by: %s): %s → %s\n", c.Date, c.RequestedBy, c.Description, disp)
		}
	}
	return b.String()
}

// renderRecordManifest writes RECORD.md — the freeze's own record: what is
// inside, how fresh it was, what is missing (stated), and the signature
// references, UNSIGNED until a human enters each pointer.
func renderRecordManifest(p *tracePack, version string, evidence, notes []string, carried map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s/%s — frozen release evidence\n\n", recordsDir, version)
	fmt.Fprintf(&b, "- product: %s\n- version: %s\n- frozen: %s\n", p.Product, version, today())
	b.WriteString("\n## Contents\n\n" +
		"- trace.md / trace.json — the derived traceability matrix (WP.21) as rendered at freeze\n" +
		"- test.json / e2e.json — copies of the recorded runs (WP.20/WP.23), mtimes preserved\n" +
		"- requirements.md — statuses, approval stamps (WP.22), open questions, change-request dispositions (WP.03)\n" +
		"- frames/ — Figma frame exports for requirements that pin one\n")
	b.WriteString("\n## Evidence freshness — the staleness verdict at freeze\n\n")
	if len(evidence) == 0 {
		b.WriteString("- no recorded runs were present — see the notes below\n")
	}
	for _, e := range evidence {
		b.WriteString(e + "\n")
	}
	if len(notes) > 0 {
		b.WriteString("\n## Notes — what this record is missing, stated\n\n")
		for _, n := range notes {
			b.WriteString("- " + n + "\n")
		}
	}
	b.WriteString("\n## Signature references\n\n" +
		"Each (sign) work product is either a POINTER to the externally signed\n" +
		"artifact (file / scan / message-id — entered by a human via\n" +
		"`ultra records sign <wp> --ref <pointer>`) or explicitly UNSIGNED.\n" +
		"An agent cannot fabricate a signature it can only point at.\n\n")
	for _, wp := range signWPs {
		val := "UNSIGNED"
		if v, ok := carried[wp.key]; ok {
			val = v
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", wp.key, wp.label, val)
	}
	return b.String()
}

// signedRefs reads the non-UNSIGNED signature values back off a frozen
// RECORD.md, so a --force re-freeze carries the human-entered pointers
// instead of destroying them.
func signedRefs(dest string) map[string]string {
	raw, err := os.ReadFile(filepath.Join(dest, "RECORD.md"))
	if err != nil {
		return nil
	}
	refs := map[string]string{}
	for _, wp := range signWPs {
		prefix := "- " + wp.key + " (" + wp.label + "): "
		for _, line := range strings.Split(string(raw), "\n") {
			if val, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(val) != "UNSIGNED" {
				refs[wp.key] = strings.TrimSpace(val)
			}
		}
	}
	return refs
}

// exportFrames renders every pinned frame through the Figma REST API. Every
// failure is a NOTE in the record — the freeze states what is missing and
// never fails over pixels.
func exportFrames(dest string, reqs []*reqFile) (written, notes []string) {
	var pinned []*reqFile
	for _, r := range reqs {
		if r.Frame != "" {
			pinned = append(pinned, r)
		}
	}
	if len(pinned) == 0 {
		return nil, nil
	}
	token := os.Getenv("FIGMA_ACCESS_TOKEN")
	if token == "" {
		return nil, []string{fmt.Sprintf("frame exports skipped — FIGMA_ACCESS_TOKEN is not set (%s pin a frame); the frame references are recorded in requirements.md, the pixels are not", count(len(pinned), "requirement"))}
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, r := range pinned {
		rel, err := exportFrame(client, token, dest, r)
		if err != nil {
			notes = append(notes, fmt.Sprintf("frame export for REQ-%s skipped — %v; the reference is recorded, the export is not", r.ID, err))
			continue
		}
		written = append(written, rel)
	}
	return written, notes
}

// exportFrame resolves one frame ref (file key + node-id, and the pinned
// version-id when the URL carries one) and writes frames/REQ-<id>.png.
func exportFrame(client *http.Client, token, dest string, r *reqFile) (string, error) {
	u, uerr := url.Parse(r.Frame)
	key, node, pinnedVer := "", "", ""
	if uerr == nil {
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i, s := range segs {
			if (s == "design" || s == "file" || s == "proto") && i+1 < len(segs) {
				key = segs[i+1]
				break
			}
		}
		// dash-spelled in URLs, colon-spelled in the API.
		node = strings.ReplaceAll(u.Query().Get("node-id"), "-", ":")
		pinnedVer = u.Query().Get("version-id")
	}
	if key == "" || node == "" {
		return "", fmt.Errorf("frame %q is not a Figma node URL (need …/design/<key>/…?node-id=…)", r.Frame)
	}
	api := figmaAPIBase + "/v1/images/" + key + "?ids=" + url.QueryEscape(node) + "&format=png"
	if pinnedVer != "" {
		api += "&version=" + url.QueryEscape(pinnedVer)
	}
	req, err := http.NewRequest("GET", api, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Figma-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the Figma API is unreachable: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the Figma API answered %d: %s", resp.StatusCode, firstLineOf(string(body)))
	}
	var res struct {
		Images map[string]string `json:"images"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Images[node] == "" {
		return "", fmt.Errorf("the Figma API returned no image for node %s", node)
	}
	imgResp, err := client.Get(res.Images[node])
	if err != nil {
		return "", fmt.Errorf("the rendered image is unreachable: %v", err)
	}
	img, err := io.ReadAll(imgResp.Body)
	imgResp.Body.Close()
	if err != nil || imgResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the rendered image did not download (status %d)", imgResp.StatusCode)
	}
	rel := "frames/REQ-" + r.ID + ".png"
	if err := os.MkdirAll(filepath.Join(dest, "frames"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(rel)), img, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// ---- the sign verb ----

func cmdRecordsSign(args []string, out, errW io.Writer) int {
	var wp, ref, version, by, dir string
	flagVal := func(i *int) string {
		if *i+1 < len(args) {
			*i++
			return args[*i]
		}
		return ""
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--ref":
			ref = flagVal(&i)
		case a == "--version":
			version = flagVal(&i)
		case a == "--by":
			by = flagVal(&i)
		case strings.HasPrefix(a, "-"):
			return badFlag(errW, ultraTree().find("records").find("sign"), a)
		case wp == "":
			wp = a
		default:
			dir = a
		}
	}
	if wp == "" || ref == "" {
		ultraTree().find("records").find("sign").help(errW)
		return 2
	}
	if dir == "" {
		dir = "."
	}
	if by == "" {
		by = gitUserName(dir)
	}
	msg, err := recordsSign(dir, version, wp, ref, by)
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "records sign "+wp, firstLineOf(err.Error()))
		return 1
	}
	fmt.Fprint(out, msg)
	verdict(errW, "records sign "+wp, firstLineOf(msg))
	return 0
}

// recordsSign is the shared core (CLI + MCP): it turns one UNSIGNED marker
// in a frozen RECORD.md into the reference, stamped who/when. It refuses to
// overwrite an entered reference — a signature pointer is entered once.
func recordsSign(dir, version, wp, ref, by string) (string, error) {
	label := ""
	for _, s := range signWPs {
		if s.key == wp {
			label = s.label
		}
	}
	if label == "" {
		return "", fmt.Errorf("unknown work product %q — the signed work products are: agreement, uat, acceptance", wp)
	}
	if by == "" {
		return "", fmt.Errorf("no signer-of-record identity — set `git config user.name` or pass --by; the record names the human who entered the reference")
	}
	if version == "" {
		// Default to the latest frozen version on disk.
		entries, _ := os.ReadDir(filepath.Join(dir, recordsDir))
		var best semver
		for _, e := range entries {
			if sv, ok := parseSemver(e.Name()); ok && e.IsDir() && (version == "" || best.less(sv)) {
				best, version = sv, e.Name()
			}
		}
		if version == "" {
			return "", fmt.Errorf("nothing is frozen yet — signature references land in a frozen record: ultra records freeze <version> first")
		}
	}
	path := filepath.Join(dir, recordsDir, version, "RECORD.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no %s/%s/RECORD.md — freeze that release first: ultra records freeze %s", recordsDir, version, version)
	}
	prefix := "- " + wp + " (" + label + "): "
	lines := strings.Split(string(raw), "\n")
	found := false
	for i, line := range lines {
		val, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		found = true
		if strings.TrimSpace(val) != "UNSIGNED" {
			return "", fmt.Errorf("the %s reference in %s/%s is already entered: %s\na signature reference is entered once; if the record itself is wrong, that is a human edit to RECORD.md with its own commit message", wp, recordsDir, version, strings.TrimSpace(val))
		}
		lines[i] = prefix + fmt.Sprintf("%s — entered by %s, %s", ref, by, today())
	}
	if !found {
		return "", fmt.Errorf("%s/%s/RECORD.md carries no %q signature line — re-freeze it with a current ultra (--force)", recordsDir, version, wp)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("recorded the %s signature reference in %s/%s — %s (entered by %s, %s)\nthe signed artifact lives in the customer's world; this record points at it\n",
		label, recordsDir, version, ref, by, today()), nil
}

// toolRecordsSign: the MCP spelling — a signature reference is a human act,
// so the tool elicits the human or stays pending, exactly like req_approve.
func toolRecordsSign(s *mcpServer, raw json.RawMessage) (string, error) {
	var a struct {
		Dir, WP, Ref, Version string
	}
	json.Unmarshal(raw, &a)
	if a.Dir == "" || a.WP == "" || a.Ref == "" {
		return "", fmt.Errorf("records_sign: dir, wp and ref are required")
	}
	if !s.canElicit {
		return fmt.Sprintf("pending human signature — this MCP client declared no elicitation capability, "+
			"so no human can confirm the reference from here. A human runs:\n\n  ultra records sign %s --ref %q\n\n"+
			"in a terminal (identity stamps from their git config user.name). The record stays UNSIGNED until then.", a.WP, a.Ref), nil
	}
	by := gitUserName(a.Dir)
	if by == "" {
		return "", fmt.Errorf("no signer-of-record identity on this machine — set `git config user.name`")
	}
	action, _, err := s.elicitCreate(
		fmt.Sprintf("Record %q as the signed %s reference in records/? This states the artifact exists, was signed in the customer's world, and stamps %q as the human who entered it.", a.Ref, a.WP, by),
		map[string]any{"confirm": map[string]any{"type": "boolean", "description": "true to record the reference"}},
		"confirm")
	if err != nil {
		return "", err
	}
	if action != "accept" {
		return fmt.Sprintf("the human answered %q — the record stays UNSIGNED", action), nil
	}
	return recordsSign(a.Dir, a.Version, a.WP, a.Ref, by)
}
