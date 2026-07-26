package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// The process half of `ultra dev`. Two rules, both learned the hard way:
//
//  1. A dev server is never ONE process. `bun dev` forks vite; a supervised
//     binary may fork anything. Signaling the direct child leaves the
//     grandchildren holding the port, and the next start fails to bind with
//     an error that names nothing. So every child is started in its own
//     process GROUP and every signal goes to the group.
//
//  2. SIGTERM first, always. The platform's lifecycle IS the graceful stop —
//     reverse-order Stop, drained connections, closed pools. SIGKILL is the
//     fallback after the grace window, not the tool.

// managedProc is one supervised child: the built API binary, or the frontend
// dev server.
type managedProc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when Wait returns
	err  error         // read only after <-done

	// expected marks a stop WE asked for, so the exit notification is not
	// reported as a crash. Written and read on the loop goroutine only.
	expected bool
}

// startManaged starts bin in dir with its own process group, streaming its
// output through the given writers and announcing its exit on exits.
func startManaged(dir, bin string, args []string, out, errOut io.Writer, exits chan<- *managedProc) (*managedProc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = out, errOut
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &managedProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
		if exits != nil {
			select {
			case exits <- p:
			default: // nobody is listening any more; the shutdown path reaps
			}
		}
	}()
	return p, nil
}

func (p *managedProc) pid() int {
	if p == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *managedProc) alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// stop is the graceful path: SIGTERM the whole group, wait up to grace for
// the lifecycle to unwind, then SIGKILL the group and wait for the reap.
// Returns when the child is genuinely gone — the next start must not race the
// old process for the port.
func (p *managedProc) stop(grace time.Duration) {
	if p == nil || !p.alive() {
		return
	}
	p.expected = true
	signalGroup(p.cmd.Process, syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(grace):
	}
	signalGroup(p.cmd.Process, syscall.SIGKILL)
	<-p.done
}

// exitReason renders why a child is gone, for the one line that reports it.
func (p *managedProc) exitReason() string {
	<-p.done
	if p.err == nil {
		return "status 0"
	}
	if ee, ok := p.err.(*exec.ExitError); ok {
		return fmt.Sprintf("status %d", ee.ExitCode())
	}
	return p.err.Error()
}

// prefixWriter tags every LINE of a child's output with its source, so one
// terminal can carry two servers without the reader guessing. Partial writes
// are buffered until the newline arrives: a progress spinner must not become
// one prefix per byte.
//
// All prefixWriters over one terminal share a mutex, so [api] and [web] never
// interleave inside a line.
type prefixWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func newPrefixer() func(prefix string, w io.Writer) io.Writer {
	mu := &sync.Mutex{}
	return func(prefix string, w io.Writer) io.Writer {
		return &prefixWriter{mu: mu, w: w, prefix: prefix}
	}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := p.buf[:i+1]
		p.buf = p.buf[i+1:]
		if _, err := io.WriteString(p.w, p.prefix+" "); err != nil {
			return len(b), err
		}
		if _, err := p.w.Write(line); err != nil {
			return len(b), err
		}
	}
}
