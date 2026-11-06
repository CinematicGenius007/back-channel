package main

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A cell canvas for the app TUI: every frame is drawn into a grid of cells, turned into
// one string per row, and only rows that changed since the last frame are written to
// the terminal. That keeps output small over SSH and makes overlays (palette, forms)
// simple: they are just drawn last.

// colorV packs a colour: 0 = terminal default, 1<<24|n = palette index n (0-255),
// 2<<24|rgb = 24-bit colour.
type colorV uint32

const (
	colDefault colorV = 0
	colIndex   colorV = 1 << 24
	colRGB     colorV = 2 << 24
)

func hexColor(s string) colorV {
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	if err != nil {
		return colDefault
	}
	return colRGB | colorV(v&0xffffff)
}

func ansiColor(n int) colorV { return colIndex | colorV(n&0xff) }

type style struct {
	fg, bg                        colorV
	bold, dim, italic, under, rev bool
}

type cell struct {
	r  rune
	w  uint8 // 1 or 2; 0 marks the right half of a wide rune
	st style
}

type canvas struct {
	w, h  int
	cells []cell
}

func newCanvas(w, h int, base style) *canvas {
	cv := &canvas{w: w, h: h, cells: make([]cell, w*h)}
	for i := range cv.cells {
		cv.cells[i] = cell{r: ' ', w: 1, st: base}
	}
	return cv
}

func (cv *canvas) at(x, y int) *cell {
	if x < 0 || y < 0 || x >= cv.w || y >= cv.h {
		return nil
	}
	return &cv.cells[y*cv.w+x]
}

// fill paints a rectangle with spaces in style st.
func (cv *canvas) fill(x, y, w, h int, st style) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			if c := cv.at(xx, yy); c != nil {
				*c = cell{r: ' ', w: 1, st: st}
			}
		}
	}
}

// hline draws a horizontal rule.
func (cv *canvas) hline(x, y, w int, r rune, st style) {
	for xx := x; xx < x+w; xx++ {
		if c := cv.at(xx, y); c != nil {
			*c = cell{r: r, w: 1, st: st}
		}
	}
}

func (cv *canvas) vline(x, y, h int, r rune, st style) {
	for yy := y; yy < y+h; yy++ {
		if c := cv.at(x, yy); c != nil {
			*c = cell{r: r, w: 1, st: st}
		}
	}
}

// put writes one rune and returns its width (0 if it did not fit).
func (cv *canvas) put(x, y int, r rune, st style, limit int) int {
	w := runeWidth(r)
	if w == 0 || x+w > limit {
		return 0
	}
	c := cv.at(x, y)
	if c == nil {
		return 0
	}
	*c = cell{r: r, w: uint8(w), st: st}
	if w == 2 {
		if c2 := cv.at(x+1, y); c2 != nil {
			*c2 = cell{r: 0, w: 0, st: st}
		}
	}
	return w
}

// text writes plain text clipped to maxw cells; returns the width written.
func (cv *canvas) text(x, y, maxw int, s string, st style) int {
	limit := x + maxw
	if limit > cv.w {
		limit = cv.w
	}
	n := 0
	for _, r := range s {
		if r == '\n' || r == '\t' {
			r = ' '
		}
		w := cv.put(x+n, y, r, st, limit)
		if w == 0 && runeWidth(r) > 0 {
			break
		}
		n += w
	}
	return n
}

// textR writes s right-aligned so that it ends at column right (exclusive).
func (cv *canvas) textR(right, y, maxw int, s string, st style) int {
	w := strWidth(s)
	if w > maxw {
		w = maxw
	}
	return cv.text(right-w, y, w, s, st)
}

// ---- styled runs (text that carries SGR escapes, as the client formats messages) ----

type run struct {
	r  rune
	st style
}

// ansiRuns interprets SGR escape sequences in s on top of base. ANSI palette colours
// 0-15 go through pal so themes can re-colour what the client printed.
func ansiRuns(s string, base style, pal *[16]colorV) []run {
	var out []run
	st := base
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
				j++
			}
			if j < len(s) {
				if s[j] == 'm' {
					st = applySGR(st, base, s[i+2:j], pal)
				}
				i = j + 1
				continue
			}
			break
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r == '\t' {
			for k := 0; k < 4; k++ {
				out = append(out, run{' ', st})
			}
			continue
		}
		if r < 0x20 && r != '\n' {
			continue
		}
		out = append(out, run{r, st})
	}
	return out
}

