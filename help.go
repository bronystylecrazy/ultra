package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// The help surface, cobra-shaped without cobra.
//
// Every command in this binary is one node in the tree below, and the tree is
// the ONLY place a description lives: the root listing, a parent's subcommand
// listing, a leaf's `--help`, and the did-you-mean on a typo all render from
// it. That is the whole reason it exists — the previous design kept one wall
// of prose in usage() and the real flag parsing somewhere else, so the two
// drifted every time a flag landed.
//
// The layout is cobra's, deliberately: title, Usage:, Available Commands:,
// Flags:, and the `Use "x [command] --help"` footer. Millions of Go CLIs have
// trained the reader's eye on that shape; importing 40k lines of dependency to
// reproduce ~150 is not a trade this repo makes.

// flagDoc is one row of a Flags: block. spec is the flag as typed
// ("--format github"), desc the one-line explanation.
type flagDoc struct{ spec, desc string }

// helpFlag is the row every node carries — the one flag that works at every
// level of the tree.
var helpFlag = flagDoc{"-h, --help", "help for %s"}

// command is one node. A node with subs renders a parent's help; a node
// without renders a leaf's.
type command struct {
	name    string
	aliases []string
	// short is the one-line description in a listing. Keep it under 58
	// characters: two columns plus the longest command name is a terminal
	// width, and a wrapped listing is a listing nobody scans.
	short string
	// args is what follows the command path on the Usage: line.
	args string
	// long is the description a leaf's --help prints above Usage. Empty
	// falls back to short, exactly as cobra does.
	long  string
	flags []flagDoc
	subs  []*command

	parent *command
}

