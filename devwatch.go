package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// The watch half of `ultra dev`: which paths mean "rebuild the backend", and
// how a burst of them becomes exactly one rebuild.
//
// The exclusion list is the interesting part, because two of its entries are
// paths ultra dev ITSELF writes. A loop that watches its own output is an
// infinite loop: refresh openapi.json → the watcher sees a write → rebuild →
// refresh openapi.json. The generated client (web/src/lib/api) is the same
// trap with more files. So the rule is not "ignore noise" — it is "never
// watch your own exhaust".

// devExcludedRoots are the top-level paths ultra dev never descends into.
// Each one is either somebody else's job or our own output:
//
//	web/            the frontend dev server owns it (vite has its own watcher),
//	                and web/src/lib/api is the client WE regenerate
//	node_modules/   never source, always enormous
//	vendor/         Go source, but not source anyone EDITS: a vendored tree
//	                changes only when `go mod vendor` rewrites it, and that
//	                changes go.mod/go.sum too — which ARE watched. So the
//	                rebuild still happens, without watching ten thousand files
//	testdata/       the go tool excludes testdata from package loading, so a
//	                .go file under it is data, not a compilation input
//
// Anything whose path has a dot-segment is excluded too (see devExcluded):
// that covers .git/, the .ultradev/ build output, .svelte-kit/, editor
// scratch directories, and emacs' `.#main.go` lock files in one rule.
//
// The names matter more on macOS than anywhere else. Linux has inotify, which
// watches a DIRECTORY with one descriptor. kqueue cannot: fsnotify has to open
// one descriptor per FILE and, on every event, re-read the directory listing
// to work out what changed. A 4,000-file tree measured 3,628 open descriptors
// here — so on a Mac, every directory this list removes is paid back twice.
var devExcludedRoots = []string{"web", "node_modules", "vendor", "testdata"}

// devExcludedOutputs are the derived artifacts the loop regenerates on every
// restart, named here so the "never watch your own exhaust" rule is explicit
// rather than an accident of the extension filter. They are the same two
// constants `ultra upgrade` refreshes — one definition, one meaning.
var devExcludedOutputs = []string{contractFile, clientDir}

// devExcluded reports whether rel (slash-separated, relative to the product
// root) is outside the watch entirely — as a file OR as a directory to walk.
func devExcluded(rel string) bool {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return false
	}
	for _, out := range devExcludedOutputs {
		if rel == out || strings.HasPrefix(rel, out+"/") {
			return true
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
		for _, ex := range devExcludedRoots {
			if seg == ex {
				return true
			}
		}
	}
	return false
}

// devShouldTrigger reports whether a change at rel must rebuild the backend.
// Only Go compilation inputs qualify: **/*.go plus go.mod and go.sum.
//
// config.toml is deliberately NOT here. The reloadable sections reload
// themselves at runtime, and a restart on every keystroke in a config file
// would trade a live reload for a cold boot.
func devShouldTrigger(rel string) bool {
	if devExcluded(rel) {
		return false
	}
	return devIsBuildInput(filepath.Base(filepath.ToSlash(rel)))
}

// devIsBuildInput reports whether a FILE name is one the loop rebuilds for.
// The trigger set and the watch set are decided by the same predicate, so a
// directory can never be watched for a file that could not have mattered.
func devIsBuildInput(name string) bool {
	return name == "go.mod" || name == "go.sum" || strings.HasSuffix(name, ".go")
}

