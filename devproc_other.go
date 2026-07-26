//go:build !unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// Windows has no process groups in the POSIX sense and no graceful signal an
// unrelated process may send. The loop still works — it just cannot ask
// politely, so the "graceful stop" is a kill and a product's Stop hooks do
// not run. Named here rather than hidden behind a compile error.
func setProcessGroup(cmd *exec.Cmd) {}

func signalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
