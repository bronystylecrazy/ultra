package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---- the exclusion rules ----

// The watcher's predicate is the loop's safety interlock: two of the paths it
// must ignore are files ultra dev itself writes, and watching your own output
// is an infinite rebuild.
func TestDevShouldTrigger(t *testing.T) {
	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"main.go", true, "the root package is the binary"},
		{"internal/app/app.go", true, "a feature file"},
		{"internal/db/gen/queries.sql.go", true, "generated Go is still Go the compiler reads"},
		{"go.mod", true, "a dependency change rebuilds"},
		{"go.sum", true, "so does a checksum change"},

		{"openapi.json", false, "OUR output — watching it is an infinite loop"},
		{"web/src/lib/api/index.ts", false, "the generated client is OUR output too"},
		{"web/src/routes/+page.svelte", false, "vite owns the frontend"},
		{"web/src/lib/api/orders.ts", false, "same client dir, any file"},
		{".ultradev/app", false, "the binary we just built"},
		{".git/index", false, "git churns constantly"},
		{".git/HEAD.lock", false, "and noisily"},
		{"web/node_modules/x/index.js", false, "never source"},
		{"node_modules/pkg/main.go", false, "a vendored .go under node_modules is not ours"},
		{".svelte-kit/generated/root.js", false, "any dot directory"},
		{".#main.go", false, "an emacs lock file looks exactly like a save"},

		{"config.toml", false, "live reload covers the reloadable sections"},
		{"README.md", false, "not a compilation input"},
		{"Taskfile.yml", false, "nor this"},
		{"internal/db/migrations/00001_init.sql", false, "nor SQL"},
	}
	for _, c := range cases {
		if got := devShouldTrigger(c.path); got != c.want {
			t.Errorf("devShouldTrigger(%q) = %v, want %v — %s", c.path, got, c.want, c.why)
		}
	}
}

// watchTree materialises a fixture tree: directories from dirs, and a file per
// entry of files (path → contents are irrelevant, only the name is read).
func watchTree(t *testing.T, dirs, files []string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func watchedRels(t *testing.T, root string, dirs []string) []string {
	t.Helper()
	var rels []string
	for _, d := range dirs {
		rel, err := filepath.Rel(root, d)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	return rels
}

// devWatchDirs must not descend into the excluded trees at all: registering a
// watch per node_modules directory is how a dev loop runs out of file
// descriptors — and on macOS, where kqueue costs one descriptor per FILE, it
// is how a dev loop runs a core hot for nothing.
func TestDevWatchDirsSkipsExcludedTrees(t *testing.T) {
	root := watchTree(t,
		[]string{"web/src/lib/api", "web/node_modules/svelte", ".git/objects", ".ultradev"},
		[]string{
			"main.go", "go.mod",
			"internal/app/orders/orders.go",
			"internal/db/gen/queries.sql.go",
			// Everything below is inside an excluded tree and must not be
			// walked, however Go-looking it is.
			"web/src/lib/api/index.ts",
			"web/node_modules/svelte/main.go",
			".git/objects/pack.go",
			".ultradev/app.go",
			"vendor/github.com/x/y/y.go",
			"internal/app/orders/testdata/golden.go",
		})
	want := []string{".", "internal", "internal/app", "internal/app/orders", "internal/db", "internal/db/gen"}
	dirs, _ := devWatchDirs(root)
	if got := watchedRels(t, root, dirs); !reflect.DeepEqual(got, want) {
		t.Fatalf("watched dirs = %v, want %v", got, want)
	}
}

// The second prune, and the expensive one: a directory with no Go input
// anywhere below it delivers only events this loop throws away. Watching it
// buys nothing and costs a descriptor per file plus a wakeup per write.
func TestDevWatchDirsSkipsTreesWithNoGoInThem(t *testing.T) {
	root := watchTree(t,
		[]string{"docs/adr", "dist/assets", "internal/db/migrations"},
		[]string{
			"main.go", "go.mod", "go.sum",
			"internal/app/orders/orders.go",
			// No Go anywhere below these — prose, output, and SQL.
			"docs/README.md", "docs/adr/0001.md",
			"dist/assets/app.css",
			"internal/db/migrations/00001_init.sql",
			// …but a Go file deep in an otherwise Go-free tree keeps the whole
			// ancestor chain, or the package it belongs to goes unwatched.
			"tools/gen/deep/gen.go",
			"tools/notes.txt",
		})
	want := []string{
		".",
		"internal", "internal/app", "internal/app/orders",
		"tools", "tools/gen", "tools/gen/deep",
	}
	dirs, pruned := devWatchDirs(root)
	if got := watchedRels(t, root, dirs); !reflect.DeepEqual(got, want) {
		t.Fatalf("watched dirs = %v, want %v", got, want)
	}
	// And what was dropped is reported as the TOP of each dropped subtree —
	// `internal/db`, not `internal/db/migrations` — because that is the list
	// the rescan walks, and walking a directory twice is walking it twice.
	wantPruned := []string{"dist", "docs", "internal/db"}
	if got := watchedRels(t, root, pruned); !reflect.DeepEqual(got, wantPruned) {
		t.Fatalf("pruned roots = %v, want %v", got, wantPruned)
	}
}

// A big frontend tree beside a small Go one is the shape that actually ships,
// and the watch set must be a function of the Go tree alone.
func TestDevWatchDirsIgnoresTheSizeOfTheFrontend(t *testing.T) {
	var dirs, files []string
	for i := range 300 {
		d := fmt.Sprintf("web/node_modules/pkg%d/dist", i)
		dirs = append(dirs, d)
		files = append(files, d+"/bundle.js")
	}
	for i := range 50 {
		files = append(files, fmt.Sprintf(".svelte-kit/generated/client/n%d.js", i))
	}
	files = append(files, "main.go", "go.mod", "internal/app/app.go")
	root := watchTree(t, dirs, files)

	watched, _ := devWatchDirs(root)
	got := watchedRels(t, root, watched)
	want := []string{".", "internal", "internal/app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watched dirs = %v, want %v — the frontend leaked into the watch set", got, want)
	}
}

// The content prune must NOT apply to a directory that appears while the loop
// runs. `ultra new feature billing` creates the directory a moment before
// billing.go lands in it, and a watcher that waited for the Go file would miss
// the feature's every save until a restart.
func TestDevNewWatchDirsWatchesAnEmptyNewPackage(t *testing.T) {
	root := watchTree(t,
		[]string{"internal/app/billing/sub", "internal/app/billing/node_modules"},
		nil)
	got := watchedRels(t, root, devNewWatchDirs(root))
	want := []string{".", "internal", "internal/app", "internal/app/billing", "internal/app/billing/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new-directory watch set = %v, want %v", got, want)
	}
	// And the startup set, on the same tree, watches only the root: nothing
	// here can rebuild anything yet.
	dirs, _ := devWatchDirs(root)
	if got := watchedRels(t, root, dirs); !reflect.DeepEqual(got, []string{"."}) {
		t.Fatalf("startup watch set = %v, want [.]", got)
	}
}

