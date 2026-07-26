package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// The process half of `ultra dev`. Three rules, all learned the hard way:
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
//
//  3. A child that is asked "are you a terminal?" must be able to say yes.
//     Piping a dev server's stdout is what turns vite's box into a grey wall,
//     drops the API's coloured diagnostics, and switches libc to 4KB block
//     buffering so a crash loses its last words. When ultra's own stdout is a
//     terminal, so is every child's — see devpty_unix.go.

// errNoPTY is the platform saying it has no pseudo-terminal to give. Not an
// error the loop reports: it is the signal to use the pipe path.
var errNoPTY = errors.New("no pty on this platform")

// procSpec is everything startManaged needs about one child. A struct rather
// than six positional arguments because two of the six are booleans about
// terminals, and `startManaged(dir, bin, args, env, true, false, ...)` is not
// a call anyone can read.
type procSpec struct {
	dir  string
	bin  string
	args []string

	// env is appended to the inherited environment: the colour contract with
	// the children (FORCE_COLOR / CLICOLOR_FORCE / NO_COLOR / TERM).
	env []string

	// pty asks for a pseudo-terminal. Honoured only where the platform has
	// one; a refusal falls back to pipes rather than failing the start.
	pty bool
}

// managedProc is one supervised child: the built API binary, or the frontend
// dev server.
type managedProc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when Wait returns
	err  error         // read only after <-done

	// ptmx is the master side when this child runs on a pseudo-terminal, and
	// nil when it runs on pipes. Its presence is also the answer to "did the
	// pty path actually happen", which is the only thing a test can assert.
	ptmx *os.File

	// expected marks a stop WE asked for, so the exit notification is not
	// reported as a crash. Written and read on the loop goroutine only.
	expected bool
}

// startManaged starts a child with its own process group, streaming its
// output through the given writers and announcing its exit on exits.
//
// On the pty path there is only ONE stream — a terminal has no separate
// stderr — so everything lands on out, interleaved exactly as the child wrote
// it. errOut is used only by the pipe path.
func startManaged(spec procSpec, out, errOut io.Writer, exits chan<- *managedProc) (*managedProc, error) {
	cmd := exec.Command(spec.bin, spec.args...)
	cmd.Dir = spec.dir
	cmd.Env = append(os.Environ(), spec.env...)

	p := &managedProc{cmd: cmd, done: make(chan struct{})}
	relayed := make(chan struct{})

	if spec.pty {
		// A pty that cannot be allocated (the system ran out, or this is not
		// a unix) is not a reason to refuse to start a dev server. Fall back.
		if ptmx, err := startOnPTY(cmd, p.done); err == nil {
			p.ptmx = ptmx
			go func() {
				defer close(relayed)
				relayPTY(out, ptmx)
			}()
		}
	}
	if p.ptmx == nil {
		close(relayed)
		cmd.Stdout, cmd.Stderr = out, errOut
		setProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
	}

	go func() {
		p.err = cmd.Wait()
		if p.ptmx != nil {
			// Drain what the child wrote just before dying, then close. The
			// wait is BOUNDED: a grandchild that inherited the tty and is
			// still holding it keeps the master readable forever, and the
			// exit report must not wait for a leaked vite.
			select {
			case <-relayed:
			case <-time.After(ptyDrainGrace):
			}
			p.ptmx.Close() // unblocks the relay if it is still reading
		}
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

// ptyDrainGrace bounds the wait for the last bytes off a dying child's
// terminal. Long enough that a panic's stack trace lands before the "exited"
// line, short enough that nobody notices.
const ptyDrainGrace = 200 * time.Millisecond

// relayPTY copies a child's terminal output to w until the stream ends.
//
// THE pty gotcha: when the last process holding the slave side exits, Linux
// answers the master's read with EIO, not EOF. Treating that as a failure is
// how a supervisor grows a spurious "read /dev/ptmx: input/output error" line
// under every single restart. It is the end of the stream and nothing else —
// as is ErrClosed, which is us closing the master to unblock this loop.
func relayPTY(w io.Writer, r io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if isPTYEOF(err) {
				return nil
			}
			return err
		}
	}
}

// isPTYEOF reports whether err is a pseudo-terminal's way of saying "done".
func isPTYEOF(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.EIO) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, fs.ErrClosed)
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
// are buffered until the boundary arrives: a progress spinner must not become
// one prefix per byte.
//
// THE CARRIAGE RETURN IS A BOUNDARY TOO, and it has to be, because a prefixed
// stream cannot honour one. A spinner redraws by returning to column 0 and
// overwriting — which works only if column 0 is where its line started. Ours
// starts after "[web] ", so the redraw lands on top of the prefix and the two
// smear into each other, and if the other child prints meanwhile the spinner
// overwrites THAT. So a `\r` ends the line here: the progress becomes one
// tagged line per tick, scrollable, and nothing is ever overwritten. Verbose
// beside a raw terminal — but a raw terminal is exactly what a prefixed
// stream is not, and this is why a pty raises the stakes: the children now
// print spinners they would have suppressed over a pipe.
//
// A `\r` that is the last byte held may still be the front half of a CRLF —
// a pty's ONLCR turns every newline into one — so it waits for the next
// write rather than splitting the pair and emitting a stray blank line.
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
		i := bytes.IndexAny(p.buf, "\r\n")
		if i < 0 {
			return len(b), nil
		}
		end := i + 1
		if p.buf[i] == '\r' {
			if end == len(p.buf) {
				return len(b), nil // might be the front half of a CRLF
			}
			if p.buf[end] == '\n' {
				end++
			}
		}
		line := p.buf[:i]
		bare := p.buf[i] == '\r' && end == i+1
		p.buf = p.buf[end:]
		// A bare `\r` with nothing before it is the *start* of a redraw, not
		// the end of anything. Emitting it would put one empty tagged line in
		// front of every spinner tick.
		if bare && len(line) == 0 {
			continue
		}
		if _, err := io.WriteString(p.w, p.prefix+" "); err != nil {
			return len(b), err
		}
		if _, err := p.w.Write(line); err != nil {
			return len(b), err
		}
		if _, err := io.WriteString(p.w, "\n"); err != nil {
			return len(b), err
		}
	}
}
