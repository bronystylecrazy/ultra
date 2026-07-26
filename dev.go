package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

	// devDebounce coalesces a save burst — "format on save" is several writes,
	// and a multi-file refactor is a wave of them.
	devDebounce = 200 * time.Millisecond

	// devStopGrace is how long a SIGTERMed backend gets to run its
	// reverse-order Stop before the group is killed.
	devStopGrace = 10 * time.Second
)

const devUsage = "usage: ultra dev [--no-web] [--no-infra] [--no-pty] [--build-flags \"...\"] [-- <serve args>]"

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
				fmt.Fprintln(errW, "ultra dev: --build-flags needs a value (e.g. --build-flags \"-race\")")
				return 2
			}
			opts.buildFlags = strings.Fields(args[i+1])
			i++
		case strings.HasPrefix(a, "--build-flags="):
			opts.buildFlags = strings.Fields(strings.TrimPrefix(a, "--build-flags="))
		default:
			fmt.Fprintf(errW, "ultra dev: unknown argument %q\n%s\n", a, devUsage)
			return 2
		}
	}

	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(errW, err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		fmt.Fprintf(errW, "ultra dev: no go.mod in %s — run it from inside a product.\n", root)
		return 2
	}
	return newDevLoop(root, opts, out, errW).run()
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
			d.cycle(files)
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
		env:  d.childEnv(),
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
	d.say(d.out, "·", "dev infra — docker compose -f %s up -d --wait", devComposeFile)
	cmd := exec.Command("docker", "compose", "-f", devComposeFile, "up", "-d", "--wait")
	cmd.Dir = d.root
	cmd.Env = append(os.Environ(), d.childEnv()...)
	cmd.Stdout, cmd.Stderr = d.prefixer("[infra]", d.out), d.prefixer("[infra]", d.errW)
	if err := cmd.Run(); err != nil {
		d.say(d.errW, "✗", "dev infra failed — %v (the loop continues; the API may not connect)", err)
	}
}

// startWeb spawns the frontend dev server beside the API. bun when it is on
// PATH (the paved road), npm otherwise. No web/package.json means no
// frontend, and that is silent — a --bare product is not missing anything.
func (d *devLoop) startWeb() {
	webDir := filepath.Join(d.root, "web")
	if _, err := os.Stat(filepath.Join(webDir, "package.json")); err != nil {
		return
	}
	name, args := "npm", []string{"run", "dev"}
	if _, err := exec.LookPath("bun"); err == nil {
		name, args = "bun", []string{"dev"}
	}
	if _, err := exec.LookPath(name); err != nil {
		d.say(d.out, "·", "[web] skipped — neither bun nor npm is on PATH")
		return
	}
	p, err := startManaged(procSpec{
		dir:  webDir,
		bin:  name,
		args: args,
		env:  d.childEnv(),
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