// ultraTree is the command surface of this binary, and the source every help
// screen renders from.
func ultraTree() *command {
	root := &command{
		name:  "ultra",
		short: "the ultrastack companion",
		subs: []*command{
			{
				name:  "brief",
				args:  "[dir] [--json] [--check] [--drift]",
				short: "the orientation pack: one product, one page",
				long: `Everything an agent needs before it touches a product, in one page.

Name and module, the framework pin, the canonical root, the wired presets
with the entry spelling each used, the operation table from openapi.json,
which of those operations are still 501 stubs, the config sections and
which wiring reads each, the state of the committed artifacts, and the
last commits that moved the contract.

The STUBS section is the plan for a long port: a handler scaffolded by
--from declares <feature>.<op>.not_implemented, so the committed contract
counts the work left and any successor session resumes from it.

It replaces the five commands that answer those separately, and it is
built for a context window: compact, grep-friendly, --json for the whole
table when the human render caps it.

Everything is a static read of files on disk. The two answers that cost
something are opt-in, and their absence is stated rather than guessed:
--check resolves the latest release (network), --drift regenerates the
contract to compare it (a product build).`,
				flags: []flagDoc{
					{"--json", "the whole pack as one JSON document"},
					{"--check", "also resolve the latest release (needs the network)"},
					{"--drift", "also regenerate openapi.json and compare"},
				},
			},
			{
				name:  "breaking",
				args:  "<old.json> <new.json> | --against <ref> [dir]",
				short: "gate a contract change against its consumers",
				long: `Compare two OpenAPI documents and name every change that breaks a
consumer of the old one.

It classifies by the DIRECTION the bytes travel, not by whether the
document changed. A response field is READ by consumers, so removing it
breaks them. A request field is WRITTEN by them, and contrib/api decodes
request bodies with a plain json.Decoder — an unknown key is dropped, not
rejected — so removing one is a WARNING, and the warning says the part
that hurts: the sender's intent is discarded with no 4xx.

The mirror runs through the whole taxonomy. Adding a REQUEST enum value is
safe; adding a RESPONSE one is a warning, because a consumer switching
exhaustively has no arm for it. An array going nullable in a response is
BREAKING — the null→[] guarantee means nobody wrote the null check.

Additive changes are reported in their own section and never gate: "no
breaking changes" and "no changes" are different answers.

Exit 1 with named findings when something breaks, 0 otherwise — so the
one line that gates every pull request is:

  ultra breaking --against origin/main`,
				flags: []flagDoc{
					{"--against <ref>", "compare openapi.json at a git ref against the working tree"},
					{"--json", "the findings as one JSON document"},
				},
			},
			{
				name:  "codes",
				short: "list every diagnostic code with its summary",
				long: `List every diagnostic code this binary knows, one line each.

ultra links no preset, so this is the kernel's DIxxxx plus the analyzer's
UVxxxx. A product binary answers the same question about ITS registry —
./app codes adds the codes the presets it wired registered at init.`,
			},
			{
				name:  "contrib",
				short: "this product's capabilities: list, add, remove",
				long: `Manage the capability set of ONE product — the contrib presets its
assembly wires.

Run it from a product root (the directory holding main.go). Every
subcommand reads the same canonical root ultravet resolves, so what
contrib reports and what the graph actually builds cannot disagree.

There is no "contrib upgrade": the presets ship as one module under one
tag, so they version in lockstep — ultra upgrade moves them all, and
ultra upgrade --check compares without writing.`,
				subs: []*command{
					{
						name:    "list",
						aliases: []string{"ls"},
						args:    "[dir]",
						short:   "WIRED and AVAILABLE capabilities, one line each",
						long: `Inventory this product's capabilities.

WIRED is the presets it imports, with the entry spelling it actually used
(pg.Use(), api.Use(api.Info{…})) and the file:line that wired it — an
import with no Use() is called out, because that is a DI0001 waiting at
Validate. AVAILABLE is the rest, each with the config section it reads and
whether it wants a dev service.`,
					},
					{
						name:  "add",
						args:  "<preset> [dir] [--dry]",
						short: "wire one preset into the assembly",
						long: `Wire one preset.

The argument goes into the assembly's bundle call (before app.Modules
when there is one), the import is added, the file is gofmt'd, and the
refresh chain is printed. A non-canonical root is refused with the manual
one-liner to paste instead — this command edits an AST it can prove it
understands, or it edits nothing.`,
						flags: []flagDoc{{"--dry", "show the diff and write nothing"}},
					},
					{
						name:    "remove",
						aliases: []string{"rm"},
						args:    "<preset> [dir] [--dry] [--force]",
						short:   "unwire one preset, behind a dependency guard",
						long: `Unwire one preset — add's inverse, with a guard.

It refuses while product packages import the preset, or while a wired
preset needs it (jobs without pg is a DI0001, not a smaller product), and
names what is holding it. --force proceeds anyway and reports the build.`,
						flags: []flagDoc{
							{"--dry", "show the diff and write nothing"},
							{"--force", "unwire despite the dependency guard"},
						},
					},
				},
			},
			{
				name:  "compliance",
				short: "the class-2 compliance templates: init",
				long: `The ISO/IEC 29110 class-2 harness — templates and records for a product
that must answer an auditor.

This parent holds the OPT-IN act. Everything downstream of it
(requirements/, ultra trace, ultra req) is presence-activated: a product
that never runs this is byte-for-byte unaffected.`,
				subs: []*command{
					{
						name:  "init",
						args:  "[dir] [--dry]",
						short: "scaffold the class-2 templates and requirements/",
						long: `Opt one product into the compliance harness.

It writes the class-2 templates — AGREEMENT.md (WP.02, signed by reference),
DECISIONS.md (WP.07, seeded), BACKUP.md (WP.12, the stated policy),
requirements/README.md + EXAMPLE.md + exempt.txt, records/README.md — and
nothing else. Deliberately NO empty record templates: records are created
BY the acts (ultra req approve writes its stamp), never filled into forms.

The contract is ultra init's exactly: a file that exists is never touched,
--dry shows the plan, the second run writes nothing. The presence of
requirements/ is what activates ultra trace from then on.`,
						flags: []flagDoc{{"--dry", "show the plan and write nothing"}},
					},
				},
			},
			{
				name:  "dev",
				args:  `[--no-web] [--no-infra] [--no-pty] [-- <serve args>]`,
				short: "the inner loop in one terminal",
				long: `THE inner loop, in one terminal.

It boots docker-compose.dev.yml, builds and serves, then on every .go
change rebuilds and restarts GRACEFULLY and regenerates openapi.json plus
the typed client (changed bytes only). A red build keeps the previous
binary serving. The frontend dev server runs beside it under the same
Ctrl-C. On a terminal every child runs on a PTY, so vite and the API
colour and buffer as if you had run them yourself.`,
				flags: []flagDoc{
					{"--no-web", "skip the frontend dev server"},
					{"--no-infra", "skip docker-compose.dev.yml"},
					{"--no-pty", "run children on pipes, not a pty"},
					{`--build-flags "..."`, "extra flags for the go build (e.g. -race)"},
					{"-- <serve args>", "everything after -- goes to the served binary"},
				},
			},
			{
				name:  "diff",
				args:  "<old> <new>",
				short: "semantic diff of two GraphSummary files",
				long: `Diff two GraphSummary files semantically.

Providers are matched by head — type, kind, module — so a changed
dependency list reads as a modification, not a remove plus an add.

Graph files come from the app itself: write app.GraphSummary() to a file
(a one-line test or an ops endpoint) and commit it. Architecture drift
then shows up in review, and this command exits 1 when the graphs differ,
so CI can fail on it.`,
			},
			{
				name:  "explain",
				args:  "<code>",
				short: "the mini-lesson behind one diagnostic code",
				long: `Print the worked mini-lesson for a diagnostic code.

Kernel DIxxxx and analyzer UVxxxx only. A PRESET code (PG0101) belongs to
a package this companion deliberately does not link — the product binary
that wired it is the one process that can explain it:

  ./app explain PG0101`,
			},
			{
				name:  "fleet",
				short: "many products at once, from above",
				long: `The fleet layer: one team, many products, one command.

Every product is a toolbox binary with machine-readable answers (graph
--json, doctor, ultravet), so these subcommands need no special hooks —
they walk a directory for go.mod files requiring the framework and ask
each product about itself. State lives in .ultra-fleet.json at the root.`,
				subs: []*command{
					{
						name:  "status",
						args:  "[dir] [--save] [--json]",
						short: "versions, fingerprints and drift across the fleet",
						long: `One row per product: framework version, graph fingerprint, component
count, and drift against the saved baseline.

Exits 1 when any product drifted or failed to answer, so a scheduled job
can gate on it.`,
						flags: []flagDoc{
							{"--save", "record today's fingerprints as the baseline"},
							{"--json", "the table as one JSON array"},
						},
					},
					{
						name:  "vet",
						args:  "[dir]",
						short: "run the analyzer across every product",
						long: `Run ultravet over every discovered product and report per product.

Needs an installed ultravet — a fleet sweep shells out once per repo, and
` + "`go run @latest`" + ` per repo is a different command's patience.`,
					},
					{
						name:  "bump",
						args:  "[dir] --to vX.Y.Z [--push] [--pr] [--full]",
						short: "move products onto a release, on a branch",
						long: `The many-products form of ultra upgrade, driven from above.

Every product behind the target gets a branch, the pins moved, and a
build plus wiring test as verification; a failure reverts that product and
leaves the rest alone. Products already current, dirty, or on a local
replace are reported and skipped.`,
						flags: []flagDoc{
							{"--to vX.Y.Z", "the target framework version (required)"},
							{"--push", "push the branch"},
							{"--pr", "open a pull request (implies --push)"},
							{"--full", "run the full test suite, not the wiring test"},
							{"--json", "the report as one JSON array"},
						},
					},
					{
						name:  "profiles",
						args:  "[dir] [--json]",
						short: "group products by the capability set they wire",
						long: `Descriptive discovery, not a prescription.

Products are grouped by the set of preset modules they wire — the blessed
capability combos the fleet already runs. It reports what the architecture
looks like today; it recommends nothing.`,
						flags: []flagDoc{{"--json", "the profiles as one JSON document"}},
					},
				},
			},
			{
				name:  "init",
				args:  "[dir] [--force <file>] [--dry]",
				short: "bring an EXISTING product up to the current doctrine",
				long: `Retrofit a product that already exists.

ultra new emits the doctrine files at birth and never again, so a product
scaffolded by an older release — or ported, or hand-made — silently lacks
whatever the scaffold learned since. init renders exactly the ones that
are ABSENT, from the same templates, with the capabilities read off the
tree (web/, internal/db, the wired presets, the design system in
web/package.json) rather than typed.

A file that exists is NEVER touched — it reports "kept (exists)" and
moves on — so the second run writes nothing. --force <file> regenerates
one named file and prints the diff before it does.`,
				flags: []flagDoc{
					{"--force <file>", "regenerate one file, diff first"},
					{"--dry", "show the plan and write nothing"},
				},
			},
			{
				name:  "mcp",
				short: "Model Context Protocol server over stdio",
				long: `Serve the graph intelligence as agent tools, over stdio.

  claude mcp add ultrastack -- ultra mcp

The tools are the same answers this CLI gives — brief, explain, codes,
vet, graph, blast, diff, fleet_status, report — so an agent and a human
read one registry. Every tool calls the command's own core; none of them
is a second implementation.`,
			},
			{
				name:  "new",
				args:  "<name> [flags]",
				short: "scaffold a product, or a feature inside one",
				long: `Scaffold a product on the paved road: main.go + internal/app +
config.toml + Taskfile + AGENTS.md.

--db --web --auth are ON by default; --bare turns all three off, and a
later --db/--web/--auth turns one back on.

--ds picks the frontend's design system (--web only). connected (the
default) wires @connected/svelte-connected-design from the depot registry, so
bun install needs depot auth. bare wires Tailwind v4 and an empty @theme:
the open foundation, for a product that diverges deliberately.

--from is REVERSE scaffolding, the legacy-service on-ramp: an OpenAPI 3.x
document in, a doctrine-shaped product out — a feature package per tag,
one api.Handle per path+method, request/response structs from the schemas,
and every handler a 501 stub (<feature>.<op>.not_implemented) so the
product boots and answers on arrival. It prints a migration report: what
came across, what was guessed, and — by path — what did not. JSON only;
convert YAML first.`,
				flags: []flagDoc{
					{"--module github.com/org/name", "the module path (default: the name)"},
					{"--version vX.Y.Z", "the framework version to require"},
					{"--bare", "no db, no web, no auth"},
					{"--db, --no-db", "Postgres pool + migrations (on by default)"},
					{"--web, --no-web", "the SvelteKit frontend (on by default)"},
					{"--auth, --no-auth", "identity + route enforcement (on by default)"},
					{"--ds connected|bare", "the design system for --web (default: connected)"},
					{"--from openapi.json", "reverse-scaffold from an OpenAPI 3.x document"},
				},
				subs: []*command{
					{
						name:  "feature",
						args:  "<name>",
						short: "one file at internal/app/<name>/<name>.go",
						long: `Write the single-file collapse form of a feature package: one file
exporting Module — its wiring, its Handler, and the fluent route table to
fill in — plus its errors.go.

Manifest files (handler.go, service.go, types.go) are NOT scaffolded —
they appear when content demands them, which is the doctrine's growth
rule. Run it from the product root; one line in app.go finishes the job.`,
					},
					{
						name:  "requirement",
						args:  "<id> \"<title>\" [dir]",
						short: "one requirement file at requirements/REQ-<id>.md",
						long: `Scaffold one requirement: requirements/REQ-<id>.md, status draft.

Frontmatter carries the joins (operations[] on governed operationIds,
tests[], e2e[], frame) and the body carries the SRS prose — Statement,
Rationale, Acceptance. Code never carries a requirement id; the
operationId is the join key, and ultra trace derives every status.

Writing the FIRST requirement is the opt-in act that activates trace for
this product (ultra compliance init scaffolds the full class-2 set).`,
					},
					{
						name:  "table",
						args:  `<name> "<columns>" [flags] [dir]`,
						short: "migration + queries + store, then sqlc generate",
						long: `Write the whole persistence slice for one table, in one command.

The migration (next goose number, id + your columns + created_at with NO
default — law 11 puts the row's time in di.Clock), the five canonical
queries (create, get, keyset list, update, delete :execrows), and a Store
skeleton in the feature. Then it runs sqlc generate, and the ultra sqlc
plugin adds the typed half: <Table>Error with a sentinel per unique
constraint, <Table>Cursor + List<Table>Page, and a row factory for tests.

The SQL is YOURS from the moment it lands — this command never runs over
it again. Only internal/db/gen regenerates.

--owned adds a subject column and threads ownership through the index and
every WHERE, so a caller can never read or delete another subject's rows.
--migration folds the DDL into an EXISTING migration instead of adding
one, which is how a parent and its child reach production together.

It refuses on a module that does not build, and on a table name any
migration or query file already mentions — naming the file.`,
						flags: []flagDoc{
							{"--owned", "every row belongs to a subject; ownership in every WHERE"},
							{"--feature <pkg>", "put the store in this existing feature (default: the table's name)"},
							{"--migration <n>", "append the DDL to migration n instead of adding one"},
							{"--no-store", "SQL only — write no Go"},
						},
					},
				},
			},
			{
				name:  "report",
				args:  "<kind> [--code X] [--pkg Y] <message...>",
				short: "file a field report where the fleet can read it",
				long: `File a field report: friction, bug, docs or idea.

Product work turns up framework problems constantly — an error that did
not name the consumer, a preset that refused a valid config, a doctrine
page that lied. Those findings are written today and lost today, because
there is nowhere they all land.

This is that place, and it is a FILE: one JSON line appended to
.ultra-reports.jsonl at the fleet root (walking up for .ultra-fleet.json;
the product root when there is no fleet, and the verdict says which). No
network, no server, no daemon — a human reads it and deletes what they
have acted on.

Product, framework version, directory and time are stamped for you; the
message is the only thing worth typing.`,
				flags: []flagDoc{
					{"--code DI0001", "the diagnostic code the finding is about"},
					{"--pkg contrib/pg", "the package the finding is about"},
				},
				subs: []*command{
					{
						name:  "list",
						args:  "[--kind K] [--since 14d] [--json]",
						short: "the inbox as a table, newest first",
						long: `Read the inbox: one row per report, newest first.

--since takes a Go duration (72h), a day count (14d), or a date
(2026-07-01). It reads the same file ` + "`ultra report`" + ` writes, found the
same way, so the two cannot disagree about where the inbox is.`,
						flags: []flagDoc{
							{"--kind friction", "only this kind (friction|bug|docs|idea)"},
							{"--since 14d", "only reports newer than this"},
							{"--json", "the inbox as one JSON array"},
						},
					},
				},
			},
			{
				name:  "records",
				short: "frozen release evidence: freeze, sign",
				long: `records/<version>/ — the evidence set one release freezes, and the
signature references a human enters into it.

Presence-activated like the whole compliance harness: a product without
requirements/ has no evidence to freeze, and these verbs refuse politely.
The frozen directory commits WITH the release, by a human.`,
				subs: []*command{
					{
						name:  "freeze",
						args:  "[version] [dir] [--force]",
						short: "render one release's evidence into records/<version>/",
						long: `Freeze the evidence of one release into records/<version>/ (WP.09/20/21/
22/23): the derived trace matrix (human + JSON), COPIES of the recorded
runs with their real mtimes and the staleness verdict trace computed — a
freeze of stale evidence says so, it never launders staleness fresh — the
requirements set as-of-now (statuses, approval stamps, open questions,
change-request dispositions), Figma frame exports for every requirement
that pins one (FIGMA_ACCESS_TOKEN; absent token or API = a stated note,
never a failed freeze), and RECORD.md with an UNSIGNED marker per signed
work product until ultra records sign enters each reference.

version defaults to the newest git tag. Re-freezing an existing version
REFUSES without --force: evidence rewriting must be loud. --force carries
already-entered signature references over. Nothing is committed — the
directory belongs in the release commit.`,
						flags: []flagDoc{{"--force", "replace an existing records/<version>/ (loudly)"}},
					},
					{
						name:  "sign",
						args:  "<wp> --ref <pointer> [--version vX.Y.Z] [--by <name>] [dir]",
						short: "enter a signature reference into a frozen record",
						long: `Record the POINTER to an externally signed artifact — agreement (WP.02),
uat (the UAT validation record) or acceptance (WP.01). The customer signs
in their own world (a PDF, an email, a portal); the frozen RECORD.md holds
the reference plus who entered it and when. A reference is entered once;
overwriting one is refused.

This is a human act: the MCP records_sign tool elicits the human or
answers pending — a tool caller can never supply the signature.
--version defaults to the latest frozen version.`,
						flags: []flagDoc{
							{"--ref <pointer>", "file / scan / message-id of the signed artifact (required)"},
							{"--version vX.Y.Z", "which frozen record (default: the latest)"},
							{"--by <name>", "who entered it (default: git config user.name)"},
						},
					},
				},
			},
			{
				name:  "req",
				short: "the requirements ledger: approve, ask, change",
				long: `The human acts on a requirement file, as verbs — so every act leaves the
stamp an auditor asks for. requirements/REQ-<id>.md is the record;
ultra trace derives every status from it.`,
				subs: []*command{
					{
						name:  "approve",
						args:  "<id> [dir] [--by <name>]",
						short: "draft → approved: the one human-declared transition",
						long: `Approve a requirement — the WP.22 validation record, as data.

This is the ONLY path to status: approved. It stamps who (git config
user.name, unless --by) and the date, and it REFUSES while the file holds
"- open:" clarifications: ask-don't-guess has teeth, and an approval over
unanswered questions is not a validation.

Agents never run this on a human's behalf. The MCP req_approve tool either
elicits the human directly or answers "pending human approval" — no human
signal, no approval.`,
						flags: []flagDoc{{"--by <name>", "the approver (default: git config user.name)"}},
					},
					{
						name:  "ask",
						args:  "<id> \"<question>\" [dir]",
						short: "park an open question instead of guessing",
						long: `Append an unanswered clarification to a requirement.

The question lands under ## Clarifications as "- open: …", and ultra trace
renders the requirement blocked-on-human until a human writes the answer
back as a dated Q&A. Approval is refused while any open item stands.

Batch your questions; ask once, and the answer written into the file is
answered forever.`,
					},
					{
						name:  "change",
						args:  "<id> --description \"...\" [flags] [dir]",
						short: "log a change request (WP.03); accepted drops to draft",
						long: `Append a change-request entry to a requirement's ## Changes log.

Every disposition is recorded — accepted, rejected, AND deferred: a
rejection is knowledge, and git history alone holds no diff for it. An
entry without a disposition is UNDECIDED and trace renders the requirement
blocked-on-human until a human decides.

disposition: accepted on an APPROVED requirement drops it back to draft
and clears the stamp — a materially changed requirement is one nobody has
approved yet (the re-approval law). Cite ultra breaking --against output
in --impact: generated evidence, not prose guesswork.`,
						flags: []flagDoc{
							{"--description \"...\"", "what is being asked for (required)"},
							{"--requested-by <who>", "who asked (default: git config user.name)"},
							{"--impact \"...\"", "the analysed impact — cite `ultra breaking --against`"},
							{"--disposition accepted|rejected|deferred", "the HUMAN decision; omit to record undecided"},
							{"--decided-by <who>", "who decided (default: git config user.name)"},
						},
					},
				},
			},
			{
				name:  "skill",
				short: "the vendored doctrine: generate and install",
				long: `The doctrine agents read — generated from this repo, vendored into a
product.`,
				subs: []*command{
					{
						name:  "gen",
						short: "regenerate references/ from the code",
						long: `Regenerate references/ — errors.md, and the go-doc and compiled-example
marker blocks inside the hand-written prose.

Run it from the repo root. The drift test gates it, so a doc block and the
symbol it documents cannot disagree.`,
					},
					{
						name:  "install",
						args:  "[--check] [--force]",
						short: "vendor SKILL.md + references/ into .claude/skills",
						long: `Vendor SKILL.md + references/ into .claude/skills/ultrastack at the
EXACT version go.mod pins (a replace wins: the live checkout).

Agents then read doctrine that matches the code in front of them rather
than whatever the model remembers. ultra upgrade refreshes it with the
bump.`,
						flags: []flagDoc{
							{"--check", "fail when the vendored copy is stale (for CI)"},
							{"--force", "re-vendor even when it is current"},
						},
					},
				},
			},
			{
				name:  "trace",
				args:  "[dir] [--json] [--list]",
				short: "requirements × contract × recorded runs, derived",
				long: `The traceability record (WP.21), DERIVED — never hand-kept.

It joins requirements/REQ-*.md against the committed openapi.json
(operationIds; x-error-codes for 501-stub detection, the same logic as
ultra brief's STUBS) and against the RECORDED runs task test / task e2e
tee into .ultra/test.json and .ultra/e2e.json. trace never runs tests and
never fakes freshness: each record's age is shown, and a record older than
the newest source renders the statuses that depend on it unknown — not
green.

Per requirement it derives: implemented (all operations live; stubs render
◐ n/m), verified (pinned tests present in the run and passed), validated
(pinned e2e titles passed + acceptance prose present). Open questions or
an undecided change request render blocked-on-human.

Three gates: a REQ naming a dead operationId is an ERROR (REQ0102); a
pinned test missing from the test list is an ERROR (REQ0103); an operation
no requirement claims is a WARNING (REQ0104) unless exempted in
requirements/exempt.txt. Errors exit 1.

PRESENCE-ACTIVATED: without a requirements/ directory this is a clean
no-op — the covenant products that never opted in rely on.`,
				flags: []flagDoc{
					{"--json", "the whole pack as one JSON document"},
					{"--list", "re-derive the test list via `go test -list` (compiles every test package)"},
				},
			},
			{
				name:  "upgrade",
				args:  "[dir] [--check] [--to vX.Y.Z] [--all] [--dry]",
				short: "move THIS product onto a framework release",
				long: `Move one product onto a framework release — latest unless --to.

Before the gate runs it prints the target's release notes (releases/
vX.Y.Z.md, shipped in the module) with the Behavior changes section
highlighted — a green gate proves your tests still pass, not that a
listed change didn't move what a test pins. Old tags shipped no notes;
that is one honest line, never an error.

It trues the ultrastack pins, then go mod tidy, build, test. A failure
restores go.mod/go.sum, so a bad release leaves nothing behind. Contract
drift is refreshed and reported, never committed. It never touches git.

--check only answers "am I behind?": pinned vs latest, nothing written,
exit 1 when a newer release exists — a CI-friendly staleness gate.

A whole workspace at once is a fleet operation: ultra fleet bump.`,
				flags: []flagDoc{
					{"--check", "compare the pins against the latest release; write nothing"},
					{"--to vX.Y.Z", "the target version (default: the latest release)"},
					{"--all", `also "go get -u ./..." every other dependency`},
					{"--dry", "show the plan and write nothing"},
				},
			},
			{
				name:  "version",
				short: "print the ultra, build and Go versions",
				long: `Print this binary's version.

Installed from a module, that is the module version go install resolved.
Built from a checkout, it is the VCS revision the build stamped, with
-dirty when the tree had uncommitted changes. Neither available (go run
from a tarball) reports devel. The Go toolchain version rides along,
because "which Go built it" is the second question every bug report asks.`,
			},
			{
				name:  "vet",
				args:  "[packages] [-fix]",
				short: "static wiring checks before anything runs",
				long: `Static wiring checks before anything runs: the DI0001-DI0106 graph
family and the UV0001-UV0006 lints, including the product-structure laws.

-fix applies the machine-safe edits the report marks [fixable]; the rest
stay advisory, because they need a decision. Patterns default to ./...,
so a bare ` + "`ultra vet -fix`" + ` means this whole module.

Exit codes are the analyzer's, every format: 1 with findings, 0 clean, 2
on a load error.`,
				flags: []flagDoc{
					{"-fix", "apply the edits marked [fixable]"},
					{"--json", "the findings as one JSON array"},
					{"--format github", "GitHub Actions annotations (auto on GITHUB_ACTIONS)"},
				},
			},
			{
				name:  "help",
				args:  "[command]",
				short: "help about any command",
				long:  `Print the help for any command in the tree.`,
			},
		},
	}
	link(root)
	return root
}