// ---- the rescan that closes the content prune's blind spot ----

// startRescanWatcher wires a watcher whose rescan is driven by the returned
// channel instead of the clock, pumps it, and returns the change stream. tick
// is unbuffered, so a send that COMPLETES proves the previous rescan returned —
// that is the test's only synchronisation, and it needs no sleeps.
func startRescanWatcher(t *testing.T, root string) (*devWatcher, chan time.Time, chan string) {
	t.Helper()
	w, err := newDevWatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	tick := make(chan time.Time)
	w.rescan = tick
	changes := make(chan string, 8)
	go w.pump(changes, io.Discard)
	return w, tick, changes
}

// The case the content prune used to lose: a directory that existed at startup
// with no Go under it is watched by nothing, so its first .go file delivers no
// event at all. Only a walk can notice, and the rescan is that walk.
func TestDevWatcherAdoptsAGoFileUnderAPrunedTree(t *testing.T) {
	root := watchTree(t,
		[]string{"docs/adr", "internal/db/migrations"},
		[]string{"main.go", "go.mod", "docs/README.md", "internal/db/migrations/0001_init.sql"})
	w, tick, changes := startRescanWatcher(t, root)

	// Nothing has changed yet, and a rescan of prose and SQL says so twice.
	tick <- time.Now()
	tick <- time.Now()
	select {
	case p := <-changes:
		t.Fatalf("an idle rescan invented a change: %q", p)
	default:
	}

	// Two levels below the root, one below the pruned top: docs/ is unwatched,
	// docs/adr/ is unwatched, and the file lands in silence.
	if err := os.WriteFile(filepath.Join(root, "docs/adr/gen.go"), []byte("package adr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tick <- time.Now()
	select {
	case got := <-changes:
		if got != "docs/adr/gen.go" {
			t.Fatalf("rescan reported %q, want docs/adr/gen.go", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first .go file under docs/ triggered nothing — the blind spot is open")
	}

	// A whole package tree created inside a still-pruned subtree: no directory
	// on the way down was watched, so no Create event was ever delivered.
	deep := filepath.Join(root, "internal/db/migrations/gen/model")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "model.go"), []byte("package model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tick <- time.Now()
	select {
	case got := <-changes:
		if got != "internal/db/migrations/gen/model/model.go" {
			t.Fatalf("rescan reported %q, want internal/db/migrations/gen/model/model.go", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a new package nested inside a pruned tree triggered nothing")
	}

	// Adoption means real watches, so the SECOND save arrives as an event —
	// the rescan is how a subtree gets in, not how it is polled forever.
	want := []string{
		".",
		"docs", "docs/adr",
		"internal", "internal/db", "internal/db/migrations",
		"internal/db/migrations/gen", "internal/db/migrations/gen/model",
	}
	got := watchedRels(t, root, w.w.WatchList())
	slices.Sort(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("watch set after adoption = %v, want %v", got, want)
	}
}

// The prune's win is the whole reason it exists — 3,778 descriptors down to
// 248 on a Mac — so the rescan must not creep the watch set back up. An idle
// loop over a big Go-free tree registers exactly what it registered at startup.
func TestDevWatcherIdleRescanDoesNotGrowTheWatchSet(t *testing.T) {
	var dirs, files []string
	for i := range 100 {
		d := fmt.Sprintf("docs/adr/%d", i)
		dirs = append(dirs, d, fmt.Sprintf("dist/assets/%d", i))
		files = append(files, d+"/note.md", fmt.Sprintf("dist/assets/%d/app.css", i))
	}
	files = append(files, "main.go", "go.mod", "internal/app/app.go")
	root := watchTree(t, dirs, files)
	w, tick, changes := startRescanWatcher(t, root)

	want := []string{".", "internal", "internal/app"}
	baseline := watchedRels(t, root, w.w.WatchList())
	slices.Sort(baseline)
	if !reflect.DeepEqual(baseline, want) {
		t.Fatalf("startup watch set = %v, want %v", baseline, want)
	}
	for range 10 {
		tick <- time.Now()
	}
	tick <- time.Now() // completes only once the tenth rescan has returned

	got := watchedRels(t, root, w.w.WatchList())
	slices.Sort(got)
	if !reflect.DeepEqual(got, baseline) {
		t.Fatalf("ten idle rescans grew the watch set to %v, want %v", got, baseline)
	}
	select {
	case p := <-changes:
		t.Fatalf("an idle rescan of a 400-entry Go-free tree invented a change: %q", p)
	default:
	}
}

// ---- debounce and coalesce ----

// A save burst is one rebuild, and the batch is de-duplicated and sorted so
// the "(3 files changed)" banner counts files, not events.
func TestDevBatchDebouncesABurst(t *testing.T) {
	in := make(chan string)
	out := make(chan []string)
	stop := make(chan struct{})
	defer close(stop)
	go devBatch(in, out, 20*time.Millisecond, stop)

	for _, p := range []string{"b.go", "a.go", "b.go", "c.go"} {
		in <- p
	}
	select {
	case got := <-out:
		if want := []string{"a.go", "b.go", "c.go"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch after the quiet period")
	}

	// And then nothing: a delivered batch is not redelivered.
	select {
	case extra := <-out:
		t.Fatalf("a second batch appeared out of nowhere: %v", extra)
	case <-time.After(80 * time.Millisecond):
	}
}

// The coalescing rule: every event that lands while a rebuild is in flight
// joins ONE queued rebuild. Here the consumer is simply slow — it does not
// come back for a batch until well after several more saves have landed.
func TestDevBatchCoalescesDuringAnInFlightBuild(t *testing.T) {
	in := make(chan string)
	out := make(chan []string)
	stop := make(chan struct{})
	defer close(stop)
	go devBatch(in, out, 10*time.Millisecond, stop)

	in <- "first.go"
	first := <-out // the consumer takes batch one and "starts building"

	if !reflect.DeepEqual(first, []string{"first.go"}) {
		t.Fatalf("first batch = %v", first)
	}
	// Five saves during the build, from two files.
	for i := 0; i < 5; i++ {
		in <- []string{"x.go", "y.go"}[i%2]
	}
	time.Sleep(50 * time.Millisecond) // the build is still running

	select {
	case got := <-out:
		if want := []string{"x.go", "y.go"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("queued batch = %v, want %v (one rebuild, both files)", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the events that arrived during the build were lost")
	}
	select {
	case extra := <-out:
		t.Fatalf("five saves became more than one queued rebuild: %v", extra)
	case <-time.After(60 * time.Millisecond):
	}
}

// ---- changed-bytes contract sync ----

func TestWriteIfChangedOnlyTouchesRealChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", contractFile)
	wrote, err := writeIfChanged(path, []byte("{}"))
	if err != nil || !wrote {
		t.Fatalf("first write: wrote=%v err=%v", wrote, err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	if wrote, err := writeIfChanged(path, []byte("{}")); err != nil || wrote {
		t.Fatalf("identical bytes must not be written: wrote=%v err=%v", wrote, err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the mtime moved on an unchanged write — the watcher would see that")
	}
	if wrote, err := writeIfChanged(path, []byte(`{"x":1}`)); err != nil || !wrote {
		t.Fatalf("different bytes must be written: wrote=%v err=%v", wrote, err)
	}
}

func TestSyncGeneratedWritesOnlyTheDiff(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	const header = "// Code generated by `x client` (ultrastack). DO NOT EDIT.\n"
	write := func(dir, name, body string) {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// dst already holds the previous generation, plus one stale generated
	// module (a renamed operation) and one file a human put there.
	write(dst, "index.ts", header+"export const v = 1\n")
	write(dst, "orders.ts", header+"export const orders = 1\n")
	write(dst, "legacy.ts", header+"export const gone = 1\n")
	write(dst, ".gitkeep", "")
	write(dst, "notes.md", "mine, not the generator's\n")

	// The new generation: index.ts unchanged, orders.ts moved, one new file.
	write(src, "index.ts", header+"export const v = 1\n")
	write(src, "orders.ts", header+"export const orders = 2\n")
	write(src, "invoices.ts", header+"export const invoices = 1\n")

	unchanged, err := os.Stat(filepath.Join(dst, "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	changed, err := syncGenerated(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"invoices.ts", "legacy.ts", "orders.ts"}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	if after, _ := os.Stat(filepath.Join(dst, "index.ts")); !after.ModTime().Equal(unchanged.ModTime()) {
		t.Error("an identical file was rewritten — vite would reload for nothing")
	}
	if _, err := os.Stat(filepath.Join(dst, "legacy.ts")); !os.IsNotExist(err) {
		t.Error("a stale GENERATED module must go, or the frontend keeps importing a dead route")
	}
	for _, keep := range []string{"notes.md", ".gitkeep"} {
		if _, err := os.Stat(filepath.Join(dst, keep)); err != nil {
			t.Errorf("%s carries no generated banner and must survive: %v", keep, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "orders.ts")); !strings.Contains(string(b), "orders = 2") {
		t.Error("orders.ts was not updated")
	}
}

// The toolbox is assembled from whatever presets a product wires, so the loop
// asks the binary rather than assuming.
func TestDevHasCommand(t *testing.T) {
	help := `usage: app <command> [flags] [args]

commands:
  client               generate the typed TypeScript client
  infra compose        write docker-compose.dev.yml from the graph
  openapi              print the OpenAPI document
  serve                start the app and run until SIGINT/SIGTERM
`
	for _, name := range []string{"client", "openapi", "infra compose", "serve"} {
		if !devHasCommand(help, name) {
			t.Errorf("%q must be found in the listing", name)
		}
	}
	for _, name := range []string{"infra", "compose", "openapiv2", "migrate"} {
		if devHasCommand(help, name) {
			t.Errorf("%q must NOT be found in the listing", name)
		}
	}
	if devHasCommand("unknown command \"help\"\n", "openapi") {
		t.Error("a binary that cannot list its toolbox advertises nothing")
	}
}

// ---- the supervisor, over real processes ----

// devFixture is a stand-in product: it serves nothing, sleeps, and on SIGTERM
// writes a marker naming its own pid before exiting 0. The marker is the
// proof that the graceful path — not the SIGKILL fallback — was taken. Any
// argument is an unknown command, so the loop's toolbox probe fails fast the
// way a product with no api preset does.
//
// Hermetic on purpose: no ultrastack import, so the build is a fraction of a
// second and the test never touches the network.
const devFixture = `package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const version = %q

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "unknown command %%q\n", os.Args[1])
		os.Exit(2)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	fmt.Println("listening", version)
	<-sigs
	wd, _ := os.Getwd()
	os.WriteFile(filepath.Join(wd, fmt.Sprintf("term-%%d", os.Getpid())), []byte(version), 0o644)
}
`

// devFixtureRoot writes a hermetic one-file module and returns its directory.
func devFixtureRoot(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module devfixture\n\ngo 1.26.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	devFixtureSource(t, root, version)
	return root
}

func devFixtureSource(t *testing.T, root, version string) {
	t.Helper()
	body := fmt.Sprintf(devFixture, version)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestLoop(t *testing.T, root string) (*devLoop, *syncBuilder, *syncBuilder) {
	t.Helper()
	out, errW := &syncBuilder{}, &syncBuilder{}
	d := newDevLoop(root, devOptions{}, out, errW)
	d.grace = 5 * time.Second
	if err := d.prepareBuildDir(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stopAll)
	return d, out, errW
}

// syncBuilder is a strings.Builder a child's output goroutine may also write
// to while the test reads it.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuilder) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func skipIfNoProcessSupervision(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and supervises a real process")
	}
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX signals to assert on")
	}
}

// THE central promise: a red build never takes the app down. The compiler
// output is printed, the banner says so, and the process that was serving a
// second ago is still serving.
func TestDevBuildFailureKeepsTheOldBinaryServing(t *testing.T) {
	skipIfNoProcessSupervision(t)
	root := devFixtureRoot(t, "v1")
	d, out, errOut := newTestLoop(t, root)

	d.cycle(nil)
	if !strings.Contains(out.String(), "● serving pid ") {
		t.Fatalf("first cycle must serve:\nout: %s\nerr: %s", out.String(), errOut.String())
	}
	old := d.api
	if !old.alive() {
		t.Fatal("the first binary is not running")
	}

	// Break it the way a human does: mid-edit, unparseable.
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() { this is not go }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	d.cycle([]string{"main.go"})

	if want := "✗ build failed — still serving previous binary"; !strings.Contains(out.String(), want) {
		t.Errorf("banner missing %q, got:\n%s", want, out.String())
	}
	if want := "↻ restarting (1 file changed)"; !strings.Contains(out.String(), want) {
		t.Errorf("banner missing %q, got:\n%s", want, out.String())
	}
	if !strings.Contains(errOut.String(), "main.go") {
		t.Errorf("the compiler output must reach the terminal, got:\n%s", errOut.String())
	}
	if d.api != old {
		t.Fatal("a failed build replaced the running process")
	}
	if !old.alive() {
		t.Fatal("a failed build killed the process that was serving")
	}
	if err := old.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the old pid is gone: %v", err)
	}
}

// A restart is the platform's graceful stop, not a kill: the old process gets
// SIGTERM (to its whole group) and runs its shutdown before the new one
// starts. The marker file is the proof.
func TestDevRestartStopsTheOldProcessGracefully(t *testing.T) {
	skipIfNoProcessSupervision(t)
	root := devFixtureRoot(t, "v1")
	d, out, errOut := newTestLoop(t, root)

	d.cycle(nil)
	old := d.api
	if old == nil || !old.alive() {
		t.Fatalf("first cycle did not serve:\nout: %s\nerr: %s", out.String(), errOut.String())
	}
	oldPID := old.pid()

	devFixtureSource(t, root, "v2")
	out.Reset()
	d.cycle([]string{"main.go"})

	if d.api == old {
		t.Fatal("a green build must restart")
	}
	if d.api.pid() == oldPID {
		t.Fatal("the new process reused the old pid")
	}
	if old.alive() {
		t.Fatal("the old process outlived the restart")
	}
	marker := filepath.Join(root, "term-"+strconv.Itoa(oldPID))
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no graceful-stop marker: the old process was killed, not asked (%v)", err)
	}
	if string(body) != "v1" {
		t.Fatalf("marker written by the wrong binary: %q", body)
	}
	if !strings.Contains(out.String(), "● serving pid ") {
		t.Errorf("the restart banner is missing:\n%s", out.String())
	}

	// And the whole loop comes down on one stop.
	current := d.api
	d.stopAll()
	if current.alive() {
		t.Fatal("stopAll left the API running")
	}
}

// A product with no toolbox — no openapi, no client — gets no contracts
// segment at all, rather than a misleading "contracts unchanged".
func TestDevContractsSilentWithoutAToolbox(t *testing.T) {
	skipIfNoProcessSupervision(t)
	root := devFixtureRoot(t, "v1")
	d, _, _ := newTestLoop(t, root)
	if _, err := d.build(); err != nil {
		t.Fatal(err)
	}
	if got := d.refreshContracts(d.binPath()); got != "" {
		t.Fatalf("refreshContracts = %q, want empty", got)
	}
}

// The override file is Compose's documented escape hatch — and passing -f for
// the generated file turns Compose's own search for it OFF. So the loop has to
// name it, and only when it is there.
func TestDevComposePassesTheOverrideWhenItExists(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, devComposeFile), []byte("services: {}\n"), 0o644))

	want := []string{"compose", "-f", devComposeFile, "up", "-d", "--wait"}
	if got := devComposeArgs(root); !reflect.DeepEqual(got, want) {
		t.Errorf("without an override: docker %v, want docker %v", got, want)
	}

	must(t, os.WriteFile(filepath.Join(root, devComposeOverride), []byte("services: {}\n"), 0o644))
	want = []string{"compose", "-f", devComposeFile, "-f", devComposeOverride, "up", "-d", "--wait"}
	if got := devComposeArgs(root); !reflect.DeepEqual(got, want) {
		t.Errorf("with an override: docker %v, want docker %v", got, want)
	}

	// And the banner says so, because a merge nobody can see is the bug this
	// fixes wearing different clothes.
	var out strings.Builder
	d := newDevLoop(root, devOptions{}, &out, &strings.Builder{})
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH — the banner is only printed on the path that runs it")
	}
	d.bootInfra()
	if !strings.Contains(out.String(), "-f "+devComposeFile+" -f "+devComposeOverride) {
		t.Errorf("the banner must name both files:\n%s", out.String())
	}
}

// The scratch directory ignores itself, so no product's .gitignore has to
// learn about ultra dev.
func TestDevBuildDirIgnoresItself(t *testing.T) {
	root := t.TempDir()
	d := newDevLoop(root, devOptions{}, &strings.Builder{}, &strings.Builder{})
	if err := d.prepareBuildDir(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, devBuildDir, ".gitignore"))
	if err != nil || strings.TrimSpace(string(body)) != "*" {
		t.Fatalf("%s/.gitignore = %q (%v)", devBuildDir, body, err)
	}
}

func TestDevFlagParsing(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errW strings.Builder
	if code := run([]string{"dev", "--nope"}, &out, &errW); code != 2 {
		t.Fatalf("an unknown flag must exit 2, got %d", code)
	}
	if !strings.Contains(errW.String(), `Error: unknown argument "--nope" for "ultra dev"`) {
		t.Fatalf("unhelpful error: %q", errW.String())
	}
	// No go.mod: the loop refuses rather than building nothing forever.
	errW.Reset()
	if code := run([]string{"dev"}, &out, &errW); code != 2 {
		t.Fatalf("outside a product ultra dev must exit 2, got %d", code)
	}
	if !strings.Contains(errW.String(), "no go.mod") {
		t.Fatalf("unhelpful error: %q", errW.String())
	}
}

// A prefixed writer tags LINES, not writes — a child that prints a byte at a
// time must not produce one prefix per byte.
func TestPrefixWriterTagsWholeLines(t *testing.T) {
	var sink strings.Builder
	p := newPrefixer()("[web]", &sink)
	p.Write([]byte("vite "))
	p.Write([]byte("ready\nlocal: "))
	p.Write([]byte("http://x\n"))
	p.Write([]byte("no newline yet"))
	want := "[web] vite ready\n[web] local: http://x\n"
	if sink.String() != want {
		t.Fatalf("got %q, want %q", sink.String(), want)
	}
}

// ---- carriage returns, which a pty makes unavoidable ----

// A spinner redraws by returning to column 0 and overwriting. That works on a
// raw terminal and CANNOT work on a prefixed one: column 0 is where "[web] "
// starts, so the redraw lands on the tag, and while two children share the
// terminal it lands on the other one's line too. So `\r` ends a line here —
// one tagged line per tick, nothing overwritten, nothing smeared.
func TestPrefixWriterTreatsCarriageReturnAsALineBoundary(t *testing.T) {
	cases := []struct {
		name   string
		writes []string
		want   string
		why    string
	}{
		{
			name:   "trailing CR spinner",
			writes: []string{"build 10%\rbuild 90%\rbuild done\n"},
			want:   "[web] build 10%\n[web] build 90%\n[web] build done\n",
			why:    "every tick is its own scrollable line",
		},
		{
			name:   "leading CR spinner",
			writes: []string{"\rstep 1\rstep 2\n"},
			want:   "[web] step 1\n[web] step 2\n",
			why:    "a CR with nothing before it starts a redraw and ends nothing — no empty tagged line",
		},
		{
			name:   "CRLF from a pty's ONLCR",
			writes: []string{"listening on :8080\r\n"},
			want:   "[web] listening on :8080\n",
			why:    "a pty turns every newline into CRLF; the pair is ONE boundary, not two",
		},
		{
			name:   "CRLF split across two reads",
			writes: []string{"listening\r", "\nready\r\n"},
			want:   "[web] listening\n[web] ready\n",
			why:    "a held CR may still be the front half of a CRLF — splitting it invents a blank line",
		},
		{
			name:   "a blank line is still a blank line",
			writes: []string{"a\n\nb\n"},
			want:   "[web] a\n[web] \n[web] b\n",
			why:    "an explicit empty LINE is content; an empty CR segment is not",
		},
		{
			name:   "the tail is held until a boundary",
			writes: []string{"downloading 40%"},
			want:   "",
			why:    "unchanged from before: no boundary, no line",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var sink strings.Builder
			p := newPrefixer()("[web]", &sink)
			for _, w := range c.writes {
				if n, err := p.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v", w, n, err)
				}
			}
			if got := sink.String(); got != c.want {
				t.Fatalf("got %q, want %q — %s", got, c.want, c.why)
			}
			if strings.Contains(sink.String(), "\r") {
				t.Error("a carriage return reached the shared terminal — the prefix will be overwritten")
			}
		})
	}
}

// ---- the pty stream's ending ----

// scriptedReader replays chunks and then fails the way a pseudo-terminal
// master does when the last process holding the slave has exited.
type scriptedReader struct {
	chunks []string
	err    error
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, r.err
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

// THE pty gotcha, and the reason relayPTY exists at all: on Linux a master
// whose slave has no processes left answers read(2) with EIO, not EOF. Report
// that and every single restart grows a spurious "input/output error" line
// under it. It is the end of the stream and nothing else.
func TestRelayPTYTreatsEIOAsTheEndOfTheStream(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr bool
		why     string
	}{
		{"EIO", &fs.PathError{Op: "read", Path: "/dev/ptmx", Err: syscall.EIO}, false,
			"the child exited — this IS the EOF a pty gives"},
		{"wrapped EOF", io.EOF, false, "the ordinary ending, on the platforms that give one"},
		{"we closed the master", os.ErrClosed, false,
			"the exit path closes the master to unblock this loop; that is not a failure either"},
		{"a real failure", syscall.EPERM, true,
			"anything else is news, and swallowing it would hide a supervisor bug"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var sink strings.Builder
			r := &scriptedReader{chunks: []string{"listening", " on :8080\n"}, err: c.err}
			err := relayPTY(&sink, r)
			if (err != nil) != c.wantErr {
				t.Fatalf("relayPTY err = %v, wantErr %v — %s", err, c.wantErr, c.why)
			}
			// Either way, every byte the child managed to write is delivered:
			// a crash's last words are the ones worth having.
			if got := sink.String(); got != "listening on :8080\n" {
				t.Fatalf("output = %q — bytes were dropped on the way out", got)
			}
		})
	}
}

// ---- the colour contract with the children ----

// vite, bun and everything chalk-based read FORCE_COLOR before they read
// isatty, so the colour survives even the pipe path. And NO_COLOR is passed
// THROUGH: the user said no once, and the children must hear the same no.
func TestDevChildEnvCarriesTheColorDecision(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		env     map[string]string
		tty     bool
		opts    devOptions
		wantPTY bool
		want    []string
	}{
		{"a terminal", nil, true, devOptions{}, true,
			[]string{"FORCE_COLOR=1", "CLICOLOR_FORCE=1"}},
		{"--no-pty keeps the colour", nil, true, devOptions{noPTY: true}, false,
			[]string{"FORCE_COLOR=1", "CLICOLOR_FORCE=1"}},
		{"NO_COLOR", map[string]string{"NO_COLOR": "1"}, true, devOptions{}, false,
			[]string{"NO_COLOR=1"}},
		{"redirected to a file", nil, false, devOptions{}, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setEnv(t, map[string]string{"NO_COLOR": "", "FORCE_COLOR": "", "CLICOLOR_FORCE": "", "TERM": "xterm"})
			setEnv(t, c.env)
			forceTerminal(t, c.tty)

			d := newDevLoop(root, c.opts, os.Stdout, os.Stderr)
			if d.pty != c.wantPTY {
				t.Errorf("pty = %v, want %v", d.pty, c.wantPTY)
			}
			if got := d.childEnv(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("childEnv = %v, want %v", got, c.want)
			}
		})
	}
}