func applySGR(st, base style, params string, pal *[16]colorV) style {
	if params == "" {
		return base
	}
	p := strings.Split(params, ";")
	for k := 0; k < len(p); k++ {
		n, _ := strconv.Atoi(p[k])
		switch {
		case n == 0:
			st = base
		case n == 1:
			st.bold = true
		case n == 2:
			st.dim = true
		case n == 22:
			st.bold, st.dim = false, false
		case n == 3:
			st.italic = true
		case n == 23:
			st.italic = false
		case n == 4:
			st.under = true
		case n == 24:
			st.under = false
		case n == 7:
			st.rev = true
		case n == 27:
			st.rev = false
		case n >= 30 && n <= 37:
			st.fg = palColor(pal, n-30)
		case n >= 90 && n <= 97:
			st.fg = palColor(pal, n-90+8)
		case n == 39:
			st.fg = base.fg
		case n >= 40 && n <= 47:
			st.bg = palColor(pal, n-40)
		case n >= 100 && n <= 107:
			st.bg = palColor(pal, n-100+8)
		case n == 49:
			st.bg = base.bg
		case n == 38 || n == 48:
			var c colorV
			if k+2 < len(p) && p[k+1] == "5" {
				v, _ := strconv.Atoi(p[k+2])
				c = ansiColor(v)
				k += 2
			} else if k+4 < len(p) && p[k+1] == "2" {
				r, _ := strconv.Atoi(p[k+2])
				g, _ := strconv.Atoi(p[k+3])
				b, _ := strconv.Atoi(p[k+4])
				c = colRGB | colorV(r&0xff)<<16 | colorV(g&0xff)<<8 | colorV(b&0xff)
				k += 4
			} else {
				continue
			}
			if n == 38 {
				st.fg = c
			} else {
				st.bg = c
			}
		}
	}
	return st
}

func palColor(pal *[16]colorV, i int) colorV {
	if pal != nil && pal[i] != colDefault {
		return pal[i]
	}
	return ansiColor(i)
}

// wrapRuns splits styled text into rows no wider than w cells. Explicit newlines start
// a new row; continuation rows are indented by indent cells.
func wrapRuns(rs []run, w, indent int) [][]run {
	if w < 4 {
		w = 4
	}
	if indent > w/2 {
		indent = 0
	}
	var rows [][]run
	var cur []run
	cw := 0
	newRow := func(cont bool) {
		rows = append(rows, cur)
		cur, cw = nil, 0
		if cont && indent > 0 {
			for k := 0; k < indent; k++ {
				cur = append(cur, run{' ', style{}})
			}
			cw = indent
		}
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r.r == '\n' {
			newRow(false)
			continue
		}
		rw := runeWidth(r.r)
		if cw+rw > w {
			// break at the last space in this row if there is a reasonable one
			brk := -1
			for k := len(cur) - 1; k > indent && k > len(cur)-24; k-- {
				if cur[k].r == ' ' {
					brk = k
					break
				}
			}
			if brk > 0 && r.r != ' ' {
				carry := append([]run(nil), cur[brk+1:]...)
				cur = cur[:brk]
				newRow(true)
				for _, c := range carry {
					cur = append(cur, c)
					cw += runeWidth(c.r)
				}
			} else {
				newRow(true)
				if r.r == ' ' {
					continue
				}
			}
		}
		cur = append(cur, r)
		cw += rw
	}
	rows = append(rows, cur)
	return rows
}

// drawRuns writes one wrapped row.
func (cv *canvas) drawRuns(x, y, maxw int, rs []run) int {
	limit := x + maxw
	if limit > cv.w {
		limit = cv.w
	}
	n := 0
	for _, r := range rs {
		if r.r == ' ' && r.st == (style{}) { // indent padding: keep the background underneath
			n++
			continue
		}
		w := cv.put(x+n, y, r.r, r.st, limit)
		if w == 0 && runeWidth(r.r) > 0 {
			break
		}
		n += w
	}
	return n
}

