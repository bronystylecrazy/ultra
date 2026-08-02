package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bronystylecrazy/ultrastack/di/diag"
)

// `ultra dev` is the whole inner loop in ONE terminal.
//
// It boots the dev infrastructure, builds and serves the API, watches the Go
// tree, and on every change rebuilds, restarts the backend GRACEFULLY, and
// regenerates the committed contracts — while the frontend dev server runs
// beside it under the same Ctrl-C.
//
//	● serving pid 4123 · built 320ms · contracts unchanged
//	↻ restarting (3 files changed)
//	● serving pid 4171 · built 341ms · contracts refreshed (3 files)
//	✗ build failed — still serving previous binary
//
// Three decisions carry the whole design:
//
//   - A RED BUILD NEVER KILLS THE SERVER. The compiler output is printed and
//     the previous binary keeps serving. A dev loop that takes the app down on
//     a typo trains you to fear saving; one that keeps serving lets you keep
//     the browser open and read the error.
//   - THE CONTRACTS ARE PART OF THE RESTART. openapi.json and the typed client
//     are regenerated from the binary that was just built, changed bytes only.
//     Change a Go response struct and your editor underlines the frontend that
//     broke — before you reach the browser.
//   - THE GRACEFUL PATH IS THE DEFAULT PATH. Restart means SIGTERM to the
//     child's process GROUP, the platform's reverse-order Stop, and SIGKILL
//     only after the grace window. Restarting a hundred times a day is exactly
//     how a shutdown bug gets found early instead of in production.
//   - THE CHILDREN GET A REAL TERMINAL. On a unix, with ultra's own stdout on
//     a tty, every supervised child runs on a pseudo-terminal: vite draws its
//     box, the API picks its console log format and its coloured diagnostic
//     renderer, and line buffering means a crash's last words arrive. NO_COLOR
//     or --no-pty falls back to pipes, and so does Windows.

const (
	// devBuildDir is the loop's scratch: the binary it builds and restarts.
	// It is dot-prefixed so the watcher's dot-segment rule excludes it for
	// free — the output of a build must never retrigger the build.
	devBuildDir = ".ultradev"

	// devComposeFile is the dev infrastructure, booted once before the first
	// start and never again on a rebuild.
	devComposeFile = "docker-compose.dev.yml"

	// devComposeOverride is the hand-owned half beside it. Compose merges a
	// file of this name automatically — but ONLY when it was left to find the
	// files itself, and this loop passes -f. See devComposeArgs.
	devComposeOverride = "docker-compose.override.yml"

	// devDebounce coalesces a save burst — "format on save" is several writes,
	// and a multi-file refactor is a wave of them.
	devDebounce = 200 * time.Millisecond

	// devStopGrace is how long a SIGTERMed backend gets to run its
	// reverse-order Stop before the group is killed.
	devStopGrace = 10 * time.Second

	// devLoopTrip rebuilds inside devLoopWindow, every one triggered by the
	// SAME path set, is a loop feeding itself — no editor and no agent saves
	// one file five times in ten seconds and nothing else. The go.sum
	// restart-loop incident burned an afternoon because the loop could not
	// say what kept retriggering it; the breaker exists so the NEXT cause
	// names itself (error[DEV0101]) instead.
	devLoopTrip   = 5
	devLoopWindow = 10 * time.Second
)

type devOptions struct {
	web        bool
	infra      bool
	noPTY      bool
	buildFlags []string
	serveArgs  []string
}

func cmdDev(args []string, out, errW io.Writer) int {
	opts := devOptions{web: true, infra: true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			opts.serveArgs = append(opts.serveArgs, args[i+1:]...)
			i = len(args)
		case a == "--no-web":
			opts.web = false
		case a == "--no-infra":
			opts.infra = false
		case a == "--no-pty":
			opts.noPTY = true
		case a == "--build-flags":
			if i+1 >= len(args) {
				fmt.Fprintln(errW, "Error: --build-flags needs a value (e.g. --build-flags \"-race\")")
				fmt.Fprintln(errW, "\nRun 'ultra dev --help' for usage.")
				return 2
			}
			opts.buildFlags = strings.Fields(args[i+1])
			i++
		case strings.HasPrefix(a, "--build-flags="):
			opts.buildFlags = strings.Fields(strings.TrimPrefix(a, "--build-flags="))
		default:
			node := ultraTree().find("dev")
			fmt.Fprintf(errW, "Error: unknown argument %q for %q\n\n", a, node.path())
			node.help(errW)
			return 2
		}
	}

	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "dev", err.Error())
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		fmt.Fprintf(errW, "ultra dev: no go.mod in %s — run it from inside a product.\n", root)
		failVerdict(errW, "dev", "not a product directory")
		return 2
	}
	code := newDevLoop(root, opts, out, errW).run()
	// The loop only ever returns because it stopped; say which way it went.
	// A dev session that exits on a failed watcher used to leave nothing but
	// the error, and the reader could not tell a crash from a clean Ctrl-C.
	verdictFor(errW, code == 0, "dev", map[bool]string{true: "stopped cleanly", false: "stopped on an error"}[code == 0])
	return code
}

