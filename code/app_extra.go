package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// App TUI conveniences on top of the main screen: Tab completion, per-channel drafts,
// tappable file lines, find-in-channel and the device verification screen. As with
// the rest of the app, anything that changes state ends in a slash command
// (/get, /open, /verify, /unverify) so the classic TUI can do the same.

// ---- Tab completion -------------------------------------------------------------------

// completion is the state of a Tab cycle: pressing Tab again (with nothing typed in
// between) moves to the next candidate.
type completion struct {
	head, tail string   // input before the completed word, and after the cursor
	cands      []string // full replacement words
	idx        int
	result     string // input as the last Tab left it
}

var nameCmds = map[string]bool{"/kick": true, "/ban": true, "/mute": true, "/unmute": true, "/unban": true,
	"/role": true, "/verify": true, "/unverify": true, "/add": true, "/transfer": true}
var chanCmds = map[string]bool{"/c": true, "/ch": true, "/switch": true, "/go": true, "/watch": true}

// complete runs one Tab press. It reports whether it changed anything.
func (a *app) complete() bool {
	if a.comp != nil && a.comp.result == a.in.String() && len(a.comp.cands) > 1 {
		a.comp.idx = (a.comp.idx + 1) % len(a.comp.cands)
		a.applyCompletion()
		return true
	}
	a.comp = nil
	runes := a.in.buf
	before, tail := string(runes[:a.in.pos]), string(runes[a.in.pos:])
	ws := strings.LastIndexAny(before, " \n") + 1
	word := before[ws:]
	words := strings.Fields(before[:ws])
	var cands []string
	switch {
	case strings.HasPrefix(word, "@"):
		for _, n := range a.names(word[1:]) {
			cands = append(cands, "@"+n)
		}
	case len(words) == 1 && nameCmds[strings.ToLower(words[0])]:
		cands = a.names(word)
	case len(words) == 1 && chanCmds[strings.ToLower(words[0])]:
		cands = a.channelNames(strings.TrimPrefix(word, "#"))
	}
	if len(cands) == 0 {
		return false
	}
	a.comp = &completion{head: before[:ws], tail: tail, cands: cands}
	a.applyCompletion()
	return true
}

func (a *app) applyCompletion() {
	cp := a.comp
	s := cp.head + cp.cands[cp.idx]
	if !strings.HasPrefix(cp.tail, " ") {
		s += " "
	}
	a.in.set(s + cp.tail)
	a.in.pos = len([]rune(s))
	cp.result = a.in.String()
}

