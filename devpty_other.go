//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// Windows (and plan9, and js) have no pty in the sense this loop needs: no
// Setsid, no controlling terminal to hand a child, no ONLCR. The supervisor
// answers errNoPTY and falls back to the pipe path it has always used —
// FORCE_COLOR still reaches the children, so vite and bun keep their colour
// even here. Named rather than hidden behind a compile error.
func startOnPTY(cmd *exec.Cmd, done <-chan struct{}) (*os.File, error) {
	return nil, errNoPTY
}