// devLoop is the supervisor. Everything it owns is stopped by the same
// Ctrl-C: the API, the frontend, the watcher.
type devLoop struct {
	root string
	opts devOptions
	out  io.Writer
	errW io.Writer

	debounce time.Duration
	grace    time.Duration
	prefixer func(string, io.Writer) io.Writer

	// color paints OUR banners; errColor paints the ones that go to stderr,
	// which is a separate file and may separately be redirected.
	color    palette
	errColor palette

	// pty is the whole terminal decision for the CHILDREN, resolved once:
	// ultra's own stdout is a terminal, the user did not say NO_COLOR, and
	// did not pass --no-pty. Tests set it directly — there is no terminal in
	// CI to detect, and forcing the field is the seam.
	pty bool

	api   *managedProc
	web   *managedProc
	exits chan *managedProc
	guard devGuard
}

// devGuard is the loop-breaker: the last line of defence when, despite the
// exclusions and the fingerprint gate, something still manages to change a
// watched build input on every cycle — a boot-time generator emitting
// nondeterministic bytes into the tree is the canonical way. Without it, that
// bug presents as a silent ~600ms rebuild loop and costs whoever hits it the
// afternoon of instrumentation it cost us; with it, the loop stops itself and
// names the path.
type devGuard struct {
	sig     string // the batch's path set, joined — identity across cycles
	times   []time.Time
	last    time.Time
	tripped bool

	// trip and window default to the devLoop* constants; tests set them
	// directly — a loop breaker cannot be asserted against the wall clock
	// a CI machine actually has.
	trip   int
	window time.Duration
}

type guardVerdict int

const (
	guardBuild  guardVerdict = iota
	guardTrip                // the batch that crossed the line: diagnose, do not build
	guardIgnore              // still looping on the same paths: already diagnosed
)

// observe decides what one batch means. Any change of path set resets the
// count — a human's edits vary, a self-trigger repeats exactly. A tripped
// guard stays tripped only while the same set keeps arriving inside the
// window: the self-trigger fires per cycle, so once building stops it goes
// quiet, and a genuinely new save of the same file later builds again.
// limits are the effective thresholds: the constants unless a test overrode.
func (g *devGuard) limits() (trip int, window time.Duration) {
	trip, window = g.trip, g.window
	if trip == 0 {
		trip = devLoopTrip
	}
	if window == 0 {
		window = devLoopWindow
	}
	return trip, window
}

func (g *devGuard) observe(files []string, now time.Time) guardVerdict {
	trip, window := g.limits()
	sig := strings.Join(files, "\x00")
	quiet := now.Sub(g.last) > window
	g.last = now
	if sig != g.sig || (g.tripped && quiet) {
		g.sig, g.times, g.tripped = sig, nil, false
	}
	if g.tripped {
		return guardIgnore
	}
	g.times = append(g.times, now)
	for len(g.times) > 0 && now.Sub(g.times[0]) > window {
		g.times = g.times[1:]
	}
	if len(g.times) >= trip {
		g.tripped = true
		return guardTrip
	}
	return guardBuild
}

func newDevLoop(root string, opts devOptions, out, errW io.Writer) *devLoop {
	// A terminal, honestly: not colorFor, which CLICOLOR_FORCE can turn on
	// over a pipe. Forcing colour is a statement about bytes; a pty is a
	// statement about the device, and handing one to a child whose output
	// goes to a file helps nobody.
	f, isFile := out.(*os.File)
	_, noColor := os.LookupEnv("NO_COLOR")
	tty := isFile && isTerminal(f) && !noColor

	return &devLoop{
		root: root, opts: opts, out: out, errW: errW,
		debounce: devDebounce,
		grace:    devStopGrace,
		prefixer: newPrefixer(),
		color:    colorFor(out),
		errColor: colorFor(errW),
		pty:      tty && !opts.noPTY,
		exits:    make(chan *managedProc, 4),
	}
}

