package main

import "unicode"

// editor is a single-line text field with a cursor, used for the message input, the
// command palette query and form fields. Pasted newlines are kept, so a multi-line
// paste becomes one multi-line message.
type editor struct {
	buf []rune
	pos int
}

func (e *editor) String() string { return string(e.buf) }
func (e *editor) empty() bool    { return len(e.buf) == 0 }

func (e *editor) set(s string) {
	e.buf = []rune(s)
	e.pos = len(e.buf)
}

func (e *editor) insert(s string) {
	rs := []rune(s)
	nb := make([]rune, 0, len(e.buf)+len(rs))
	nb = append(nb, e.buf[:e.pos]...)
	nb = append(nb, rs...)
	nb = append(nb, e.buf[e.pos:]...)
	e.buf = nb
	e.pos += len(rs)
}

func (e *editor) backspace() {
	if e.pos > 0 {
		e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
		e.pos--
	}
}

func (e *editor) del() {
	if e.pos < len(e.buf) {
		e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
	}
}

func (e *editor) left() {
	if e.pos > 0 {
		e.pos--
	}
}

func (e *editor) right() {
	if e.pos < len(e.buf) {
		e.pos++
	}
}

func (e *editor) home() { e.pos = 0 }
func (e *editor) end()  { e.pos = len(e.buf) }

func (e *editor) wordLeft() {
	for e.pos > 0 && unicode.IsSpace(e.buf[e.pos-1]) {
		e.pos--
	}
	for e.pos > 0 && !unicode.IsSpace(e.buf[e.pos-1]) {
		e.pos--
	}
}

func (e *editor) wordRight() {
	for e.pos < len(e.buf) && unicode.IsSpace(e.buf[e.pos]) {
		e.pos++
	}
	for e.pos < len(e.buf) && !unicode.IsSpace(e.buf[e.pos]) {
		e.pos++
	}
}

// deleteWord removes the word before the cursor (Ctrl-W).
func (e *editor) deleteWord() {
	end := e.pos
	e.wordLeft()
	e.buf = append(e.buf[:e.pos], e.buf[end:]...)
}

// killStart removes everything before the cursor (Ctrl-U).
func (e *editor) killStart() {
	e.buf = append([]rune(nil), e.buf[e.pos:]...)
	e.pos = 0
}

// key applies a key event; it reports whether the event was an editing key.
func (e *editor) key(ev event) bool {
	switch {
	case ev.key == keyRune && ev.ctrl:
		switch ev.r {
		case 'a':
			e.home()
		case 'e':
			e.end()
		case 'b':
			e.left()
		case 'f':
			e.right()
		case 'w', 'h':
			if ev.r == 'h' {
				e.backspace()
			} else {
				e.deleteWord()
			}
		case 'u':
			e.killStart()
		case 'd':
			e.del()
		default:
			return false
		}
	case ev.key == keyRune && ev.alt:
		switch ev.r {
		case 'b':
			e.wordLeft()
		case 'f':
			e.wordRight()
		default:
			return false
		}
	case ev.key == keyRune:
		e.insert(string(ev.r))
	case ev.key == keyBackspace:
		if ev.alt || ev.ctrl {
			e.deleteWord()
		} else {
			e.backspace()
		}
	case ev.key == keyDelete:
		e.del()
	case ev.key == keyLeft:
		if ev.ctrl || ev.alt {
			e.wordLeft()
		} else {
			e.left()
		}
	case ev.key == keyRight:
		if ev.ctrl || ev.alt {
			e.wordRight()
		} else {
			e.right()
		}
	case ev.key == keyHome:
		e.home()
	case ev.key == keyEnd:
		e.end()
	default:
		return false
	}
	return true
}
