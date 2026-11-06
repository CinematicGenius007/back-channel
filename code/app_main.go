package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Drawing and keys for the main screen, overlays (palette, channel switcher, help,
// forms) and the frame loop. Manage and setup live in app_manage.go / app_setup.go.

func (a *app) render() {
	a.hits = a.hits[:0]
	cur := [2]int{-1, -1}
	cv := newCanvas(a.w, a.h, a.stBase())
	if a.w < 24 || a.h < 8 {
		cv.text(0, 0, a.w, "window too small", a.stMuted())
	} else {
		switch a.mode {
		case scrSetup:
			cur = a.drawSetup(cv)
		case scrManage:
			a.drawManage(cv)
		default:
			cur = a.drawMain(cv)
		}
		switch a.ov {
		case ovPalette:
			cur = a.drawPalette(cv)
		case ovChannels:
			a.drawChannelPicker(cv)
		case ovHelp:
			a.drawHelp(cv)
		case ovForm:
			cur = a.drawForm(cv)
		}
	}
	var b strings.Builder
	b.WriteString("\x1b[?25l")
	rows := make([]string, a.h)
	for y := 0; y < a.h; y++ {
		rows[y] = cv.rowString(y, a.depth)
		if a.prev == nil || y >= len(a.prev) || a.prev[y] != rows[y] {
			fmt.Fprintf(&b, "\x1b[%d;1H%s", y+1, rows[y])
		}
	}
	if cur[0] >= 0 {
		fmt.Fprintf(&b, "\x1b[%d;%dH\x1b[?25h", cur[1]+1, cur[0]+1)
	}
	io.WriteString(a.out, b.String())
	a.prev = rows
	a.dirty = false
}

// ---- layout helpers ------------------------------------------------------------------

func (a *app) sideWidth() int {
	switch {
	case a.w >= 120:
		return 26
	case a.w >= 64:
		return 22
	}
	return 0
}

func (a *app) infoWidth() int {
	if a.w >= 110 {
		return 30
	}
	return 0
}

// button draws a tappable label and returns its width.
func (a *app) button(cv *canvas, x, y int, label string, st style, fn func()) int {
	w := cv.text(x, y, a.w-x, label, st)
	if w > 0 {
		a.onHit(x, y, w, 1, fn)
	}
	return w
}

func (a *app) drawTitle(cv *canvas, crumb string) {
	cv.fill(0, 0, a.w, 1, a.stBase())
	x := 1
	if a.mode == scrMain && a.sideWidth() == 0 && a.c != nil && a.c.active != nil {
		x += a.button(cv, x, 0, "≡ #"+a.c.active.name, style{fg: a.th.ink, bg: a.th.bg, bold: true}, func() { a.ov = ovChannels; a.psel = a.activeIndex() })
	} else {
		x += cv.text(x, 0, a.w, "BACKCHANNEL", style{fg: a.th.ink, bg: a.th.bg, bold: true})
		x += cv.text(x, 0, a.w, " / BCH", a.stMuted())
	}
	right := a.w - 1
	if a.c != nil {
		dot, label, st := "○", "offline", a.stMuted()
		if a.c.online.Load() {
			dot, label, st = "●", "online", style{fg: a.th.sigText, bg: a.th.bg}
		}
		right -= cv.textR(right, 0, 12, dot+" "+label, st) + 2
	}
	if crumb != "" && right-x > 8 {
		cv.textR(right, 0, right-x-2, clip(crumb, right-x-2), a.stMuted())
	}
	if a.h >= 14 {
		cv.hline(0, 1, a.w, '─', a.stRule())
	}
}

func (a *app) bodyTop() int {
	if a.h >= 14 {
		return 2
	}
	return 1
}

// drawKeybar is the bottom row: large tap targets for the things a keyboard user
// reaches with shortcuts.
func (a *app) drawKeybar(cv *canvas, y int, items []kbItem) {
	cv.fill(0, y, a.w, 1, a.stBase())
	x := 1
	for _, it := range items {
		label := it.label
		if a.w < 70 && it.short != "" {
			label = it.short
		}
		if x+strWidth(label)+3 > a.w {
			break
		}
		st := style{fg: a.th.ink, bg: a.th.panel}
		if a.th.panel == colDefault {
			st = style{rev: true}
		}
		w := a.button(cv, x, y, " "+label+" ", st, it.fn)
		if it.key != "" && a.w >= 90 {
			w += cv.text(x+w, y, a.w, " "+it.key, a.stMuted())
		}
		x += w + 2
	}
}

type kbItem struct {
	label, short, key string
	fn                func()
}

func (a *app) mainKeybar() []kbItem {
	return []kbItem{
		{"≡ Channels", "≡", "", func() { a.ov = ovChannels; a.psel = a.activeIndex() }},
		{"⌘ Commands", "⌘", "^K", a.openPalette},
		{"⚙ Manage", "⚙", "F2", func() { a.openManage(a.msec) }},
		{"? Help", "?", "F1", func() { a.ov, a.help = ovHelp, 0 }},
		{"↵ Send", "↵", "", func() {
			if a.c != nil && !a.in.empty() {
				line := a.in.String()
				a.in.set("")
				a.submit(line)
			}
		}},
	}
}

// ---- main screen -------------------------------------------------------------------------

func (a *app) drawMain(cv *canvas) [2]int {
	c := a.c
	cfg := c.snapshot()
	who := c.me
	if who == "" {
		who = cfg.Name
	}
	crumb := who + "@" + cfg.Hub
	if cfg.TLS {
		crumb += " · TLS"
		if cfg.FP != "" {
			crumb += " pinned"
		}
	}
	a.drawTitle(cv, crumb)
	top := a.bodyTop()
	statusY, inputY, barY := a.h-3, a.h-2, a.h-1
	bodyH := statusY - top
	side, info := a.sideWidth(), a.infoWidth()

	if side > 0 {
		a.drawSidebar(cv, 0, top, side, bodyH)
		cv.vline(side, top, bodyH, '│', a.stRule())
	}
	if info > 0 {
		x := a.w - info
		cv.vline(x-1, top, bodyH, '│', a.stRule())
		a.drawInfo(cv, x, top, info, bodyH)
	}
	mx := 0
	if side > 0 {
		mx = side + 1
	}
	mw := a.w - mx
	if info > 0 {
		mw -= info + 1
	}
	a.drawMessages(cv, mx+1, top, mw-2, bodyH)

	// status line
	stl := a.stPanel()
	if a.th.panel == colDefault {
		stl = style{rev: true}
	}
	cv.fill(0, statusY, a.w, 1, stl)
	hint := "PgUp/PgDn scroll · F1 help"
	if a.mouse && a.w >= 100 {
		hint = "Shift+drag selects text · " + hint
	}
	hw := 0
	if a.w >= 70 {
		hw = cv.textR(a.w-1, statusY, a.w/2, hint, style{fg: a.th.muted, bg: stl.bg, rev: stl.rev})
	}
	cv.text(1, statusY, a.w-hw-3, clip(a.status, a.w-hw-3), style{fg: a.th.copy, bg: stl.bg, rev: stl.rev})

	cur := a.drawInput(cv, inputY)
	a.drawKeybar(cv, barY, a.mainKeybar())
	if a.ov == ovNone {
		a.drawSuggestions(cv, inputY)
	}
	return cur
}

func (a *app) activeIndex() int {
	for i, ch := range a.c.chans {
		if ch == a.c.active {
			return i
		}
	}
	return 0
}

func (a *app) drawSidebar(cv *canvas, x, y, w, h int) {
	c := a.c
	cv.text(x+2, y, w-2, "CHANNELS", a.stMuted())
	row := y + 1
	maxRow := y + h - 3
	for i, ch := range c.chans {
		if ch.name == "…" {
			continue
		}
		if row >= maxRow {
			cv.text(x+2, row, w-2, fmt.Sprintf("+%d more (≡)", len(c.chans)-i), a.stMuted())
			row++
			break
		}
		ch := ch
		st := a.stCopy()
		if ch == c.active {
			st = a.stSel()
			cv.fill(x, row, w, 1, st)
			cv.text(x, row, 1, "▌", style{fg: a.th.sig, bg: st.bg})
		} else if ch.unread > 0 {
			st = style{fg: a.th.ink, bg: a.th.bg, bold: true}
		}
		right := x + w - 1
		if ch.unread > 0 && ch != c.active {
			n := strconv.Itoa(ch.unread)
			if ch.unread > 99 {
				n = "99+"
			}
			right -= cv.textR(right, row, 5, " "+n+" ", a.stBadge()) + 1
		}
		if ch.e2e {
			right -= cv.textR(right, row, 4, "e2e", style{fg: a.th.muted, bg: st.bg, rev: st.rev}) + 1
		}
		label := "#" + ch.name
		if i < 9 {
			label = strconv.Itoa(i+1) + " #" + ch.name
		}
		cv.text(x+2, row, right-x-2, clip(label, right-x-2), st)
		a.onHit(x, row, w, 1, func() { c.switchTo(ch) })
		row++
	}
	a.button(cv, x+2, y+h-2, "+ new channel", a.stMuted(), func() { a.openForm(newChannelForm()) })
	a.button(cv, x+2, y+h-1, "⚙ manage", a.stMuted(), func() { a.openManage(0) })
}

func (a *app) drawInfo(cv *canvas, x, y, w, h int) {
	c := a.c
	ch := c.active
	row := y
	label := func(s string) {
		if row < y+h {
			cv.text(x+1, row, w-2, s, a.stMuted())
			row++
		}
	}
	kv := func(k, v string) {
		if row < y+h {
			cv.text(x+1, row, 10, k, a.stMuted())
			cv.text(x+11, row, w-12, clip(v, w-12), a.stCopy())
			row++
		}
	}
	if ch != nil && ch.name != "…" {
		label("HERE NOW")
		if ch.online == nil {
			cv.text(x+1, row, w-2, "…", a.stMuted())
			row++
		}
		names := append([]string(nil), ch.online...)
		sort.Strings(names)
		for i, n := range names {
			if row >= y+h/2 {
				cv.text(x+1, row, w-2, fmt.Sprintf("+%d more", len(names)-i), a.stMuted())
				row++
				break
			}
			cv.text(x+1, row, 2, "●", style{fg: a.th.sig, bg: a.th.bg})
			st := a.stCopy()
			if c.isMe(n) {
				st = style{fg: a.th.ink, bg: a.th.bg, bold: true}
			}
			cv.text(x+3, row, w-4, clip(n, w-4), st)
			row++
		}
		row++
		label("THIS CHANNEL")
		if ch.role != "" {
			kv("role", ch.role)
		}
		if ch.members > 0 {
			kv("members", strconv.Itoa(ch.members))
		}
		if ch.e2e {
			kv("e2e", fmt.Sprintf("on · epoch %d", ch.epoch))
		} else {
			kv("e2e", "off")
		}
		if ch.expire > 0 {
			kv("expires", fmtExpire(ch.expire))
		}
		if ch.readonly {
			kv("mode", "read-only")
		}
		if c.watched(ch.name) {
			kv("files", "always download")
		}
		row++
	}
	label("THIS DEVICE")
	cfg := c.snapshot()
	kv("name", cfg.Name)
	if cfg.E2EPub != "" {
		kv("key", keyFingerprint(cfg.E2EPub))
	}
	if c.clipOn.Load() {
		kv("clipboard", "sync on")
	} else {
		kv("clipboard", "sync off")
	}
}

// msgRows wraps the active channel's scrollback to width w.
func (a *app) msgRows(w int) [][]run {
	ch := a.c.active
	if ch == nil {
		return nil
	}
	var rows [][]run
	for _, l := range ch.lines {
		rows = append(rows, wrapRuns(ansiRuns(l.text, a.stBase(), a.pal), w, 6)...)
	}
	return rows
}

func (a *app) maxScroll() int {
	if m := a.msgTotal - a.paneRows; m > 0 {
		return m
	}
	return 0
}

func (a *app) drawMessages(cv *canvas, x, y, w, h int) {
	c := a.c
	ch := c.active
	if ch != nil && ch.name != "…" {
		cv.text(x, y, w, "#"+ch.name, style{fg: a.th.ink, bg: a.th.bg, bold: true})
		nx := x + strWidth(ch.name) + 1
		var flags []string
		if ch.e2e {
			flags = append(flags, "e2e")
		}
		if ch.expire > 0 {
			flags = append(flags, "expires "+fmtExpire(ch.expire))
		}
		if ch.readonly {
			flags = append(flags, "read-only")
		}
		fw := 0
		if len(flags) > 0 {
			fw = cv.textR(x+w, y, w/2, strings.Join(flags, " · "), a.stMuted())
		}
		if ch.topic != "" && x+w-fw-nx > 6 {
			cv.text(nx, y, x+w-fw-nx-2, clip(" · "+ch.topic, x+w-fw-nx-2), a.stMuted())
		}
		cv.hline(x, y+1, w, '┄', a.stRule())
		y += 2
		h -= 2
	}
	a.paneX, a.paneY, a.paneW, a.paneRows = x-1, y, w+2, h
	rows := a.msgRows(w)
	a.msgTotal = len(rows)
	a.clampScroll()
	end := len(rows) - a.scroll
	start := end - h
	if start < 0 {
		start = 0
	}
	off := h - (end - start) // anchor to the bottom
	for i := start; i < end; i++ {
		cv.drawRuns(x, y+off+i-start, w, rows[i])
	}
	if a.scroll > 0 {
		label := "↓ latest"
		if a.unseen > 0 {
			label = fmt.Sprintf("↓ %d new", a.unseen)
		}
		label = " " + label + " "
		bx := x + w - strWidth(label)
		a.button(cv, bx, y+h-1, label, a.stBadge(), func() { a.scroll, a.unseen = 0, 0 })
	}
}

func (a *app) drawInput(cv *canvas, y int) [2]int {
	cv.fill(0, y, a.w, 1, a.stBase())
	p := a.prompt
	pw := cv.text(1, y, a.w-2, p, a.stSig())
	x := 1 + pw
	avail := a.w - x - 1
	text := a.in.buf
	pos := a.in.pos
	if a.hidden {
		masked := make([]rune, len(text))
		for i := range masked {
			masked[i] = '•'
		}
		text = masked
	}
	if len(text) == 0 && !a.hidden && a.c != nil {
		ph := "message"
		if ch := a.c.active; ch != nil && ch.name != "…" {
			ph += " #" + ch.name
		}
		ph += " — / for commands, drag a file in to send it"
		cv.text(x, y, avail, clip(ph, avail), a.stMuted())
		return [2]int{x, y}
	}
	disp := make([]rune, len(text))
	for i, r := range text {
		if r == '\n' {
			r = '⏎'
		}
		disp[i] = r
	}
	// scroll horizontally so the cursor stays visible
	start := 0
	for strWidth(string(disp[start:pos])) > avail-1 && start < pos {
		start++
	}
	cv.text(x, y, avail, string(disp[start:]), a.stBase())
	return [2]int{x + strWidth(string(disp[start:pos])), y}
}

// suggestions shows matching commands while the input is "/word" with no space yet.
func (a *app) suggestions() []cmdInfo {
	s := a.in.String()
	if a.hidden || !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \n") {
		return nil
	}
	m := matchCommands(s[1:])
	if len(m) == 1 && "/"+m[0].name == s {
		return nil
	}
	return m
}