// say prints one status banner to w — glyph coloured, text plain. The palette
// follows the writer, because stdout and stderr are not the same file and one
// of them may be a log.
func (d *devLoop) say(w io.Writer, glyph, format string, a ...any) {
	p := d.color
	if w == d.errW {
		p = d.errColor
	}
	fmt.Fprintln(w, p.banner(glyph, fmt.Sprintf(format, a...)))
}

// childEnv is the colour contract with the supervised children, and it holds
// in BOTH modes. vite, bun and everything chalk-based read FORCE_COLOR /
// CLICOLOR_FORCE before they read isatty, so they keep their colour even on
// the pipe path (Windows, --no-pty); with a pty it is belt and braces.
// TERM is inherited, and only defaulted when the environment has none —
// a child on a terminal with no TERM assumes the dumbest one there is.
//
// NO_COLOR wins over all of it, and is passed THROUGH rather than dropped:
// the user said no, and the children must hear the same no we did.
func (d *devLoop) childEnv() []string {
	if v, ok := os.LookupEnv("NO_COLOR"); ok {
		return []string{"NO_COLOR=" + v}
	}
	if !d.pty && !bool(d.color) {
		return nil
	}
	env := []string{"FORCE_COLOR=1", "CLICOLOR_FORCE=1"}
	if d.pty && os.Getenv("TERM") == "" {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

// apiEnv is childEnv plus the dev-spans handshake: the otel preset's span
// printer defaults ON under this loop (ULTRA_DEV_SPANS=1) and off everywhere
// else; [otel] dev_print stays the product's override in both directions.
func (d *devLoop) apiEnv() []string {
	return append(d.childEnv(), "ULTRA_DEV_SPANS=1")
}

// webEnv is childEnv plus the API's resolved origin — the frontend is the one
// child that has to reach the other one.
func (d *devLoop) webEnv() []string {
	env := d.childEnv()
	if origin := devAPIOrigin(d.root); origin != "" {
		env = append(env, "ULTRA_DEV_API="+origin)
	}
	return env
}

func (d *devLoop) run() int {
	if err := d.prepareBuildDir(); err != nil {
		fmt.Fprintln(d.errW, err)
		return 1
	}
	if d.opts.infra {
		d.bootInfra()
	}

	// The watcher starts BEFORE the first build, so an edit made while the
	// first build runs is queued rather than lost.
	w, err := newDevWatcher(d.root)
	if err != nil {
		fmt.Fprintf(d.errW, "ultra dev: cannot watch %s — %v\n", d.root, err)
		return 1
	}
	defer w.Close()

	changes := make(chan string, 512)
	batches := make(chan []string)
	stop := make(chan struct{})
	go w.pump(changes, d.errW)
	go devBatch(changes, batches, d.debounce, stop)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	d.cycle(nil)
	if d.opts.web {
		d.startWeb()
	}

	for {
		select {
		case files := <-batches:
			d.onBatch(files)
		case p := <-d.exits:
			d.reportExit(p)
		case <-sigs:
			fmt.Fprintln(d.out)
			d.say(d.out, "·", "stopping")
			close(stop)
			d.stopAll()
			return 0
		}
	}
}

// onBatch is one batch through the loop-breaker: build it, or — when the same
// path set has retriggered devLoopTrip rebuilds inside devLoopWindow — refuse,
// say why once, and keep serving. The previous binary stays up either way.
func (d *devLoop) onBatch(files []string) {
	switch d.guard.observe(files, time.Now()) {
	case guardTrip:
		trip, window := d.guard.limits()
		d.say(d.errW, "✗", "error[DEV0101]: restart loop broken — %s retriggered %d rebuilds in %s with no other change",
			strings.Join(files, ", "), trip, window)
		fmt.Fprintf(d.errW, "  something in the cycle itself is rewriting that path — a boot-time generator with nondeterministic output is the usual culprit\n"+
			"  still serving pid %d; a change to any other file resumes rebuilds, and so does this path going quiet\n"+
			"  more: ultra explain DEV0101\n", d.api.pid())
	case guardIgnore:
		// Diagnosed one batch ago; repeating it every 600ms is the loop's noise.
	default:
		d.cycle(files)
	}
}

// prepareBuildDir creates the scratch directory and makes it self-ignoring,
// so no product's .gitignore has to learn about ultra dev.
func (d *devLoop) prepareBuildDir() error {
	dir := filepath.Join(d.root, devBuildDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, err := writeIfChanged(filepath.Join(dir, ".gitignore"), []byte("*\n"))
	return err
}

func (d *devLoop) binPath() string { return filepath.Join(d.root, devBuildDir, "app") }

// cycle is one pass of the loop: build, restart, refresh contracts, report.
//
// The restart happens BEFORE the contract refresh on purpose — the API is
// back up in the time the build took, and the generators run against a binary
// that has already proved it boots.
func (d *devLoop) cycle(files []string) {
	if len(files) > 0 {
		d.say(d.out, "↻", "restarting (%s changed)", devPlural(len(files), "file"))
	}

	start := time.Now()
	buildOut, err := d.build()
	if err != nil {
		if trimmed := strings.TrimRight(buildOut, "\n"); trimmed != "" {
			fmt.Fprintln(d.errW, trimmed)
		}
		if d.api.alive() {
			d.say(d.out, "✗", "build failed — still serving previous binary")
		} else {
			d.say(d.out, "✗", "build failed — nothing is serving yet")
		}
		return
	}
	built := time.Since(start)

	if err := d.restartAPI(); err != nil {
		d.say(d.errW, "✗", "cannot start %s — %v", devBuildDir+"/app", err)
		return
	}

	line := fmt.Sprintf("serving pid %d · built %s", d.api.pid(), built.Round(time.Millisecond))
	if contracts := d.refreshContracts(d.binPath()); contracts != "" {
		line += " · " + contracts
	}
	d.say(d.out, "●", "%s", line)
}

// build compiles the product into the scratch directory and returns the
// compiler's combined output — which IS the error report on failure.
func (d *devLoop) build() (string, error) {
	args := append([]string{"build"}, d.opts.buildFlags...)
	args = append(args, "-o", filepath.Join(devBuildDir, "app"), ".")
	cmd := exec.Command("go", args...)
	cmd.Dir = d.root
	var b strings.Builder
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return b.String(), err
}

func (d *devLoop) restartAPI() error {
	d.api.stop(d.grace)
	p, err := startManaged(procSpec{
		dir:  d.root,
		bin:  d.binPath(),
		args: d.opts.serveArgs,
		env:  d.apiEnv(),
		pty:  d.pty,
	}, d.prefixer("[api]", d.out), d.prefixer("[api]", d.errW), d.exits)
	if err != nil {
		return err
	}
	d.api = p
	return nil
}

// bootInfra brings the dev dependencies up once, before the first start.
// `--wait` is the point: the first boot of the API must not race a Postgres
// that is still initializing. A missing compose file or a missing docker is
// a one-line skip, not an error — plenty of products need neither.
func (d *devLoop) bootInfra() {
	if _, err := os.Stat(filepath.Join(d.root, devComposeFile)); err != nil {
		d.say(d.out, "·", "dev infra skipped — no %s", devComposeFile)
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		d.say(d.out, "·", "dev infra skipped — %s is here but docker is not on PATH", devComposeFile)
		return
	}
	args := devComposeArgs(d.root)
	d.say(d.out, "·", "dev infra — docker %s", strings.Join(args, " "))
	cmd := exec.Command("docker", args...)
	cmd.Dir = d.root
	cmd.Env = append(os.Environ(), d.childEnv()...)
	cmd.Stdout, cmd.Stderr = d.prefixer("[infra]", d.out), d.prefixer("[infra]", d.errW)
	if err := cmd.Run(); err != nil {
		d.say(d.errW, "✗", "dev infra failed — %v (the loop continues; the API may not connect)", err)
	}
}

// devComposeArgs is the docker invocation for this root.
//
// The override file is passed EXPLICITLY, and that is the whole point: Compose
// auto-merges docker-compose.override.yml only while it is choosing the files
// itself, and one -f turns the whole search off. Since this loop always passes
// -f for the generated file, a product's override — the escape hatch devinfra's
// own diagnostics tell you to write, `ports: ["55432:5432"]` and friends —
// silently did nothing under `ultra dev`, which is worse than not having one.
func devComposeArgs(root string) []string {
	args := []string{"compose", "-f", devComposeFile}
	if _, err := os.Stat(filepath.Join(root, devComposeOverride)); err == nil {
		args = append(args, "-f", devComposeOverride)
	}
	return append(args, "up", "-d", "--wait")
}

// devAPIOrigin resolves the origin the API is about to serve on, so the
// frontend dev server can proxy to it. It walks the same ladder the product
// does, minus the rung no outsider can see (a product's own di.Supply):
// ULTRA_HTTP_ADDR — which the child inherits from us — over [http] addr, over
// the ":8080" default. An addr that is not host:port is left to the product to
// complain about; "" means we have nothing honest to export.
//
// A vite proxy that hardcodes 8080 answers every API call with a 404 the
// moment the addr moves, and the 404 names nothing.
func devAPIOrigin(root string) string {
	addr := os.Getenv("ULTRA_HTTP_ADDR")
	if addr == "" {
		addr = devConfigAddr(filepath.Join(root, "config.toml"))
	}
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	// A wildcard bind is where the server listens, never an address to dial.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// devConfigAddr reads [http] addr out of a product's config.toml. A scanner
// rather than a TOML dependency for one key in one table — cmd/ultra buys a
// dependency only for what it cannot do plainly. A file it cannot make sense
// of yields "", and the caller falls back to the default the product would.
func devConfigAddr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[]")
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || section != "http" || strings.TrimSpace(key) != "addr" {
			continue
		}
		// The value is a quoted string trailed by whatever comment the
		// scaffold wrote; the closing quote ends it.
		val = strings.TrimSpace(val)
		if len(val) > 1 && (val[0] == '"' || val[0] == '\'') {
			if end := strings.IndexByte(val[1:], val[0]); end >= 0 {
				return val[1 : 1+end]
			}
		}
		return ""
	}
	return ""
}

// startWeb spawns the frontend dev server beside the API. bun is the whole
// toolchain — there is no second runner to fall back to. No web/package.json
// means no frontend, and that is silent — a --bare product is not missing
// anything.
//
// The child is told where the API actually is (ULTRA_DEV_API), because the
// vite proxy target is not a constant of the universe — see devAPIOrigin.
func (d *devLoop) startWeb() {
	webDir := filepath.Join(d.root, "web")
	if _, err := os.Stat(filepath.Join(webDir, "package.json")); err != nil {
		return
	}
	name, args := "bun", []string{"dev"}
	if _, err := exec.LookPath(name); err != nil {
		d.say(d.out, "·", "[web] skipped — bun is not on PATH")
		return
	}
	p, err := startManaged(procSpec{
		dir:  webDir,
		bin:  name,
		args: args,
		env:  d.webEnv(),
		pty:  d.pty,
	}, d.prefixer("[web]", d.out), d.prefixer("[web]", d.errW), d.exits)
	if err != nil {
		d.say(d.errW, "✗", "[web] cannot start %s — %v", name, err)
		return
	}
	d.web = p
	d.say(d.out, "●", "[web] %s %s (pid %d)", name, strings.Join(args, " "), p.pid())
}

// reportExit turns a child dying on its own into one line. A stop WE asked
// for is not news; a crash is, and the loop stays up to rebuild on the fix.
func (d *devLoop) reportExit(p *managedProc) {
	if p == nil || p.expected {
		return
	}
	switch p {
	case d.api:
		d.say(d.errW, "✗", "api exited (%s) — the loop is still watching", p.exitReason())
	case d.web:
		d.say(d.errW, "✗", "[web] dev server exited (%s)", p.exitReason())
	}
}

// stopAll unwinds in reverse start order: the frontend first (it proxies to
// the API), then the API's own graceful stop.
func (d *devLoop) stopAll() {
	d.web.stop(d.grace)
	d.api.stop(d.grace)
}

// DEV0101 is the first code in the dev family: cmd/ultra's own loop, not the
// kernel's and not a preset's. Registered here so `ultra explain DEV0101`
// answers in the binary that printed it.
func init() {
	diag.Register("DEV0101", `DEV0101 — ultra dev broke a self-sustained restart loop

The same path set triggered five rebuilds inside ten seconds with nothing
else changing. No editor works like that; a loop feeding itself does. The
watcher already refuses ultra dev's own exhaust (openapi.json, the generated
client, .ultradev/) and attribute-only churn (macOS stamping an xattr on
go.sum during the loop's own build — the incident that bought this code), so
what remains is almost always the product: something that runs EVERY cycle —
a boot-time generator, a go:generate step wired into the build, a tool the
app execs at startup — writing a watched .go file, go.mod or go.sum with
bytes that differ run to run.

Find it: the diagnostic names the path. Ask what writes that path, then make
the writer deterministic (sorted maps, stable timestamps) or write only when
the bytes actually changed — writeIfChanged is the house pattern. Output that
is honestly derived belongs in an ignored location instead (.ultradev/, a
dot-directory, web/).

The loop is not down: the last good binary keeps serving, saving any other
file resumes rebuilds, and the named path resumes too once it stops
retriggering for ten seconds.`)
}