// link fills in parent pointers and sorts each listing, so rendering never
// depends on the order the tree literal happens to be written in.
func link(c *command) {
	sort.SliceStable(c.subs, func(i, j int) bool { return c.subs[i].name < c.subs[j].name })
	for _, s := range c.subs {
		s.parent = c
		link(s)
	}
}

// find resolves one child by name or alias.
func (c *command) find(name string) *command {
	for _, s := range c.subs {
		if s.name == name {
			return s
		}
		for _, a := range s.aliases {
			if a == name {
				return s
			}
		}
	}
	return nil
}

// path is the command as the reader types it: "ultra contrib add".
func (c *command) path() string {
	if c.parent == nil {
		return c.name
	}
	return c.parent.path() + " " + c.name
}

// usage is the Usage: line's body.
func (c *command) usage() string {
	switch {
	case c.args != "":
		return c.path() + " " + c.args
	case len(c.subs) > 0:
		return c.path() + " [command]"
	default:
		return c.path()
	}
}

// help renders one node's screen. The root gets its title line; every other
// node opens with its long description, cobra-style.
func (c *command) help(w io.Writer) {
	if c.parent == nil {
		fmt.Fprintf(w, "%s — %s\n\n", c.name, c.short)
	} else if desc := strings.TrimSpace(c.longOrShort()); desc != "" {
		fmt.Fprintf(w, "%s\n\n", desc)
	}

	fmt.Fprintf(w, "Usage:\n  %s\n", c.usage())
	if len(c.subs) > 0 && c.args != "" {
		// A node that both takes arguments and nests (ultra new) shows both
		// forms — the second is the one a reader would otherwise never find.
		fmt.Fprintf(w, "  %s [command]\n", c.path())
	}

	if len(c.subs) > 0 {
		fmt.Fprint(w, "\nAvailable Commands:\n")
		t := newHelpTable(w)
		for _, s := range c.subs {
			t.row("  "+s.name, s.short)
		}
		t.flush()
	}

	fmt.Fprint(w, "\nFlags:\n")
	t := newHelpTable(w)
	for _, f := range c.flags {
		t.row("  "+f.spec, f.desc)
	}
	t.row("  "+helpFlag.spec, fmt.Sprintf(helpFlag.desc, c.name))
	t.flush()

	if len(c.subs) > 0 {
		fmt.Fprintf(w, "\nUse \"%s [command] --help\" for more information about a command.\n", c.path())
	}
}