func (a *app) drawSuggestions(cv *canvas, inputY int) {
	sug := a.suggestions()
	if len(sug) == 0 {
		return
	}
	n := len(sug)
	if n > 8 {
		n = 8
	}
	if n > inputY-a.bodyTop()-1 {
		n = inputY - a.bodyTop() - 1
	}
	if a.sugSel >= len(sug) {
		a.sugSel = len(sug) - 1
	}
	first := 0
	if a.sugSel >= n {
		first = a.sugSel - n + 1
	}
	w := a.w - 2
	if w > 90 {
		w = 90
	}
	y0 := inputY - 1 - n
	for i := 0; i < n; i++ {
		k := first + i
		s := sug[k]
		st := a.stPanel()
		if a.th.panel == colDefault {
			st = style{}
		}
		if k == a.sugSel {
			st = a.stSel()
		}
		y := y0 + i
		cv.fill(1, y, w, 1, st)
		nx := cv.text(2, y, w-2, "/"+s.name, style{fg: a.th.sigText, bg: st.bg, bold: true, rev: st.rev})
		if s.args != "" {
			nx += cv.text(2+nx, y, w-nx-2, " "+s.args, style{fg: a.th.copy, bg: st.bg, rev: st.rev})
		}
		if w-nx-6 > 10 {
			cv.textR(1+w-1, y, w-nx-6, clip(s.desc, w-nx-6), style{fg: a.th.muted, bg: st.bg, rev: st.rev})
		}
		a.onHit(1, y, w, 1, func() { a.pickSuggestion(s, true) })
	}
	cv.fill(1, inputY-1, w, 1, a.stBase())
	cv.text(2, inputY-1, w-2, "↑↓ choose · Tab complete · Enter run", a.stMuted())
}

