//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
)

// startOnPTY starts cmd attached to a pseudo-terminal and returns the master
// side, which is where the child's stdout AND stderr now arrive — one stream,
// interleaved exactly as the child wrote them.
//
// Two things this buys, and they are the whole reason:
//
//   - The child's own isatty() says yes. vite draws its box, bun keeps its
//     colour, and a supervised ultrastack binary picks the console log format
//     and the coloured diagnostic renderer — the same output you get running
//     it by hand, instead of the CI-flavoured one a pipe forces.
//   - Line-buffered stdio flushes per line. Over a pipe, libc switches to 4KB
//     block buffering and a crashing child's last words never arrive.
//
// The process-group contract survives: pty.Start sets Setsid, which makes the
// child a session leader and therefore a process-group leader with pgid ==
// pid, so signalGroup's negative-pid kill still reaches the whole tree.
// Setpgid must NOT also be set — setpgid(2) answers EPERM for a session
// leader, and the exec would fail before the child's main runs.
func startOnPTY(cmd *exec.Cmd, done <-chan struct{}) (*os.File, error) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	go inheritSize(ptmx, done)
	return ptmx, nil
}

// inheritSize keeps the child's terminal the same size as ours: once now, and
// again on every SIGWINCH until the child is gone.
//
// Best effort throughout. When ultra's stdout is not a terminal (the forced
// path a test takes) there is no size to copy and every call fails harmlessly
// — a child that never learns the width just wraps like an 80-column one,
// which is strictly better than not starting.
func inheritSize(ptmx *os.File, done <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)

	_ = pty.InheritSize(os.Stdout, ptmx)
	for {
		select {
		case <-done:
			return
		case <-ch:
			_ = pty.InheritSize(os.Stdout, ptmx)
		}
	}
}