// The banners are the loop's whole UI, and the glyph is what the eye lands on
// while the compiler scrolls past. Colour follows the WRITER: stderr may be a
// log while stdout is a terminal.
func TestDevBannersColorTheGlyph(t *testing.T) {
	setEnv(t, map[string]string{"NO_COLOR": "", "CLICOLOR_FORCE": "", "FORCE_COLOR": "1"})
	var out, errW syncBuilder
	d := newDevLoop(t.TempDir(), devOptions{}, &out, &errW)
	d.say(&out, "●", "serving pid %d", 4123)
	d.say(&out, "↻", "restarting (3 files changed)")
	d.say(&out, "·", "stopping")
	d.say(d.errW, "✗", "api exited")

	want := cGreen + "●" + cReset + " serving pid 4123\n" +
		cYellow + "↻" + cReset + " restarting (3 files changed)\n" +
		cDim + "· stopping" + cReset + "\n"
	if got := out.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := errW.String(), cRed+"✗"+cReset+" api exited\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	// NO_COLOR: byte-identical to the banners this loop printed before colour.
	setEnv(t, map[string]string{"NO_COLOR": "1"})
	var plain syncBuilder
	plain2 := newDevLoop(t.TempDir(), devOptions{}, &plain, &plain)
	plain2.say(&plain, "●", "serving pid %d", 4123)
	if got := plain.String(); got != "● serving pid 4123\n" {
		t.Fatalf("NO_COLOR banner = %q", got)
	}
}