// devWatchDirs is the STARTUP watch set: every directory that could deliver a
// rebuild, and not one more. fsnotify is not recursive — one AddWatch per
// directory is the whole trick — and devWatcher.pump adds new ones as they
// appear.
//
// Two prunes, and the second is the one that costs a Mac real CPU:
//
//  1. By NAME, at WALK time (fs.SkipDir), so an excluded tree is never
//     descended into at all. Registering a watch per node_modules directory is
//     how a dev loop runs out of descriptors; walking one is how it takes a
//     second to start.
//  2. By CONTENT: a directory with no Go input anywhere below it is not
//     watched. `internal/db/migrations` is SQL, `docs/` is prose, `dist/` is
//     output — every event they deliver is one this loop reads, converts,
//     tests against two predicates, and throws away. Measured here: ~30µs of
//     ultra CPU per discarded event, and on macOS an open descriptor per file
//     on top. A directory that cannot produce a rebuild should not be able to
//     produce an interrupt.
//
// Rule 2 would be a blind spot on its own — the FIRST .go file under a
// directory that had none, and existed at startup, arrives in a directory
// nobody is watching. So the second return value is the TOPMOST directory of
// every subtree rule 2 dropped: the exact, and only, list the loop has to
// re-examine to close it (see devWatcher.rescanPruned). A directory CREATED
// while the loop runs needs none of that — devNewWatchDirs watches it on sight.
//
// An unreadable subtree is skipped, never fatal — a dev loop that refuses to
// start over one permission bit is worse than a dev loop with a blind spot.
func devWatchDirs(root string) (dirs, pruned []string) {
	candidates := devNewWatchDirs(root)
	if len(candidates) == 0 {
		return nil, nil
	}

	// Which directories hold a Go input, and therefore must be watched along
	// with every ancestor up to the root.
	keep := map[string]bool{root: true}
	filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable is a blind spot, not a failure
		}
		if e.IsDir() {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil || (rel != "." && devExcluded(rel)) {
				return fs.SkipDir
			}
			return nil
		}
		if !devIsBuildInput(e.Name()) {
			return nil
		}
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			if keep[d] {
				break // this ancestor chain is already marked
			}
			keep[d] = true
			if d == root || filepath.Dir(d) == d {
				break
			}
		}
		return nil
	})

	dirs = candidates[:0:0] // fresh backing array, walk order preserved
	for _, p := range candidates {
		switch {
		case keep[p]:
			dirs = append(dirs, p)
		case keep[filepath.Dir(p)]:
			// Its parent is watched but it is not: the top of a dropped subtree.
			pruned = append(pruned, p)
		}
	}
	return dirs, pruned
}

// devNewWatchDirs is the watch set for a directory that appeared WHILE the
// loop is running: pruned by name, never by content. A directory being created
// right now is a package being written right now — `ultra new feature billing`
// makes `internal/app/billing/` a moment before billing.go lands in it — and a
// loop that waited for the Go file to exist before watching for it would miss
// every new feature's first save.
func devNewWatchDirs(root string) []string {
	var dirs []string
	filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || !e.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is a blind spot, not a failure
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		if rel != "." && devExcluded(rel) {
			return fs.SkipDir
		}
		dirs = append(dirs, p)
		return nil
	})
	return dirs
}

// devRescanEvery is how long the first .go file under a Go-free directory can
// go unnoticed. It is the ONLY periodic work in the loop, so it is deliberately
// slower than a save: one pass walks the dropped subtrees and nothing else —
// ~105µs over a product's docs/ and migrations/, 4.6ms over a 500-entry tree
// with a built dist/ in it, which is the filesystem's price for the walk and
// not ours. Three seconds puts the typical pass at ~0.003% of a core, for a
// case that happens once per feature.
const devRescanEvery = 3 * time.Second

// devWatcher is the fsnotify side: it turns filesystem events into relative
// paths worth rebuilding for, and keeps itself current as packages appear.
type devWatcher struct {
	w    *fsnotify.Watcher
	root string

	// pruned are the subtree roots the content prune dropped, and rescan is
	// when to look at them again. Only pump touches pruned after construction.
	pruned []string
	ticker *time.Ticker
	rescan <-chan time.Time
}

func newDevWatcher(root string) (*devWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dirs, pruned := devWatchDirs(root)
	t := time.NewTicker(devRescanEvery)
	d := &devWatcher{w: w, root: root, pruned: pruned, ticker: t, rescan: t.C}
	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			d.Close()
			return nil, fmt.Errorf("watch %s: %w", dir, err)
		}
	}
	return d, nil
}

