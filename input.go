package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// All keyboard input goes through one buffer. Two things made a big paste
// (24 questions at once) fall apart before:
//
//   - while the model was answering, nothing read stdin, and the terminal's
//     canonical line buffer is about a kilobyte — the rest was dropped, which
//     is why pasted text came back cut mid-word;
//   - what survived arrived as separate lines, so each line started its own
//     turn instead of being one message.
//
// Now a turn keeps draining stdin into this buffer (with the line discipline
// off, so there is no length limit and no echo of what you type ahead), and
// whatever was typed or pasted meanwhile is handed to the next prompt as one
// staged message you send with Enter.

type Input struct {
	f   *os.File
	tty bool // a real terminal on both ends — see IsTTY

	mu      sync.Mutex
	pending []byte

	stopCapture func()
}

func NewInput(f *os.File) *Input {
	in := &Input{f: f}
	if st, err := f.Stat(); err == nil {
		in.tty = st.Mode()&os.ModeCharDevice != 0 && osTermWidth() > 0
	}
	return in
}

// newStringInput is an Input with no terminal behind it: unattended runs (eval)
// and tests, where reads just run out.
func newStringInput(s string) *Input { return &Input{pending: []byte(s)} }

// IsTTY reports that there is a real terminal on BOTH ends: stdin to read keys
// from and stdout to draw on. osTermWidth() answers for stdout only (it ioctls
// stdout) and makeRaw would answer for stdin only by changing it, so the cheap
// stdlib test is used for stdin and both are required — a menu you cannot see
// is worse than no menu. A pipe (CI, `lca … < file`) answers false, and the
// caller then names the non-interactive path instead of blocking on a question
// nobody is there to answer.
func (in *Input) IsTTY() bool { return in.tty }

// fd is the descriptor to put in raw mode, or -1 when there is no file behind
// this Input. -1 is not an error: a scripted Input's bytes are already raw —
// there is simply no terminal discipline to switch off.
func (in *Input) fd() int {
	if in.f == nil {
		return -1
	}
	return int(in.f.Fd())
}

// ReadByte returns the next byte, blocking only when the buffer is empty.
func (in *Input) ReadByte() (byte, error) {
	in.mu.Lock()
	if len(in.pending) > 0 {
		b := in.pending[0]
		in.pending = in.pending[1:]
		in.mu.Unlock()
		return b, nil
	}
	in.mu.Unlock()

	if in.f == nil {
		return 0, io.EOF
	}
	var one [512]byte
	n, err := in.f.Read(one[:])
	if n > 0 {
		in.mu.Lock()
		in.pending = append(in.pending, one[1:n]...)
		in.mu.Unlock()
		return one[0], nil
	}
	if err == nil {
		err = io.EOF
	}
	return 0, err
}

// ReadString reads until delim (used by the cooked fallback and the approval
// prompt).
func (in *Input) ReadString(delim byte) (string, error) {
	var b strings.Builder
	for {
		c, err := in.ReadByte()
		if err != nil {
			return b.String(), err
		}
		b.WriteByte(c)
		if c == delim {
			return b.String(), nil
		}
	}
}

// Drain moves everything the terminal has ready into the buffer. It never
// blocks, so it is safe to call while the agent is busy.
func (in *Input) Drain() {
	if in.f == nil {
		return
	}
	data := readAvailable(in.f)
	if len(data) == 0 {
		return
	}
	in.mu.Lock()
	in.pending = append(in.pending, data...)
	in.mu.Unlock()
}

// TakePending removes and returns buffered input as text — what the person
// typed or pasted while the agent was working.
func (in *Input) TakePending() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := string(in.pending)
	in.pending = nil
	return s
}

// Discard throws buffered input away (before an approval question, so a stray
// keystroke can't answer it).
func (in *Input) Discard() string { return in.TakePending() }

// Put returns text to the buffer, ahead of anything else.
func (in *Input) Put(s string) {
	if s == "" {
		return
	}
	in.mu.Lock()
	in.pending = append([]byte(s), in.pending...)
	in.mu.Unlock()
}

// StartCapture keeps reading stdin while the agent works: the terminal is put
// in a mode without the canonical line limit and without echo, so a long paste
// survives whole and doesn't scribble over the agent's output.
func (in *Input) StartCapture() {
	if in.stopCapture != nil || in.f == nil {
		return
	}
	restore, err := makeTurnMode(int(in.f.Fd()))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				in.Drain()
				return
			case <-t.C:
				in.Drain()
			}
		}
	}()
	in.stopCapture = func() {
		close(stop)
		<-done
		if err == nil {
			restore()
		}
	}
}

// StopCapture ends capturing and returns the terminal to its usual state.
func (in *Input) StopCapture() {
	if in.stopCapture == nil {
		return
	}
	in.stopCapture()
	in.stopCapture = nil
}

// pasteSummary describes staged text for the prompt line.
func pasteSummary(s string) string {
	lines := strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
	return fmt.Sprintf("%s pasted, %s", plural(lines, "line", "lines"), byteCount(len(s)))
}