// ---- width ----------------------------------------------------------------------

// runeWidth approximates the number of terminal cells a rune occupies: 0 for combining
// marks and format characters, 2 for East Asian wide characters and most emoji.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	}
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
	}
	return 1
}

var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693},
	{0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C},
	{0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0},
	{0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
	{0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF},
	{0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE30, 0xFE4F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F200, 0x1F251}, {0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB},
	{0x1F90C, 0x1F9FF}, {0x1FA70, 0x1FAFF}, {0x20000, 0x3FFFD},
}

func strWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// clip shortens s to at most w cells, ending in "…" when cut.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if strWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return strings.Repeat("…", w)
	}
	n := 0
	var b strings.Builder
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > w-1 {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	return b.String() + "…"
}

func padRight(s string, w int) string {
	if d := w - strWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// ---- output ---------------------------------------------------------------------

// rowString renders row y with SGR codes for the given colour depth (24 or 256).
func (cv *canvas) rowString(y, depth int) string {
	var b strings.Builder
	var cur style
	first := true
	for x := 0; x < cv.w; x++ {
		c := cv.cells[y*cv.w+x]
		if c.w == 0 {
			continue
		}
		if first || c.st != cur {
			b.WriteString(sgr(c.st, depth))
			cur, first = c.st, false
		}
		b.WriteRune(c.r)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

func sgr(st style, depth int) string {
	var p []string
	p = append(p, "0")
	if st.bold {
		p = append(p, "1")
	}
	if st.dim {
		p = append(p, "2")
	}
	if st.italic {
		p = append(p, "3")
	}
	if st.under {
		p = append(p, "4")
	}
	if st.rev {
		p = append(p, "7")
	}
	if st.fg != colDefault {
		p = append(p, colorSGR(st.fg, depth, false))
	}
	if st.bg != colDefault {
		p = append(p, colorSGR(st.bg, depth, true))
	}
	return "\x1b[" + strings.Join(p, ";") + "m"
}

func colorSGR(c colorV, depth int, bg bool) string {
	kind := c & 0xff000000
	v := int(c & 0xffffff)
	if kind == colIndex {
		switch {
		case v < 8 && !bg:
			return strconv.Itoa(30 + v)
		case v < 8:
			return strconv.Itoa(40 + v)
		case v < 16 && !bg:
			return strconv.Itoa(90 + v - 8)
		case v < 16:
			return strconv.Itoa(100 + v - 8)
		}
		if bg {
			return "48;5;" + strconv.Itoa(v)
		}
		return "38;5;" + strconv.Itoa(v)
	}
	r, g, b := v>>16&0xff, v>>8&0xff, v&0xff
	if depth >= 24 {
		if bg {
			return "48;2;" + strconv.Itoa(r) + ";" + strconv.Itoa(g) + ";" + strconv.Itoa(b)
		}
		return "38;2;" + strconv.Itoa(r) + ";" + strconv.Itoa(g) + ";" + strconv.Itoa(b)
	}
	n := rgbTo256(r, g, b)
	if bg {
		return "48;5;" + strconv.Itoa(n)
	}
	return "38;5;" + strconv.Itoa(n)
}

// rgbTo256 picks the nearest colour in xterm's 6×6×6 cube or 24-step grey ramp.
func rgbTo256(r, g, b int) int {
	q := func(v int) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (v - 35) / 40
	}
	lv := []int{0, 95, 135, 175, 215, 255}
	cr, cg, cb := q(r), q(g), q(b)
	cube := 16 + 36*cr + 6*cg + cb
	dc := sq(lv[cr]-r) + sq(lv[cg]-g) + sq(lv[cb]-b)
	avg := (r + g + b) / 3
	gi := (avg - 3) / 10
	if gi < 0 {
		gi = 0
	}
	if gi > 23 {
		gi = 23
	}
	gv := 8 + gi*10
	dg := sq(gv-r) + sq(gv-g) + sq(gv-b)
	if dg < dc {
		return 232 + gi
	}
	return cube
}

func sq(v int) int { return v * v }
