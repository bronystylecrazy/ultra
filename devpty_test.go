//go:build unix

package main

import (
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processGroupOf answers what the kernel thinks, not what we asked for.
func processGroupOf(pid int) (int, error) { return syscall.Getpgid(pid) }

// The pty path, end to end, over a real child.
//
// There is no terminal in CI, so the detection is bypassed and procSpec.pty is
// forced — that is exactly the seam the field decision hangs off, and forcing
// it is the only way to exercise the code a developer's terminal takes. A
// machine with no pty to give (a locked-down container) skips rather than
// fails: the supervisor's contract is that it falls back, not that a pty
// always exists.
func TestStartManagedRunsTheChildOnATerminal(t *testing.T) {
	skipIfNoProcessSupervision(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to supervise")
	}

	// The child asserts its OWN view: `[ -t 1 ]` is isatty(1), the question
	// vite and the API binary both ask before deciding to colour. Then it
	// prints progress the way a bundler does — redrawing one line with \r.
	const script = `[ -t 1 ] && echo "isatty yes"
printf 'downloading 10%%\rdownloading 90%%\rdownloaded\n'`

	var sink syncBuilder
	exits := make(chan *managedProc, 1)
	p, err := startManaged(procSpec{
		dir:  t.TempDir(),
		bin:  sh,
		args: []string{"-c", script},
		env:  []string{"FORCE_COLOR=1", "CLICOLOR_FORCE=1"},
		pty:  true,
	}, newPrefixer()("[web]", &sink), io.Discard, exits)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	if p.ptmx == nil {
		t.Skip("no pseudo-terminal available here — the supervisor fell back to pipes, as designed")
	}

	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the child never finished — the pty relay is not ending on EIO/EOF")
	}

	got := sink.String()
	if !strings.Contains(got, "[web] isatty yes") {
		t.Fatalf("the child did not see a terminal on its stdout:\n%s", got)
	}
	// Every spinner tick is its own tagged line, and not one carriage return
	// reached the shared terminal to overwrite a prefix.
	for _, want := range []string{
		"[web] downloading 10%",
		"[web] downloading 90%",
		"[web] downloaded",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\r") {
		t.Errorf("a carriage return survived the prefixer:\n%q", got)
	}
	// And the clean end: EIO on the master is the stream ending, never an
	// error line under the output.
	if strings.Contains(got, "input/output error") {
		t.Errorf("the pty's EIO leaked into the terminal:\n%s", got)
	}

	select {
	case dead := <-exits:
		if dead != p {
			t.Fatal("the exit notification named a different child")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the child's exit was never announced")
	}
	if reason := p.exitReason(); reason != "status 0" {
		t.Fatalf("exitReason = %q", reason)
	}
}

// A pty child still belongs to its own process group, which is what every
// signal in this supervisor targets. pty.Start gets there by Setsid — a
// session leader IS a group leader with pgid == pid — and NOT by Setpgid,
// which would answer EPERM for a session leader and fail the exec before the
// child's main ran. If that ever regresses, `ultra dev` leaks vite.
func TestPTYChildIsItsOwnProcessGroupLeader(t *testing.T) {
	skipIfNoProcessSupervision(t)
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to supervise")
	}
	var sink syncBuilder
	p, err := startManaged(procSpec{
		dir:  t.TempDir(),
		bin:  sh,
		args: []string{"-c", "trap 'exit 0' TERM; while :; do sleep 0.05; done"},
		pty:  true,
	}, newPrefixer()("[api]", &sink), io.Discard, nil)
	if err != nil {
		t.Fatalf("startManaged: %v", err)
	}
	if p.ptmx == nil {
		t.Skip("no pseudo-terminal available here")
	}
	t.Cleanup(func() { p.stop(2 * time.Second) })

	pgid, err := processGroupOf(p.pid())
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid != p.pid() {
		t.Fatalf("pgid = %d, want %d — the child shares OUR group and signalGroup would kill ultra dev", pgid, p.pid())
	}

	// The graceful path still reaches it through the pty.
	done := make(chan struct{})
	go func() { p.stop(5 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop() never returned — SIGTERM did not reach the pty child's group")
	}
	if p.alive() {
		t.Fatal("the child outlived stop()")
	}
}