// names lists people known in the active channel (online now, or seen posting),
// matching prefix case-insensitively, without yourself.
func (a *app) names(prefix string) []string {
	ch := a.c.active
	if ch == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		k := strings.ToLower(n)
		if n == "" || seen[k] || a.c.isMe(n) || !strings.HasPrefix(k, strings.ToLower(prefix)) {
			return
		}
		seen[k] = true
		out = append(out, n)
	}
	for _, n := range ch.online {
		add(n)
	}
	for n := range ch.authors {
		add(n)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func (a *app) channelNames(prefix string) []string {
	var out []string
	for _, ch := range a.c.chans {
		if ch.name != "…" && strings.HasPrefix(ch.name, strings.ToLower(prefix)) {
			out = append(out, ch.name)
		}
	}
	return out
}

// ---- per-channel drafts ------------------------------------------------------------------

// swapDraft keeps a half-typed message with the channel it was typed in: switching
// away stores it, switching back restores it. Called after every turn of the loop.
func (a *app) swapDraft() {
	c := a.c
	if c == nil || c.active == nil || c.active.name == a.draftCh || a.hidden || c.pending != nil {
		return
	}
	old, cur := a.draftCh, c.active.name
	a.draftCh = cur
	if old == "" || old == "…" || cur == "…" {
		return // first real channel after connecting: keep whatever was typed while waiting
	}
	if a.drafts == nil {
		a.drafts = map[string]string{}
	}
	if s := a.in.String(); s != "" {
		a.drafts[old] = s
	} else {
		delete(a.drafts, old)
	}
	a.in.set(a.drafts[cur])
	delete(a.drafts, cur)
	a.comp, a.histPos = nil, len(a.hist)
	a.findOn = false
}

// ---- file lines --------------------------------------------------------------------------

// fileTap is what tapping a file line does: download it, or, once it is saved, open
// the folder it is in.
func (a *app) fileTap(ch *chanState, id int64) {
	if _, saved := ch.saved[id]; saved {
		a.submit(fmt.Sprintf("/open %d", id))
	} else {
		a.submit(fmt.Sprintf("/get %d", id))
	}
}

// ---- find in channel --------------------------------------------------------------------

func (a *app) openFind(q string) {
	if a.c == nil || a.c.active == nil {
		return
	}
	a.findOn, a.findIdx, a.findJump = true, 0, true
	a.findEd.set(q)
}

// findMatches lists indexes into the active channel's lines whose text contains the
// query, newest first.
func (a *app) findMatches() []int {
	ch := a.c.active
	q := []rune(strings.ToLower(a.findEd.String()))
	if ch == nil || len(q) == 0 {
		return nil
	}
	var out []int
	for i := len(ch.lines) - 1; i >= 0; i-- {
		if len(matchSpans(ansiRuns(ch.lines[i].text, style{}, nil), q)) > 0 {
			out = append(out, i)
		}
	}
	return out
}

// matchSpans returns the start offsets of q (lowercase runes) in rs.
func matchSpans(rs []run, q []rune) []int {
	var out []int
	for i := 0; i+len(q) <= len(rs); i++ {
		ok := true
		for k, r := range q {
			if unicode.ToLower(rs[i+k].r) != r {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, i)
			i += len(q) - 1
		}
	}
	return out
}

// highlight marks occurrences of the find query in one wrapped row.
func (a *app) highlight(row []run, current bool) []run {
	q := []rune(strings.ToLower(a.findEd.String()))
	spans := matchSpans(row, q)
	if len(spans) == 0 {
		return row
	}
	out := append([]run(nil), row...)
	st := style{fg: a.th.sigInk, bg: a.th.sig, bold: current}
	if a.th.id == "terminal" {
		st = style{rev: true, bold: current}
	}
	for _, s := range spans {
		for k := s; k < s+len(q); k++ {
			out[k].st = st
		}
	}
	return out
}

func (a *app) findKey(ev event) {
	n := len(a.findMatches())
	switch {
	case ev.kind == evPaste:
		a.findEd.insert(strings.ReplaceAll(ev.text, "\n", " "))
		a.findIdx, a.findJump = 0, true
	case ev.key == keyEsc, ev.key == keyRune && ev.ctrl && ev.r == 'f':
		a.findOn = false
	case ev.key == keyEnter, ev.key == keyUp:
		if n > 0 {
			a.findIdx = (a.findIdx + 1) % n
			a.findJump = true
		}
	case ev.key == keyDown:
		if n > 0 {
			a.findIdx = (a.findIdx - 1 + n) % n
			a.findJump = true
		}
	case ev.key == keyPgUp:
		a.scroll += max(1, a.paneRows-2)
		a.clampScroll()
	case ev.key == keyPgDn:
		a.scroll -= max(1, a.paneRows-2)
		a.clampScroll()
	default:
		before := a.findEd.String()
		a.findEd.key(ev)
		if a.findEd.String() != before {
			a.findIdx, a.findJump = 0, true
		}
	}
}

// drawFind replaces the input line while find is open.
func (a *app) drawFind(cv *canvas, y int) [2]int {
	cv.fill(0, y, a.w, 1, a.stBase())
	x := 1 + cv.text(1, y, a.w-2, "find › ", a.stSig())
	n := len(a.findMatches())
	count := ""
	switch {
	case a.findEd.empty():
		count = "Enter/↑ older · ↓ newer · Esc close"
	case n == 0:
		count = "no matches"
	default:
		count = fmt.Sprintf("%d of %d", min(a.findIdx, n-1)+1, n)
	}
	cw := cv.textR(a.w-1, y, a.w/2, count, a.stMuted())
	avail := a.w - x - cw - 3
	text := []rune(a.findEd.String())
	start := 0
	for strWidth(string(text[start:a.findEd.pos])) > avail-1 && start < a.findEd.pos {
		start++
	}
	cv.text(x, y, avail, string(text[start:]), a.stBase())
	return [2]int{x + strWidth(string(text[start:a.findEd.pos])), y}
}

// ---- device verification screen ------------------------------------------------------------

type verifyView struct {
	ch      string
	loading bool
	err     string
	peers   []Peer
	sel     int
}

// openVerify lists every online device in the current e2e channel with its key
// fingerprint and whether you have marked it as checked.
func (a *app) openVerify() {
	c := a.c
	if c.active == nil || !c.active.e2e {
		c.sys("the verify screen lists the devices in an end-to-end encrypted channel — open one first")
		return
	}
	v := &verifyView{ch: c.active.name, loading: true}
	a.ver, a.ov = v, ovVerify
	c.cmd("e2epeers", nil, func(m Msg) {
		v.loading = false
		if !m.OK {
			v.err = m.Text
			if v.err == "" {
				v.err = "the hub refused the request"
			}
		}
		me := c.snapshot().Name
		for _, p := range m.Peers {
			if c.isMe(p.User) && strings.EqualFold(p.Device, me) {
				continue // this device is shown separately at the top
			}
			v.peers = append(v.peers, p)
		}
		sort.SliceStable(v.peers, func(i, j int) bool {
			return strings.ToLower(v.peers[i].User+"@"+v.peers[i].Device) < strings.ToLower(v.peers[j].User+"@"+v.peers[j].Device)
		})
		a.dirty = true
	})
}

func (a *app) toggleVerified(p Peer) {
	fp := keyFingerprint(p.Pub)
	cmd := "/verify"
	if a.c.isVerified(p.User, fp) {
		cmd = "/unverify"
	}
	a.c.handleInput(cmd + " " + p.User + " " + fp)
}

func (a *app) drawVerify(cv *canvas) {
	v := a.ver
	w := min(a.w-4, 76)
	h := min(a.h-2, max(10, len(v.peers)+9))
	x := (a.w - w) / 2
	y := (a.h - h) / 2
	a.box(cv, x, y, w, h, "VERIFY DEVICES · #"+v.ch, "esc")
	row := y + 1
	line := func(s string, st style) {
		if row < y+h-1 {
			cv.text(x+2, row, w-4, clip(s, w-4), st)
			row++
		}
	}
	line("This device  "+keyFingerprint(a.c.snapshot().E2EPub), a.stSig())
	line("Read each fingerprint to its owner (in person or on a call). Mark it", a.stMuted())
	line("once it matches what their own device shows under /verify.", a.stMuted())
	cv.hline(x+1, row, w-2, '┄', a.stRule())
	row++
	switch {
	case v.loading:
		line("asking the hub who is here…", a.stMuted())
	case v.err != "":
		line(v.err, style{fg: a.th.danger, bg: a.th.bg})
	case len(v.peers) == 0:
		line("no other device is online in this channel right now", a.stMuted())
	}
	if v.sel >= len(v.peers) {
		v.sel = max(0, len(v.peers)-1)
	}
	visible := y + h - 1 - row
	first := 0
	if v.sel >= visible {
		first = v.sel - visible + 1
	}
	for i := first; i < len(v.peers) && row < y+h-1; i++ {
		p := v.peers[i]
		fp := keyFingerprint(p.Pub)
		st := a.stBase()
		if i == v.sel {
			st = a.stSel()
			cv.fill(x+1, row, w-2, 1, st)
		}
		mark, mst := "not verified", style{fg: a.th.muted, bg: st.bg, rev: st.rev}
		if a.c.isVerified(p.User, fp) {
			mark, mst = "✓ verified", style{fg: a.th.sigText, bg: st.bg, rev: st.rev, bold: true}
		}
		mw := cv.textR(x+w-2, row, 14, mark, mst)
		who := p.User + " @ " + p.Device
		lw := cv.text(x+2, row, w-mw-6, clip(who, 24), st)
		if w-lw-mw-8 >= 19 {
			cv.text(x+2+max(lw+2, 26), row, w-mw-max(lw+2, 26)-6, fp, style{fg: a.th.copy, bg: st.bg, rev: st.rev})
		}
		i := i
		a.onHit(x+1, row, w-2, 1, func() { v.sel = i; a.toggleVerified(p) })
		row++
	}
}

func (a *app) verifyKey(ev event) {
	v := a.ver
	switch ev.key {
	case keyEsc:
		a.closeOverlay()
	case keyUp:
		v.sel = max(0, v.sel-1)
	case keyDown:
		if v.sel < len(v.peers)-1 {
			v.sel++
		}
	case keyEnter:
		if v.sel < len(v.peers) {
			a.toggleVerified(v.peers[v.sel])
		}
	case keyRune:
		if ev.r == ' ' && v.sel < len(v.peers) {
			a.toggleVerified(v.peers[v.sel])
		}
	}
}
