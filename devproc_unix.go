//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup gives the child its own process group, which is what makes
// signalGroup possible at all: without Setpgid the child shares OUR group,
// and killing the group would kill `ultra dev` itself.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the child's whole process group — the negative pid
// form. That is the difference between stopping a dev server and leaking the
// vite (or `go run`-spawned) grandchild that still holds the port.
//
// A group that is already gone while the leader lingers as a zombie answers
// ESRCH; fall back to the direct child so the reap still happens.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, sig); err == nil {
		return nil
	}
	return p.Signal(sig)
}