// unknown renders the did-you-mean screen for a name this node does not have,
// and is the only place that error is worded.
func (c *command) unknown(w io.Writer, typo string) {
	fmt.Fprintf(w, "Error: unknown command %q for %q\n", typo, c.path())
	if s := c.nearest(typo); s != "" {
		fmt.Fprintf(w, "\nDid you mean %q?\n", s)
	} else if hit := c.rootward(typo); hit != nil {
		// Not a typo — a real verb, filed one level too deep. `ultra contrib
		// upgrade` is a reasonable guess (presets version in lockstep, so the
		// top-level command IS the per-preset one); point at the right level
		// instead of shrugging.
		fmt.Fprintf(w, "\nDid you mean %q?\n", hit.path())
	}
	fmt.Fprintf(w, "\nRun '%s --help' for usage.\n", c.path())
}

// rootward resolves a name against the ancestors' children — the command the
// reader meant when they nested a real verb under the wrong parent.
func (c *command) rootward(name string) *command {
	for p := c.parent; p != nil; p = p.parent {
		if hit := p.find(name); hit != nil {
			return hit
		}
	}
	return nil
}

// nearest is the closest child name within an edit distance of 2 — close
// enough to be a typo, far enough that a different word is not "corrected"
// into one the reader never meant.
func (c *command) nearest(typo string) string {
	best, bestD := "", 3
	for _, s := range c.subs {
		for _, name := range append([]string{s.name}, s.aliases...) {
			if d := editDistance(strings.ToLower(typo), name); d < bestD {
				best, bestD = s.name, d
			}
		}
	}
	return best
}

// editDistance is the classic Levenshtein distance, on bytes: command names
// are ASCII, and a typo is by definition not multi-byte.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = minOf(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func minOf(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func (c *command) longOrShort() string {
	if c.long != "" {
		return c.long
	}
	return c.short
}

// wantsHelp reports whether -h/--help appears in args, stopping at a bare
// "--": everything past that belongs to the child process (`ultra dev -- -h`
// is the served binary's flag, not ours).
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}