func (d *devWatcher) Close() error {
	d.ticker.Stop()
	return d.w.Close()
}

// rescanPruned adopts any dropped subtree that has since grown a Go file, and
// reports the file that did it so the change rebuilds like any other save.
//
// This is what the content prune costs and how it is paid: those directories
// deliver no events because nothing watches them, so a walk is the only thing
// that can notice. Only the dropped subtrees are walked, each abandoned at its
// first Go file, and an adopted one re-enters by the normal rules — including
// its own newly dropped subtrees, which take its place on the list.
func (d *devWatcher) rescanPruned(changes chan<- string) {
	var still []string
	for _, dir := range d.pruned {
		first := devFirstBuildInput(d.root, dir)
		if first == "" {
			still = append(still, dir)
			continue
		}
		dirs, pruned := devWatchDirs(dir)
		for _, add := range dirs {
			d.w.Add(add)
		}
		still = append(still, pruned...)
		changes <- first
	}
	d.pruned = still
}

// devFirstBuildInput returns the root-relative path of the first Go input
// under dir, or "" if there is none. It reads no bytes and stops at the first
// hit: the rescan asks whether a subtree has become interesting, not what is
// in it.
func devFirstBuildInput(root, dir string) string {
	var found string
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable is a blind spot, not a failure
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if devExcluded(rel) {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if e.IsDir() || !devIsBuildInput(e.Name()) {
			return nil
		}
		found = rel
		return fs.SkipAll
	})
	return found
}

// pump forwards the triggering changes to changes and returns when the
// watcher is closed. A brand-new directory is registered as it is created, so
// `ultra new feature billing` is watched the moment it exists; a directory
// that was already there and was too Go-free to watch is picked up by the
// rescan instead.
func (d *devWatcher) pump(changes chan<- string, errW io.Writer) {
	for {
		select {
		case <-d.rescan:
			d.rescanPruned(changes)
		case ev, ok := <-d.w.Events:
			if !ok {
				return
			}
			rel, err := filepath.Rel(d.root, ev.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if devExcluded(rel) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					// Unconditionally, by name only: a directory appearing
					// under the loop's nose is a package being written, and
					// its first .go file has not landed yet.
					for _, dir := range devNewWatchDirs(ev.Name) {
						d.w.Add(dir)
					}
					continue
				}
			}
			// Every op counts, Chmod included: on macOS `touch main.go` is a
			// bare attribute event, and "touch it to rebuild" is a reflex no
			// loop should punish. Spurious ones cost a debounced no-op build,
			// and our OWN writes cannot land here — the outputs are excluded
			// and written only when the bytes actually moved.
			if devShouldTrigger(rel) {
				changes <- rel
			}
		case err, ok := <-d.w.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(errW, "watch: %v\n", err)
		}
	}
}

// devBatch debounces and coalesces: it collects change paths from in and
// hands the consumer ONE sorted, de-duplicated batch per quiet period.
//
// The coalescing is the part a plain time.AfterFunc cannot do. A rebuild
// takes longer than the debounce, and a save-on-every-keystroke editor will
// fire again while it runs. Because out is unbuffered, the ready batch is
// only handed over when the consumer comes back for it, and every event that
// arrives in the meantime joins the SAME pending set — one queued rebuild, no
// matter how many saves land during the build.
func devBatch(in <-chan string, out chan<- []string, quiet time.Duration, stop <-chan struct{}) {
	pending := map[string]bool{}
	var timer *time.Timer
	ready := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var tick <-chan time.Time
		if timer != nil {
			tick = timer.C
		}
		var send chan<- []string
		var batch []string
		if ready && len(pending) > 0 {
			send, batch = out, sortedKeys(pending)
		}
		select {
		case p, ok := <-in:
			if !ok {
				return
			}
			pending[p] = true
			ready = false
			if timer == nil {
				timer = time.NewTimer(quiet)
				break
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(quiet)
		case <-tick:
			timer, ready = nil, true
		case send <- batch:
			pending, ready = map[string]bool{}, false
		case <-stop:
			return
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
