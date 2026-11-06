package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Raw terminal input → events. Every platform (Windows included, via virtual-terminal
// input) sends the same byte sequences, so this file is the only input parser.
//
// Touch screens reach a terminal as mouse reports: a tap is a press + release, a
// swipe is a press, a run of motion reports and a release, and some mobile terminals
// turn a swipe into wheel reports instead. The app treats all of these the same way.

type evKind int

const (
	evKey evKind = iota
	evMouse
	evPaste
	evFocus // the terminal window gained or lost focus (mode 1004)
)

type keyCode int

const (
	keyRune keyCode = iota
	keyEnter
	keyTab
	keyBacktab
	keyBackspace
	keyEsc
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPgUp
	keyPgDn
	keyDelete
	keyInsert
	keyF1
	keyF2
	keyF3
	keyF4
	keyF5
	keyF6
	keyF7
	keyF8
	keyF9
	keyF10
	keyF11
	keyF12
)

type mouseAct int

const (
	mPress mouseAct = iota
	mRelease
	mDrag
	mWheelUp
	mWheelDown
)

type event struct {
	kind  evKind
	key   keyCode
	r     rune // keyRune; with ctrl set, the lowercase letter ('c' for Ctrl-C)
	ctrl  bool
	alt   bool
	shift bool

	act    mouseAct
	button int // 0 left, 1 middle, 2 right
	x, y   int // 0-based cell

	text  string // evPaste
	focus bool   // evFocus: true = gained
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// parseInput turns buf into events. Bytes that might be the start of a longer
// sequence are returned in rest; call again with more input, or with final=true
// once the terminal has been quiet for a moment (that is how a lone Esc is told
// apart from the start of an arrow-key sequence).
func parseInput(buf []byte, final bool) (evs []event, rest []byte) {
	i := 0
	for i < len(buf) {
		b := buf[i]
		switch {
		case b == 0x1b:
			ev, n, ok := parseEsc(buf[i:], final)
			if !ok {
				return evs, buf[i:]
			}
			if n == 0 { // unrecognised sequence: skip the ESC byte
				i++
				continue
			}
			if ev != nil {
				evs = append(evs, *ev)
			}
			i += n
		case b == '\r' || b == '\n':
			evs = append(evs, event{kind: evKey, key: keyEnter})
			i++
		case b == '\t':
			evs = append(evs, event{kind: evKey, key: keyTab})
			i++
		case b == 0x7f || b == 0x08:
			evs = append(evs, event{kind: evKey, key: keyBackspace})
			i++
		case b == 0x00:
			evs = append(evs, event{kind: evKey, key: keyRune, r: ' ', ctrl: true})
			i++
		case b < 0x20:
			evs = append(evs, event{kind: evKey, key: keyRune, r: rune('a' + b - 1), ctrl: true})
			i++
		default:
			r, n := utf8.DecodeRune(buf[i:])
			if r == utf8.RuneError && n <= 1 {
				if !final && !utf8.FullRune(buf[i:]) {
					return evs, buf[i:]
				}
				i++
				continue
			}
			evs = append(evs, event{kind: evKey, key: keyRune, r: r})
			i += n
		}
	}
	return evs, nil
}

// parseEsc parses one sequence starting with ESC. ok=false means "need more bytes";
// n=0 with ok=true means "not a sequence we know, skip the ESC".
func parseEsc(b []byte, final bool) (ev *event, n int, ok bool) {
	if len(b) == 1 {
		if final {
			return &event{kind: evKey, key: keyEsc}, 1, true
		}
		return nil, 0, false
	}
	switch b[1] {
	case '[':
		return parseCSI(b, final)
	case 'O': // SS3: application-mode arrows and F1-F4
		if len(b) < 3 {
			if final {
				return &event{kind: evKey, key: keyRune, r: 'O', alt: true}, 2, true
			}
			return nil, 0, false
		}
		k, known := map[byte]keyCode{'A': keyUp, 'B': keyDown, 'C': keyRight, 'D': keyLeft, 'H': keyHome, 'F': keyEnd, 'P': keyF1, 'Q': keyF2, 'R': keyF3, 'S': keyF4}[b[2]]
		if !known {
			return nil, 3, true
		}
		return &event{kind: evKey, key: k}, 3, true
	case 0x1b: // Esc Esc: report one Esc, let the second start over
		return &event{kind: evKey, key: keyEsc}, 1, true
	}
	// Alt+key: ESC followed by a normal key.
	rest, _ := parseInput(b[1:2], true)
	if len(b) >= 2 && b[1] >= 0x80 {
		r, size := utf8.DecodeRune(b[1:])
		if r == utf8.RuneError && !utf8.FullRune(b[1:]) && !final {
			return nil, 0, false
		}
		return &event{kind: evKey, key: keyRune, r: r, alt: true}, 1 + size, true
	}
	if len(rest) == 1 {
		e := rest[0]
		e.alt = true
		return &e, 2, true
	}
	return nil, 0, true
}

func parseCSI(b []byte, final bool) (*event, int, bool) {
	// bracketed paste: everything up to the end marker is literal text
	if strings.HasPrefix(string(b), pasteStart) {
		end := strings.Index(string(b[len(pasteStart):]), pasteEnd)
		if end < 0 {
			if final && len(b) > 1<<20 { // runaway paste without an end marker
				return &event{kind: evPaste, text: string(b[len(pasteStart):])}, len(b), true
			}
			return nil, 0, false
		}
		txt := string(b[len(pasteStart) : len(pasteStart)+end])
		return &event{kind: evPaste, text: txt}, len(pasteStart) + end + len(pasteEnd), true
	}
	if len(pasteStart) > len(b) && strings.HasPrefix(pasteStart, string(b)) && !final {
		return nil, 0, false
	}
	j := 2
	for j < len(b) && b[j] >= 0x20 && b[j] <= 0x3f {
		j++
	}
	if j >= len(b) {
		if final {
			return nil, len(b), true
		}
		return nil, 0, false
	}
	fin := b[j]
	if fin < 0x40 || fin > 0x7e {
		return nil, j, true
	}
	params := string(b[2:j])
	n := j + 1

	if strings.HasPrefix(params, "<") && (fin == 'M' || fin == 'm') {
		return parseSGRMouse(params[1:], fin == 'M'), n, true
	}
	if params == "" && (fin == 'I' || fin == 'O') {
		return &event{kind: evFocus, focus: fin == 'I'}, n, true
	}
	parts := strings.Split(params, ";")
	num := func(i int) int {
		if i < len(parts) {
			v, _ := strconv.Atoi(parts[i])
			return v
		}
		return 0
	}
	mod := num(1)
	ev := &event{kind: evKey}
	if mod > 1 {
		m := mod - 1
		ev.shift, ev.alt, ev.ctrl = m&1 != 0, m&2 != 0, m&4 != 0
	}
	switch fin {
	case 'A':
		ev.key = keyUp
	case 'B':
		ev.key = keyDown
	case 'C':
		ev.key = keyRight
	case 'D':
		ev.key = keyLeft
	case 'H':
		ev.key = keyHome
	case 'F':
		ev.key = keyEnd
	case 'Z':
		ev.key = keyBacktab
	case 'P', 'Q', 'R', 'S':
		ev.key = keyF1 + keyCode(fin-'P')
	case '~':
		k, known := map[int]keyCode{1: keyHome, 7: keyHome, 4: keyEnd, 8: keyEnd, 2: keyInsert, 3: keyDelete, 5: keyPgUp, 6: keyPgDn,
			11: keyF1, 12: keyF2, 13: keyF3, 14: keyF4, 15: keyF5, 17: keyF6, 18: keyF7, 19: keyF8, 20: keyF9, 21: keyF10, 23: keyF11, 24: keyF12}[num(0)]
		if !known {
			return nil, n, true
		}
		ev.key = k
	default: // cursor position replies and the like: ignore
		return nil, n, true
	}
	return ev, n, true
}

// parseSGRMouse decodes "b;x;y" from ESC [ < b ; x ; y M|m (1-based coordinates).
func parseSGRMouse(p string, press bool) *event {
	f := strings.Split(p, ";")
	if len(f) != 3 {
		return nil
	}
	cb, _ := strconv.Atoi(f[0])
	x, _ := strconv.Atoi(f[1])
	y, _ := strconv.Atoi(f[2])
	ev := &event{kind: evMouse, x: x - 1, y: y - 1, button: cb & 3,
		shift: cb&4 != 0, alt: cb&8 != 0, ctrl: cb&16 != 0}
	switch {
	case cb&64 != 0:
		if cb&1 == 0 {
			ev.act = mWheelUp
		} else {
			ev.act = mWheelDown
		}
		if cb&2 != 0 { // horizontal wheel: ignore
			return nil
		}
		ev.button = 0
	case cb&32 != 0:
		ev.act = mDrag
	case press:
		ev.act = mPress
	default:
		ev.act = mRelease
	}
	return ev
}
