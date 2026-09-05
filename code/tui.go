package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync"
)

// tui is an IRC-style layout built purely from VT escape codes:
//
//	rows 1..rows-4  scroll region (messages of the active channel)
//	row  rows-3     channel bar   " 1:#general  2:#design(3)  3:#ops "
//	row  rows-2     status bar (inverse video)
//	row  rows-1     input line "> "
//	row  rows       spare line so the terminal's own Enter/LF never scrolls the screen
//
// Input is read in the terminal's normal (cooked) mode, so it works identically on
// macOS Terminal/iTerm, Windows Terminal, and Linux without any raw-mode dependency.
// The one concession is turning echo off for password prompts (term_*.go).
type tui struct {
	mu     sync.Mutex
	rows   int
	cols   int
	status string
	bar    string
	prompt string
	out    *os.File
}

func newTUI() *tui { return &tui{out: os.Stdout, rows: 24, cols: 80, prompt: "> "} }

func (t *tui) init() {
	enableVT()
	t.rows, t.cols = termSize()
	if t.rows < 8 {
		t.rows = 24
	}
	if t.cols < 20 {
		t.cols = 80
	}
	fmt.Fprint(t.out, "\x1b[?1049h") // alternate screen
	t.layout()
}

func (t *tui) top() int { return t.rows - 4 } // last row of the scroll region

func (t *tui) layout() {
	fmt.Fprint(t.out, "\x1b[2J")              // clear
	fmt.Fprintf(t.out, "\x1b[1;%dr", t.top()) // scroll region
	t.drawBar()
	t.drawStatus()
	fmt.Fprintf(t.out, "\x1b[%d;1H\x1b[2K%s", t.rows-1, t.prompt) // input prompt
}

func (t *tui) cleanup() {
	fmt.Fprint(t.out, "\x1b[r\x1b[?1049l")
}

func (t *tui) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.layout()
}

func (t *tui) checkResize() {
	r, c := termSize()
	if r < 8 || c < 20 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if r != t.rows || c != t.cols {
		t.rows, t.cols = r, c
		t.layout()
	}
}

// print writes a (possibly multi-line, ANSI-coloured) message into the scroll region
// without disturbing whatever the user is typing on the input line.
func (t *tui) print(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	b.WriteString("\x1b7")                            // save cursor
	b.WriteString(fmt.Sprintf("\x1b[%d;1H", t.top())) // bottom of scroll region
	for _, line := range strings.Split(s, "\n") {
		for _, w := range wrap(line, t.cols) {
			b.WriteString("\n\x1b[2K" + w + "\x1b[0m")
		}
	}
	b.WriteString("\x1b8") // restore cursor
	fmt.Fprint(t.out, b.String())
}

// redraw replaces the scroll region with the tail of `lines` (used on channel switch).
func (t *tui) redraw(lines []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var rendered []string
	for _, l := range lines {
		for _, part := range strings.Split(l, "\n") {
			rendered = append(rendered, wrap(part, t.cols)...)
		}
	}
	if n := t.top(); len(rendered) > n {
		rendered = rendered[len(rendered)-n:]
	}
	var b strings.Builder
	b.WriteString("\x1b7")
	for r := 1; r <= t.top(); r++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", r)
	}
	start := t.top() - len(rendered) + 1
	for i, l := range rendered {
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[0m", start+i, l)
	}
	b.WriteString("\x1b8")
	fmt.Fprint(t.out, b.String())
}

func (t *tui) setStatus(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s == "" && t.status != "" && !strings.HasPrefix(t.status, "connected") {
		s = "connected"
	}
	if s != "" {
		t.status = s
	}
	t.drawStatus()
}

func (t *tui) drawStatus() {
	s := " " + t.status
	if visLen(s) > t.cols {
		s = truncVis(s, t.cols)
	}
	pad := t.cols - visLen(s)
	fmt.Fprintf(t.out, "\x1b7\x1b[%d;1H\x1b[7m%s%s\x1b[0m\x1b8", t.rows-2, s, strings.Repeat(" ", pad))
}

func (t *tui) setBar(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bar = s
	t.drawBar()
}

func (t *tui) drawBar() {
	s := t.bar
	if visLen(s) > t.cols {
		s = truncVis(s, t.cols)
	}
	fmt.Fprintf(t.out, "\x1b7\x1b[%d;1H\x1b[2K%s\x1b[0m\x1b8", t.rows-3, s)
}

// setPrompt changes the input-line prompt (e.g. for a password question).
func (t *tui) setPrompt(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prompt = p
	fmt.Fprintf(t.out, "\x1b[%d;1H\x1b[2K%s", t.rows-1, t.prompt)
}

// resetInput is called after the user presses Enter: clear the echoed line and re-prompt.
func (t *tui) resetInput() {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.out, "\x1b[%d;1H\x1b[2K\x1b[%d;1H\x1b[2K%s", t.rows, t.rows-1, t.prompt)
}

func (t *tui) bell() { fmt.Fprint(t.out, "\a") }

// ---- ANSI helpers -----------------------------------------------------------

func dim(s string) string  { return "\x1b[2m" + s + "\x1b[22m" }
func bold(s string) string { return "\x1b[1m" + s + "\x1b[22m" }

var nickColors = []string{"\x1b[36m", "\x1b[32m", "\x1b[35m", "\x1b[34m", "\x1b[91m", "\x1b[96m", "\x1b[92m", "\x1b[95m"}

func colorNick(n string) string {
	h := fnv.New32a()
	h.Write([]byte(n))
	return nickColors[int(h.Sum32())%len(nickColors)] + "\x1b[1m" + n + "\x1b[0m"
}

// visLen counts printable runes, ignoring ANSI escape sequences.
func visLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' || r == 'H' || r == 'J' || r == 'K' {
				esc = false
			}
		case r == 0x1b:
			esc = true
		default:
			n++
		}
	}
	return n
}

func truncVis(s string, w int) string {
	var b strings.Builder
	n, esc := 0, false
	for _, r := range s {
		if esc {
			b.WriteRune(r)
			if r == 'm' {
				esc = false
			}
			continue
		}
		if r == 0x1b {
			esc = true
			b.WriteRune(r)
			continue
		}
		if n >= w {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// wrap splits a line into chunks no wider than w visible cells (escapes carried along).
func wrap(s string, w int) []string {
	if w < 10 {
		w = 10
	}
	if visLen(s) <= w {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	n, esc := 0, false
	for _, r := range s {
		if esc {
			cur.WriteRune(r)
			if r == 'm' {
				esc = false
			}
			continue
		}
		if r == 0x1b {
			esc = true
			cur.WriteRune(r)
			continue
		}
		if n >= w-1 {
			out = append(out, cur.String())
			cur.Reset()
			cur.WriteString("        ") // continuation indent
			n = 8
		}
		cur.WriteRune(r)
		n++
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