func (a *app) pickSuggestion(s cmdInfo, run bool) {
	if s.args == "" && run {
		a.in.set("")
		a.submit("/" + s.name)
		return
	}
	a.in.set("/" + s.name + " ")
	a.sugSel = 0
}

// ---- keys ---------------------------------------------------------------------------------

func (a *app) handle(ev event) {
	a.dirty = true
	if ev.kind == evMouse {
		if a.mouse {
			a.handleMouse(ev)
		}
		return
	}
	if ev.kind == evKey && ev.key == keyRune && ev.ctrl {
		switch ev.r {
		case 'c':
			switch {
			case a.ov != ovNone:
				a.closeOverlay()
			case a.mode == scrManage:
				a.mode = scrMain
			case !a.in.empty():
				a.in.set("")
			case a.c != nil && a.c.pending != nil:
				a.c.pending = nil
				a.c.ui.secret(false)
				a.c.ui.setPrompt("> ")
				a.c.sys("cancelled")
			default:
				a.quit = true
			}
			return
		case 'd':
			if a.in.empty() && a.ov == ovNone {
				a.quit = true
				return
			}
		case 'l':
			a.prev = nil
			return
		case 'k':
			if a.mode != scrSetup {
				if a.ov == ovPalette {
					a.closeOverlay()
				} else {
					a.openPalette()
				}
				return
			}
		}
	}
	if ev.kind == evKey && ev.key == keyF1 {
		if a.ov == ovHelp {
			a.closeOverlay()
		} else {
			a.ov, a.help = ovHelp, 0
		}
		return
	}
	switch a.ov {
	case ovPalette:
		a.paletteKey(ev)
		return
	case ovChannels:
		a.pickerKey(ev)
		return
	case ovHelp:
		a.helpKey(ev)
		return
	case ovForm:
		a.formKey(ev)
		return
	}
	switch a.mode {
	case scrSetup:
		a.setupKey(ev)
	case scrManage:
		a.manageKey(ev)
	default:
		a.mainKey(ev)
	}
}