// A child on a terminal with no TERM at all assumes the dumbest one there is —
// but only the pty path can do anything about it, and only when the
// environment truly has none. Inheriting beats guessing.
func TestDevChildEnvOnlyDefaultsAMissingTERM(t *testing.T) {
	setEnv(t, map[string]string{"NO_COLOR": "", "FORCE_COLOR": "", "CLICOLOR_FORCE": "", "TERM": ""})
	forceTerminal(t, true)
	d := newDevLoop(t.TempDir(), devOptions{}, os.Stdout, os.Stderr)
	if !slices.Contains(d.childEnv(), "TERM=xterm-256color") {
		t.Fatalf("childEnv = %v, want a TERM default", d.childEnv())
	}
	setEnv(t, map[string]string{"TERM": "screen-256color"})
	for _, kv := range d.childEnv() {
		if strings.HasPrefix(kv, "TERM=") {
			t.Fatalf("childEnv overrode an inherited TERM with %q", kv)
		}
	}
}

// TestDevWebProxyTargetFollowsTheAPIAddr: the vite proxy target is not a
// constant of the universe. `ultra dev` resolves the addr the API will really
// serve on and hands it to the frontend child; the scaffolded vite config
// reads it. Break either half and moving [http] addr turns every API call in
// the browser into a 404 that names nothing.
func TestDevWebProxyTargetFollowsTheAPIAddr(t *testing.T) {
	for _, c := range []struct {
		name, cfg, env, want string
	}{
		{"no config at all", "", "", "http://localhost:8080"},
		{"the file", "[http]\naddr = \":9090\"   # ULTRA_HTTP_ADDR overrides\n", "", "http://localhost:9090"},
		{"a host in the file", "[http]\naddr = \"127.0.0.1:9091\"\n", "", "http://127.0.0.1:9091"},
		// Where it LISTENS is not where you dial it.
		{"a wildcard bind", "[http]\naddr = \"0.0.0.0:9092\"\n", "", "http://localhost:9092"},
		{"env over the file", "[http]\naddr = \":9090\"\n", ":9099", "http://localhost:9099"},
		// [http] in another product's section order, and an addr key that is
		// not the one we want.
		{"addr under another table", "[postgres]\naddr = \":5432\"\n[http]\naddr = \":9093\"\n", "", "http://localhost:9093"},
		// Not host:port: the product will say so far better than we can, and
		// exporting a target we invented would only move the confusion.
		{"nonsense", "[http]\naddr = \"nine thousand\"\n", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if c.cfg != "" {
				if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(c.cfg), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			setEnv(t, map[string]string{"ULTRA_HTTP_ADDR": c.env})
			if got := devAPIOrigin(root); got != c.want {
				t.Fatalf("devAPIOrigin = %q, want %q", got, c.want)
			}

			// The seam that matters: it is in the frontend child's environment.
			setEnv(t, map[string]string{"NO_COLOR": "1"})
			d := newDevLoop(root, devOptions{}, os.Stdout, os.Stderr)
			kv := "ULTRA_DEV_API=" + c.want
			if got := slices.Contains(d.webEnv(), kv); got != (c.want != "") {
				t.Fatalf("webEnv = %v, want %q present = %v", d.webEnv(), kv, c.want != "")
			}
		})
	}

	// And the other end of the wire: the scaffolded config reads that variable,
	// with the literal only as the fallback for a bare `bun dev`.
	dir := filepath.Join(t.TempDir(), "speedcheck")
	if err := scaffold(dir, testData("speedcheck", scaffoldData{Web: true, DS: dsBare})); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "web", "vite.config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "process.env.ULTRA_DEV_API ??") {
		t.Errorf("the vite proxy target must follow ULTRA_DEV_API:\n%s", b)
	}
}

