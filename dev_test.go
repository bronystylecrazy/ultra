package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

// devWatchDirs must not descend into the excluded trees at all: registering a
// watch per node_modules directory is how a dev loop runs out of file
// descriptors.
func TestDevWatchDirsSkipsExcludedTrees(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"internal/app/orders",
		"internal/db/gen",
		"web/src/lib/api",
		"web/node_modules/svelte",
		".git/objects",
		".ultradev",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var rels []string
	for _, d := range devWatchDirs(root) {
		rel, _ := filepath.Rel(root, d)
		rels = append(rels, filepath.ToSlash(rel))
	}
	want := []string{".", "internal", "internal/app", "internal/app/orders", "internal/db", "internal/db/gen"}
	if !reflect.DeepEqual(rels, want) {
		t.Fatalf("watched dirs = %v, want %v", rels, want)
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
	if !strings.Contains(errW.String(), "ultra dev: unknown argument") {
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

// The dependency ultra dev adds is a TOOL dependency. The importable kernel —
// what every product links — must still reach nothing outside the standard
// library, which is why `ultra vet` delegates to its own module in the first
// place.
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