func (a *app) closeOverlay() {
	a.ov = ovNone
	a.form = nil
}

func (a *app) mainKey(ev event) {
	c := a.c
	if ev.kind == evPaste {
		a.in.insert(strings.ReplaceAll(ev.text, "\r\n", "\n"))
		return
	}
	sug := a.suggestions()
	before := a.in.String()
	switch {
	case ev.key == keyEnter && ev.alt:
		a.in.insert("\n")
	case ev.key == keyEnter:
		if len(sug) > 0 && a.sugSel < len(sug) {
			a.pickSuggestion(sug[a.sugSel], true)
			return
		}
		line := a.in.String()
		a.in.set("")
		a.submit(line)
	case ev.key == keyTab:
		if len(sug) > 0 {
			a.pickSuggestion(sug[a.sugSel], false)
		} else if s := a.in.String(); strings.HasPrefix(s, "/c ") || strings.HasPrefix(s, "/go ") {
			pre := strings.ToLower(strings.TrimSpace(s[strings.Index(s, " ")+1:]))
			for _, ch := range c.chans {
				if strings.HasPrefix(ch.name, pre) && ch.name != "…" {
					a.in.set(s[:strings.Index(s, " ")+1] + ch.name)
					break
				}
			}
		}
	case ev.key == keyUp:
		if len(sug) > 0 {
			if a.sugSel > 0 {
				a.sugSel--
			}
			return
		}
		if a.histPos > 0 {
			if a.histPos == len(a.hist) {
				a.draft = a.in.String()
			}
			a.histPos--
			a.in.set(a.hist[a.histPos])
		}
	case ev.key == keyDown:
		if len(sug) > 0 {
			if a.sugSel < len(sug)-1 {
				a.sugSel++
			}
			return
		}
		if a.histPos < len(a.hist) {
			a.histPos++
			if a.histPos == len(a.hist) {
				a.in.set(a.draft)
			} else {
				a.in.set(a.hist[a.histPos])
			}
		}
	case ev.key == keyPgUp:
		a.scroll += max(1, a.paneRows-2)
		a.clampScroll()
	case ev.key == keyPgDn:
		a.scroll -= max(1, a.paneRows-2)
		a.clampScroll()
	case ev.key == keyEnd && a.in.empty():
		a.scroll, a.unseen = 0, 0
	case ev.key == keyEsc:
		switch {
		case len(sug) > 0:
			a.in.set("")
		case a.scroll > 0:
			a.scroll, a.unseen = 0, 0
		}
	case ev.key == keyF2:
		a.openManage(a.msec)
	case ev.key == keyRune && ev.alt && ev.r >= '1' && ev.r <= '9':
		if n := int(ev.r - '1'); n < len(c.chans) {
			c.switchTo(c.chans[n])
		}
	case ev.key == keyRune && ev.ctrl && ev.r == 'n':
		c.cycle(1)
	case ev.key == keyRune && ev.ctrl && ev.r == 'p':
		c.cycle(-1)
	case ev.key == keyRune && ev.ctrl && ev.r == 'o':
		a.ov, a.psel = ovChannels, a.activeIndex()
	default:
		a.in.key(ev)
	}
	if a.in.String() != before {
		a.sugSel = 0
	}
}