// The dependencies ultra dev adds are TOOL dependencies. The importable
// kernel — what every product links — must still reach nothing outside the
// standard library, which is why `ultra vet` delegates to its own module in
// the first place.
func TestKernelStaysZeroDependency(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to go list")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "./di/...", "./stack/...", "./cli/...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const self = "github.com/bronystylecrazy/ultrastack"
	for _, pkg := range strings.Fields(string(out)) {
		first, _, _ := strings.Cut(pkg, "/")
		if !strings.Contains(first, ".") || strings.HasPrefix(pkg, self) {
			continue
		}
		t.Errorf("the kernel imports %s — di/stack/cli must stay standard-library only "+
			"(a tool's dependency belongs to cmd/ or its own module, the way the analyzer does)", pkg)
	}
}

// toolDeps are the root module's requirements, and every one of them is
// reachable ONLY from cmd/ultra. That is what makes them free for products:
// with Go's module-graph pruning, a module that provides no package a product
// imports contributes nothing to that product's go.mod or go.sum. fsnotify set
// the precedent (the dev watcher); creack/pty follows it (the dev terminal).
var toolDeps = []string{
	"github.com/fsnotify/fsnotify",
	"github.com/creack/pty",
}

// The proof, both directions: every tool dependency IS reached from cmd/ultra
// (or the require line is dead), and NONE of them is reached from anything a
// product can import (or it stops being free). The second half is the one that
// matters — it is the difference between "ultra needs a pty" and "every
// product that links ultrastack needs a pty".
func TestToolDependenciesAreReachableOnlyFromCmd(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to go list")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	deps := func(patterns ...string) map[string]bool {
		t.Helper()
		cmd := exec.Command("go", append([]string{"list", "-deps"}, patterns...)...)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -deps %v: %v", patterns, err)
		}
		set := map[string]bool{}
		for _, pkg := range strings.Fields(string(out)) {
			set[pkg] = true
		}
		return set
	}

	tool := deps("./cmd/...")
	importable := deps("./di/...", "./stack/...", "./cli/...", "./examples/...")
	for _, dep := range toolDeps {
		if !tool[dep] {
			t.Errorf("%s is required by go.mod but no package under cmd/ imports it — drop the require line", dep)
		}
		for pkg := range importable {
			if pkg == dep || strings.HasPrefix(pkg, dep+"/") {
				t.Errorf("%s is reachable from an importable package — it is no longer tool-only, "+
					"and module-graph pruning will stop keeping it out of every product's go.mod", pkg)
			}
		}
	}
}
