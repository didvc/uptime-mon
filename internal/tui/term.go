// Package tui renders the live view.
//
// It talks to the terminal directly with ANSI escape sequences rather than
// through a widget framework. The whole interface is a table, two charts and a
// stats panel; a framework would add a dependency tree larger than this
// program in exchange for layout features it does not use. The only external
// package here is golang.org/x/term, for the one thing that genuinely needs
// platform code: putting the terminal into raw mode.
package tui

import (
	"bufio"
	"os"
	"strings"
	"sync"

	"golang.org/x/term"
)

// screen owns the terminal state and the output buffer.
type screen struct {
	out    *bufio.Writer
	fd     int
	old    *term.State
	raw    bool
	color  bool
	width  int
	height int

	frame strings.Builder // reused between renders to avoid per-frame garbage
}

func newScreen(color bool) *screen {
	return &screen{
		out:   bufio.NewWriterSize(os.Stdout, 64<<10),
		fd:    int(os.Stdout.Fd()),
		color: color,
	}
}

// IsTerminal reports whether stdout is an interactive terminal.
func IsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

func (s *screen) enter() error {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	s.old, s.raw = st, true
	s.refreshSize()
	// Alternate screen, hide cursor. Leaving restores the user's scrollback
	// exactly as it was, which is the difference between a tool that feels
	// borrowed and one that feels intrusive.
	s.out.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
	return s.out.Flush()
}

func (s *screen) leave() {
	s.out.WriteString("\x1b[?25h\x1b[?1049l")
	s.out.Flush()
	if s.raw && s.old != nil {
		_ = term.Restore(int(os.Stdin.Fd()), s.old)
		s.raw = false
	}
}

func (s *screen) refreshSize() (changed bool) {
	w, h, err := term.GetSize(s.fd)
	if err != nil || w <= 0 || h <= 0 {
		w, h = 100, 30 // a sane fallback when stdout is redirected
	}
	changed = w != s.width || h != s.height
	s.width, s.height = w, h
	return changed
}

// render writes lines to the terminal.
//
// Each line is followed by an erase-to-end-of-line rather than clearing the
// whole screen first, so there is no blank frame between redraws and no
// flicker. Synchronised-output markers are emitted too: terminals that
// understand them present the frame atomically, and those that do not ignore
// them harmlessly.
func (s *screen) render(lines []string) error {
	n := len(lines)
	if n > s.height {
		n = s.height
	}

	s.frame.Reset()
	s.frame.WriteString("\x1b[?2026h\x1b[H")
	for i := range n {
		ln := lines[i]
		// Never touch the last cell of the last row. Filling it leaves the
		// cursor in the pending-wrap state, and terminals disagree about what
		// happens next — some scroll the whole screen up by one, which silently
		// eats the header. One unused column is a cheap price for a stable
		// frame.
		if i == n-1 {
			ln = trunc(ln, s.width-1)
		}
		s.frame.WriteString(ln)
		s.frame.WriteString("\x1b[K")
		if i < n-1 {
			s.frame.WriteString("\r\n")
		}
	}
	s.frame.WriteString("\x1b[J\x1b[?2026l")
	s.out.WriteString(s.frame.String())
	return s.out.Flush()
}

// ---------------------------------------------------------------------------
// input
// ---------------------------------------------------------------------------

// key is a decoded keypress.
type key struct {
	r    rune  // printable rune, when kind is keyRune
	kind kkind //
}

type kkind int

const (
	keyRune kkind = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyPgUp
	keyPgDn
	keyHome
	keyEnd
	keyEnter
	keyEsc
	keyBackspace
	keyTab
	keyCtrlC
)

// readKeys decodes stdin into key events until stdin closes or done is closed.
//
// Escape handling is the fiddly part: a lone ESC and the start of an arrow-key
// sequence are the same byte. Terminals emit a full sequence in a single
// write, so a buffered read that ends immediately after ESC is treated as the
// Escape key, and one with more bytes behind it is parsed as a sequence.
func readKeys(done <-chan struct{}) <-chan key {
	ch := make(chan key, 32)
	go func() {
		defer close(ch)
		buf := make([]byte, 256)
		in := os.Stdin
		for {
			n, err := in.Read(buf)
			if err != nil || n == 0 {
				return
			}
			for _, k := range decode(buf[:n]) {
				select {
				case ch <- k:
				case <-done:
					return
				}
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return ch
}

func decode(b []byte) []key {
	var out []key
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x03:
			out = append(out, key{kind: keyCtrlC})
			i++
		case c == '\r' || c == '\n':
			out = append(out, key{kind: keyEnter})
			i++
		case c == '\t':
			out = append(out, key{kind: keyTab})
			i++
		case c == 0x7f || c == 0x08:
			out = append(out, key{kind: keyBackspace})
			i++
		case c == 0x1b:
			k, used := decodeEscape(b[i:])
			out = append(out, k)
			i += used
		case c < 0x20:
			i++ // ignore other control bytes
		default:
			r, size := decodeRune(b[i:])
			out = append(out, key{kind: keyRune, r: r})
			i += size
		}
	}
	return out
}

func decodeEscape(b []byte) (key, int) {
	if len(b) == 1 {
		return key{kind: keyEsc}, 1
	}
	// CSI sequences: ESC [ ... final
	if b[1] == '[' {
		j := 2
		for j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == ';') {
			j++
		}
		if j >= len(b) {
			return key{kind: keyEsc}, len(b)
		}
		params := string(b[2:j])
		final := b[j]
		used := j + 1
		switch final {
		case 'A':
			return key{kind: keyUp}, used
		case 'B':
			return key{kind: keyDown}, used
		case 'C':
			return key{kind: keyRight}, used
		case 'D':
			return key{kind: keyLeft}, used
		case 'H':
			return key{kind: keyHome}, used
		case 'F':
			return key{kind: keyEnd}, used
		case '~':
			switch params {
			case "1", "7":
				return key{kind: keyHome}, used
			case "4", "8":
				return key{kind: keyEnd}, used
			case "5":
				return key{kind: keyPgUp}, used
			case "6":
				return key{kind: keyPgDn}, used
			}
		}
		return key{kind: keyEsc}, used
	}
	// ESC O x — application cursor mode
	if b[1] == 'O' && len(b) >= 3 {
		switch b[2] {
		case 'A':
			return key{kind: keyUp}, 3
		case 'B':
			return key{kind: keyDown}, 3
		case 'C':
			return key{kind: keyRight}, 3
		case 'D':
			return key{kind: keyLeft}, 3
		case 'H':
			return key{kind: keyHome}, 3
		case 'F':
			return key{kind: keyEnd}, 3
		}
		return key{kind: keyEsc}, 3
	}
	return key{kind: keyEsc}, 1
}

// decodeRune reads one UTF-8 rune without pulling in unicode/utf8's error
// rune semantics, which would render as a visible replacement character.
func decodeRune(b []byte) (rune, int) {
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c&0xE0 == 0xC0 && len(b) >= 2:
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case c&0xF0 == 0xE0 && len(b) >= 3:
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	case c&0xF8 == 0xF0 && len(b) >= 4:
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	}
	return rune(c), 1
}

// restoreOnce guards against leaving the terminal in raw mode when both a
// signal handler and the normal exit path try to clean up.
type restoreOnce struct {
	once sync.Once
	fn   func()
}

func (r *restoreOnce) do() {
	if r.fn != nil {
		r.once.Do(r.fn)
	}
}