// ---- channel picker -------------------------------------------------------------------------

func (a *app) pickerEntries() []paletteItem {
	c := a.c
	var items []paletteItem
	for i, ch := range c.chans {
		if ch.name == "…" {
			continue
		}
		ch := ch
		label := "#" + ch.name
		hint := ""
		if ch.unread > 0 {
			hint = fmt.Sprintf("%d unread", ch.unread)
		}
		if ch.e2e {
			hint = strings.TrimSpace(hint + " e2e")
		}
		if i < 9 {
			label = fmt.Sprintf("%d  #%s", i+1, ch.name)
		}
		items = append(items, paletteItem{label: label, hint: hint, run: func() { c.switchTo(ch) }})
	}
	items = append(items,
		paletteItem{label: "+ new channel", run: func() { a.openForm(newChannelForm()) }},
		paletteItem{label: "→ join with an invite code", run: func() { a.openForm(joinForm()) }})
	return items
}

func (a *app) drawChannelPicker(cv *canvas) {
	items := a.pickerEntries()
	a.drawList(cv, "CHANNELS", "", items, a.psel, nil)
}

func (a *app) pickerKey(ev event) {
	items := a.pickerEntries()
	switch ev.key {
	case keyEsc:
		a.closeOverlay()
	case keyUp:
		if a.psel > 0 {
			a.psel--
		}
	case keyDown, keyTab:
		if a.psel < len(items)-1 {
			a.psel++
		}
	case keyEnter:
		if a.psel < len(items) {
			a.closeOverlay()
			items[a.psel].run()
		}
	case keyRune:
		if ev.r >= '1' && ev.r <= '9' && !ev.ctrl && !ev.alt {
			if n := int(ev.r - '1'); n < len(a.c.chans) {
				a.closeOverlay()
				a.c.switchTo(a.c.chans[n])
			}
		}
	}
}

// ---- palette -----------------------------------------------------------------------------------

type paletteItem struct {
	label, hint string
	run         func()
}

func (a *app) openPalette() {
	a.ov = ovPalette
	a.pq.set("")
	a.psel = 0
}

func (a *app) paletteItems() []paletteItem {
	c := a.c
	var items []paletteItem
	for _, ch := range c.chans {
		if ch.name == "…" || ch == c.active {
			continue
		}
		ch := ch
		items = append(items, paletteItem{label: "Go to #" + ch.name, hint: "channel", run: func() { c.switchTo(ch) }})
	}
	items = append(items,
		paletteItem{label: "Manage channels, people, invites, devices", hint: "F2", run: func() { a.openManage(0) }},
		paletteItem{label: "New channel", hint: "/create", run: func() { a.openForm(newChannelForm()) }},
		paletteItem{label: "Join with an invite code", hint: "/join", run: func() { a.openForm(joinForm()) }},
		paletteItem{label: "Invite someone to #" + c.activeName(), hint: "/invite", run: func() { a.openForm(inviteForm()) }},
		paletteItem{label: "Send a file", hint: "/send", run: func() { a.openForm(sendForm()) }},
	)
	for _, t := range themes {
		t := t
		items = append(items, paletteItem{label: "Theme: " + t.name, hint: "/theme " + t.id, run: func() { a.applyTheme(t.id) }})
	}
	if a.mouse {
		items = append(items, paletteItem{label: "Turn mouse and touch off", hint: "/mouse off", run: func() { a.applyMouse(false) }})
	} else {
		items = append(items, paletteItem{label: "Turn mouse and touch on", hint: "/mouse on", run: func() { a.applyMouse(true) }})
	}
	for _, ci := range catalog {
		ci := ci
		if ci.group == "app" && (ci.name == "manage" || ci.name == "theme" || ci.name == "mouse") {
			continue
		}
		items = append(items, paletteItem{label: "/" + ci.name + " " + ci.args, hint: ci.desc, run: func() {
			if ci.args == "" {
				a.submit("/" + ci.name)
			} else {
				a.mode = scrMain
				a.in.set("/" + ci.name + " ")
			}
		}})
	}
	q := a.pq.String()
	if q == "" {
		return items
	}
	type scored struct {
		it paletteItem
		s  int
	}
	var hits []scored
	for _, it := range items {
		if ok, s := fuzzy(q, it.label+" "+it.hint); ok {
			hits = append(hits, scored{it, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].s < hits[j].s })
	out := make([]paletteItem, len(hits))
	for i, h := range hits {
		out[i] = h.it
	}
	return out
}

func (a *app) drawPalette(cv *canvas) [2]int {
	items := a.paletteItems()
	if a.psel >= len(items) {
		a.psel = max(0, len(items)-1)
	}
	return a.drawList(cv, "COMMANDS", "esc", items, a.psel, &a.pq)
}

func (a *app) paletteKey(ev event) {
	items := a.paletteItems()
	switch {
	case ev.kind == evPaste:
		a.pq.insert(ev.text)
		a.psel = 0
	case ev.key == keyEsc:
		a.closeOverlay()
	case ev.key == keyUp:
		if a.psel > 0 {
			a.psel--
		}
	case ev.key == keyDown || ev.key == keyTab:
		if a.psel < len(items)-1 {
			a.psel++
		}
	case ev.key == keyPgDn:
		a.psel = min(len(items)-1, a.psel+8)
	case ev.key == keyPgUp:
		a.psel = max(0, a.psel-8)
	case ev.key == keyEnter:
		if a.psel < len(items) {
			a.closeOverlay()
			items[a.psel].run()
		}
	default:
		if a.pq.key(ev) {
			a.psel = 0
		}
	}
}

// drawList draws a centered box with an optional query line and a selectable list.
func (a *app) drawList(cv *canvas, title, corner string, items []paletteItem, sel int, q *editor) [2]int {
	w := a.w - 4
	if w > 72 {
		w = 72
	}
	h := a.h - 4
	if h > 20 {
		h = 20
	}
	x := (a.w - w) / 2
	y := 1
	if a.h > 24 {
		y = 3
	}
	a.box(cv, x, y, w, h, title, corner)
	cur := [2]int{-1, -1}
	row := y + 1
	if q != nil {
		pw := cv.text(x+2, row, 2, "> ", a.stSig())
		if q.empty() {
			cv.text(x+2+pw, row, w-6, "type to filter", a.stMuted())
		} else {
			cv.text(x+2+pw, row, w-6, q.String(), a.stBase())
		}
		cur = [2]int{x + 2 + pw + strWidth(string(q.buf[:q.pos])), row}
		cv.hline(x+1, row+1, w-2, '┄', a.stRule())
		row += 2
	}
	visible := y + h - 1 - row
	first := 0
	if sel >= visible {
		first = sel - visible + 1
	}
	if len(items) == 0 {
		cv.text(x+2, row, w-4, "nothing matches", a.stMuted())
	}
	for i := first; i < len(items) && row < y+h-1; i++ {
		it := items[i]
		st := a.stBase()
		if i == sel {
			st = a.stSel()
			cv.fill(x+1, row, w-2, 1, st)
		}
		lw := cv.text(x+2, row, w-4, clip(it.label, w-4), st)
		if it.hint != "" && w-lw-8 > 6 {
			cv.textR(x+w-2, row, w-lw-8, clip(it.hint, w-lw-8), style{fg: a.th.muted, bg: st.bg, rev: st.rev})
		}
		i := i
		a.onHit(x+1, row, w-2, 1, func() {
			a.psel = i
			a.closeOverlay()
			it.run()
		})
		row++
	}
	return cur
}

// box draws a bordered panel and makes the area outside it close the overlay on tap.
func (a *app) box(cv *canvas, x, y, w, h int, title, corner string) {
	a.onHit(0, 0, a.w, a.h, a.closeOverlay)
	bg := style{fg: a.th.ink, bg: a.th.bg}
	cv.fill(x, y, w, h, bg)
	rs := style{fg: a.th.sig, bg: a.th.bg}
	cv.hline(x, y, w, '─', rs)
	cv.hline(x, y+h-1, w, '─', rs)
	cv.vline(x, y, h, '│', rs)
	cv.vline(x+w-1, y, h, '│', rs)
	cv.put(x, y, '┌', rs, a.w)
	cv.put(x+w-1, y, '┐', rs, a.w)
	cv.put(x, y+h-1, '└', rs, a.w)
	cv.put(x+w-1, y+h-1, '┘', rs, a.w)
	if title != "" {
		cv.text(x+2, y, w-4, " "+title+" ", style{fg: a.th.ink, bg: a.th.bg, bold: true})
	}
	if corner != "" {
		a.button(cv, x+w-3-strWidth(corner), y, " "+corner+" ", a.stMuted(), a.closeOverlay)
	}
	a.onHit(x, y+1, w, h-1, func() {}) // taps inside the box don't close it
}

// ---- help ---------------------------------------------------------------------------------------

var helpLines = []string{
	"KEYS",
	"Enter              send · run a command",
	"Alt+Enter          new line in a message",
	"/                  start a command; ↑↓ pick one, Tab completes",
	"Ctrl-K             command list (type to filter)",
	"Ctrl-O             switch channel",
	"Alt+1 … Alt+9      jump to channel 1–9 (or /1 … /9)",
	"Ctrl-N / Ctrl-P    next / previous channel",
	"PgUp / PgDn        scroll messages; End or Esc back to the latest",
	"↑ / ↓              previous / next thing you typed",
	"F2                 Manage: channels, people, invites, devices, look",
	"Ctrl-W Ctrl-U      delete word / delete to start of line",
	"Ctrl-L             redraw the screen",
	"Ctrl-C             clear the input, close a panel, or quit",
	"",
	"MOUSE AND TOUCH",
	"tap                channels, buttons, list rows, the bottom bar",
	"swipe / wheel      scroll messages and lists",
	"Shift+drag         select text (most terminals); /mouse off turns",
	"                   mouse handling off entirely",
	"",
	"FILES",
	"drag a file into the window and press Enter to send it",
	"received files land in ~/backchannel/inbox/<channel>/",
	"",
	"Everything here is also a slash command — /help lists them in the channel.",
}

func (a *app) drawHelp(cv *canvas) {
	w := min(a.w-4, 78)
	h := min(a.h-2, len(helpLines)+2)
	x := (a.w - w) / 2
	y := (a.h - h) / 2
	a.box(cv, x, y, w, h, "HELP", "esc")
	maxTop := max(0, len(helpLines)-(h-2))
	if a.help > maxTop {
		a.help = maxTop
	}
	for i := 0; i < h-2 && a.help+i < len(helpLines); i++ {
		l := helpLines[a.help+i]
		st := a.stCopy()
		if l == strings.ToUpper(l) && l != "" {
			st = a.stMuted()
		}
		cv.text(x+2, y+1+i, w-4, l, st)
	}
}

func (a *app) helpKey(ev event) {
	switch ev.key {
	case keyEsc, keyEnter:
		a.closeOverlay()
	case keyUp:
		a.help = max(0, a.help-1)
	case keyDown:
		a.help++
	case keyPgDn:
		a.help += 10
	case keyPgUp:
		a.help = max(0, a.help-10)
	}
}
