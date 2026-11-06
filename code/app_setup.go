package main

import (
	"fmt"
	"os"
	"strings"
)

// Setup: the first-run flow in the app TUI (also `bch setup`). Five steps — hub,
// device, account, look, ready — replacing the plain-terminal questions the classic
// client asks before it starts. Nothing is sent anywhere until the account step,
// which uses the same one-shot login (loginOnce) as `/login`.

type setupStep int

const (
	stHub setupStep = iota
	stDevice
	stAccount
	stLook
	stReady
)

var setupNames = []string{"Hub", "Device", "Account", "Look", "Ready"}

type setupResult struct {
	kind string // "discover" | "login"
	hubs []string
	ok   Msg
	err  error
}

type setupFlow struct {
	step    setupStep
	reached setupStep
	cfg     Config
	dir     string
	c       *client

	hubs        []string
	discovering bool
	hubSel      int
	addr        editor

	name editor

	mode                           int // 0 sign in, 1 invite, 2 guest token
	focus                          int // account: 0 = mode selector, 1.. = fields
	user, pass, pass2, code, token editor

	themeSel int
	busy     bool
	err      string
	done     bool
	results  chan setupResult
}

func (a *app) runSetup(cfg Config, dir string, f clientFlags, winch, sig chan os.Signal) *client {
	s := &setupFlow{cfg: cfg, dir: dir, results: make(chan setupResult, 4)}
	a.setup = s
	a.mode = scrSetup
	s.name.set(cfg.Name)
	s.user.set(firstNonEmpty(f.user, cfg.User))
	if f.invite != "" {
		s.mode = 1
		s.code.set(f.invite)
	} else if cfg.Token != "" && cfg.User == "" {
		s.mode = 2
		s.token.set(cfg.Token)
	}
	s.focus = 1
	for i, t := range themes {
		if t == a.th {
			s.themeSel = i
		}
	}
	if cfg.Hub == "" || f.setup {
		s.step = stHub
		a.discover()
	} else {
		s.step = stAccount
		s.reached = stAccount
		s.c = a.makeClient()
	}
	a.dirty = true
	for {
		a.flush()
		select {
		case <-sig:
			return nil
		case <-winch:
			a.checkResize()
		case ev := <-a.events:
			a.handle(ev)
		case r := <-s.results:
			a.setupResult(r)
		}
		if a.quit {
			return nil
		}
		if s.done {
			a.setup = nil
			return s.c
		}
	}
}

func (a *app) discover() {
	s := a.setup
	if s.discovering {
		return
	}
	s.discovering = true
	go func() { s.results <- setupResult{kind: "discover", hubs: discover(1500)} }()
}

// makeClient builds the client for the chosen hub (creating the device key on first run).
func (a *app) makeClient() *client {
	s := a.setup
	saveConfig(s.dir, s.cfg)
	c := newClient(s.cfg, s.dir)
	c.ui = a
	s.cfg = c.snapshot()
	return c
}

func (a *app) setupResult(r setupResult) {
	s := a.setup
	a.dirty = true
	switch r.kind {
	case "discover":
		s.discovering = false
		s.hubs = r.hubs
		if s.hubSel >= len(a.hubOptions()) {
			s.hubSel = 0
		}
	case "login":
		s.busy = false
		if r.err != nil {
			s.err = r.err.Error()
			return
		}
		s.c.update(func(cf *Config) { cf.User, cf.Session, cf.Token = r.ok.User, r.ok.Session, "" })
		s.pass.set("")
		s.pass2.set("")
		a.setupGo(stLook)
	}
}

type hubOption struct {
	label, note, hub string
	kind             int // 0 a hub, 1 type an address, 2 run my own
}

func (a *app) hubOptions() []hubOption {
	s := a.setup
	var out []hubOption
	seen := map[string]bool{}
	if s.cfg.Hub != "" {
		out = append(out, hubOption{label: s.cfg.Hub, note: "saved", hub: s.cfg.Hub})
		seen[s.cfg.Hub] = true
	}
	for _, h := range s.hubs {
		if !seen[h] {
			out = append(out, hubOption{label: h, note: "found on this network", hub: h})
			seen[h] = true
		}
	}
	out = append(out, hubOption{label: "Type an address…", note: "host, host:port or tls://host", kind: 1})
	out = append(out, hubOption{label: "Run my own hub", note: "bch serve", kind: 2})
	return out
}

// setupGo moves to a step, doing whatever entering it needs.
func (a *app) setupGo(to setupStep) {
	s := a.setup
	s.err = ""
	s.step = to
	if to > s.reached {
		s.reached = to
	}
	if to == stAccount {
		s.focus = 1
	}
	a.dirty = true
}

// setupNext validates the current step and moves on.
func (a *app) setupNext() {
	s := a.setup
	if s.busy {
		return
	}
	s.err = ""
	switch s.step {
	case stHub:
		opts := a.hubOptions()
		o := opts[min(s.hubSel, len(opts)-1)]
		hub := o.hub
		switch o.kind {
		case 1:
			hub = strings.TrimSpace(s.addr.String())
			if hub == "" {
				s.err = "type the hub's address first"
				return
			}
		case 2:
			s.err = "start the hub first (see the steps on the right), then pick it here"
			return
		}
		if hub != s.cfg.Hub || s.c == nil {
			if hub != s.cfg.Hub { // sessions and pins belong to one hub
				s.cfg.Session, s.cfg.Token, s.cfg.FP = "", "", ""
			}
			s.cfg.Hub = hub
			s.cfg.TLS = strings.HasPrefix(hub, "tls://")
			s.c = a.makeClient()
		}
		a.setupGo(stDevice)
	case stDevice:
		name := strings.TrimSpace(s.name.String())
		if name == "" {
			s.err = "give this device a name"
			return
		}
		s.c.update(func(cf *Config) { cf.Name = name })
		s.cfg = s.c.snapshot()
		a.setupGo(stAccount)
	case stAccount:
		a.setupLogin()
	case stLook:
		id := themes[s.themeSel].id
		s.c.update(func(cf *Config) {
			cf.Theme = id
			if !a.mouse {
				cf.Mouse = "off"
			} else {
				cf.Mouse = ""
			}
		})
		a.setupGo(stReady)
	case stReady:
		s.done = true
	}
}

func (a *app) setupLogin() {
	s := a.setup
	c := s.c
	cfg := c.snapshot()
	user := strings.ToLower(strings.TrimSpace(s.user.String()))
	switch s.mode {
	case 0:
		if user == "" {
			s.err = "type your account name"
			return
		}
		if s.pass.empty() {
			if cfg.Session != "" && strings.EqualFold(cfg.User, user) {
				a.setupGo(stLook) // already signed in here
				return
			}
			s.err = "type your password"
			return
		}
		s.busy = true
		auth := Auth{User: user, Pass: s.pass.String()}
		go func() { ok, err := c.loginOnce(auth); s.results <- setupResult{kind: "login", ok: ok, err: err} }()
	case 1:
		code := strings.TrimSpace(s.code.String())
		switch {
		case code == "":
			s.err = "paste the invite code"
		case user == "":
			s.err = "choose a username"
		case len([]rune(s.pass.String())) < 8:
			s.err = "the password needs 8 or more characters"
		case s.pass.String() != s.pass2.String():
			s.err = "the passwords don't match"
		default:
			s.busy = true
			auth := Auth{Invite: code, User: user, Pass: s.pass.String()}
			go func() { ok, err := c.loginOnce(auth); s.results <- setupResult{kind: "login", ok: ok, err: err} }()
		}
	case 2:
		tok := strings.TrimSpace(s.token.String())
		if tok == "" {
			s.err = "paste the hub's guest token"
			return
		}
		c.update(func(cf *Config) { cf.Token, cf.Session, cf.User = tok, "", "" })
		a.setupGo(stLook)
	}
}

func (s *setupFlow) accountFields() []*editor {
	switch s.mode {
	case 0:
		return []*editor{&s.user, &s.pass}
	case 1:
		return []*editor{&s.code, &s.user, &s.pass, &s.pass2}
	}
	return []*editor{&s.token}
}

func (s *setupFlow) accountLabels() []string {
	switch s.mode {
	case 0:
		return []string{"account", "password"}
	case 1:
		return []string{"invite code", "username", "password", "repeat it"}
	}
	return []string{"guest token"}
}

func (a *app) setupKey(ev event) {
	s := a.setup
	if s == nil {
		return
	}
	a.dirty = true
	if ev.key == keyEsc {
		if s.step > stHub {
			a.setupGo(s.step - 1)
		}
		return
	}
	switch s.step {
	case stHub:
		opts := a.hubOptions()
		typing := opts[min(s.hubSel, len(opts)-1)].kind == 1
		switch {
		case ev.key == keyUp:
			if s.hubSel > 0 {
				s.hubSel--
			}
		case ev.key == keyDown || ev.key == keyTab:
			if s.hubSel < len(opts)-1 {
				s.hubSel++
			}
		case ev.key == keyEnter:
			a.setupNext()
		case ev.kind == evPaste:
			s.hubSel = len(opts) - 2
			s.addr.insert(strings.TrimSpace(ev.text))
		case ev.key == keyRune && !ev.ctrl && !ev.alt && !typing && ev.r == 'r':
			a.discover()
		case ev.key == keyRune && !ev.ctrl && !ev.alt && !typing:
			s.hubSel = len(opts) - 2 // start typing an address
			s.addr.key(ev)
		default:
			if typing {
				s.addr.key(ev)
			}
		}
	case stDevice:
		switch {
		case ev.key == keyEnter:
			a.setupNext()
		case ev.kind == evPaste:
			s.name.insert(strings.TrimSpace(ev.text))
		default:
			s.name.key(ev)
		}
	case stAccount:
		flds := s.accountFields()
		switch {
		case ev.key == keyTab || ev.key == keyDown:
			s.focus = min(s.focus+1, len(flds))
		case ev.key == keyBacktab || ev.key == keyUp:
			s.focus = max(s.focus-1, 0)
		case ev.key == keyEnter:
			if s.focus < len(flds) {
				s.focus++
			} else {
				a.setupNext()
			}
		case s.focus == 0 && (ev.key == keyRight || ev.key == keyLeft):
			d := 1
			if ev.key == keyLeft {
				d = 2
			}
			s.mode = (s.mode + d) % 3
			s.err = ""
		case s.focus > 0 && ev.kind == evPaste:
			flds[s.focus-1].insert(strings.TrimSpace(ev.text))
		case s.focus > 0:
			flds[s.focus-1].key(ev)
		}
	case stLook:
		switch {
		case ev.key == keyUp:
			if s.themeSel > 0 {
				s.themeSel--
				a.setThemeID(themes[s.themeSel].id)
			}
		case ev.key == keyDown || ev.key == keyTab:
			if s.themeSel < len(themes)-1 {
				s.themeSel++
				a.setThemeID(themes[s.themeSel].id)
			}
		case ev.key == keyRune && ev.r == 'm' && !ev.ctrl:
			a.setMouse(!a.mouse)
		case ev.key == keyEnter:
			a.setupNext()
		}
	case stReady:
		if ev.key == keyEnter {
			a.setupNext()
		}
	}
}

// ---- drawing -----------------------------------------------------------------------------

// para writes wrapped plain text and returns the number of rows used.
func (a *app) para(cv *canvas, x, y, w, maxRows int, s string, st style) int {
	rows := wrapRuns(ansiRuns(s, st, nil), w, 0)
	n := 0
	for _, r := range rows {
		if n >= maxRows {
			break
		}
		cv.drawRuns(x, y+n, w, r)
		n++
	}
	return n
}

func (a *app) drawSetup(cv *canvas) [2]int {
	s := a.setup
	a.drawTitle(cv, fmt.Sprintf("setup · step %d of %d", s.step+1, len(setupNames)))
	top := a.bodyTop()
	bot := a.h - 2
	railW := 0
	if a.w >= 72 {
		railW = 22
		cv.text(2, top, railW-2, "SET UP", a.stMuted())
		for i, n := range setupNames {
			i := setupStep(i)
			y := top + 1 + int(i)
			st := a.stMuted()
			mark := fmt.Sprintf("%02d", i+1)
			switch {
			case i == s.step:
				st = a.stSel()
				cv.fill(0, y, railW, 1, st)
				cv.text(0, y, 1, "▌", style{fg: a.th.sig, bg: st.bg})
			case i < s.reached || (i <= s.reached && i != s.step):
				st = a.stCopy()
				mark = "✓ "
			}
			mst := style{fg: a.th.sigText, bg: st.bg, rev: st.rev}
			cv.text(2, y, 3, mark, mst)
			cv.text(5, y, railW-6, n, st)
			if i <= s.reached {
				a.onHit(0, y, railW, 1, func() {
					if i == stAccount && s.c == nil {
						return
					}
					a.setupGo(i)
				})
			}
		}
		cv.vline(railW, top, bot-top, '│', a.stRule())
		a.para(cv, 2, bot-3, railW-3, 3, "nothing is saved on the hub until the account step", a.stMuted())
	}
	x := railW + 4
	if railW == 0 {
		x = 2
	}
	w := a.w - x - 2
	y := top + 1
	cv.text(x, y, w, fmt.Sprintf("[%02d] %s", s.step+1, strings.ToUpper(setupNames[s.step])), a.stMuted())
	y++
	cur := [2]int{-1, -1}
	title := func(t string) {
		cv.text(x, y, w, t, style{fg: a.th.ink, bg: a.th.bg, bold: true})
		y += 2
	}
	body := func(t string) {
		y += a.para(cv, x, y, min(w, 72), bot-y, t, a.stCopy()) + 1
	}
	fieldW := min(w, 56)
	input := func(label string, ed *editor, mask, focused bool, hint string) {
		cv.text(x, y, 14, label, map[bool]style{true: a.stSig(), false: a.stMuted()}[focused])
		fst := style{fg: a.th.ink, bg: a.th.panel}
		if a.th.panel == colDefault {
			fst = style{under: true}
		}
		vx, vw := x+14, fieldW-14
		cv.fill(vx, y, vw, 1, fst)
		val := ed.buf
		if mask {
			val = []rune(strings.Repeat("•", len(ed.buf)))
		}
		start := 0
		for strWidth(string(val[start:ed.pos])) > vw-3 && start < ed.pos {
			start++
		}
		cv.text(vx+1, y, vw-2, string(val[start:]), fst)
		if len(val) == 0 && hint != "" {
			cv.text(vx+1, y, vw-2, clip(hint, vw-2), style{fg: a.th.muted, bg: fst.bg, under: fst.under})
		}
		if focused {
			cur = [2]int{vx + 1 + strWidth(string(val[start:ed.pos])), y}
		}
		y += 2
	}

	switch s.step {
	case stHub:
		title("Which hub are you joining?")
		body("A hub is the one machine all your devices connect to. Pick one found on this network, type an address, or start your own.")
		opts := a.hubOptions()
		for i, o := range opts {
			if y >= bot-3 {
				break
			}
			i := i
			st := a.stBase()
			mark := "○"
			if i == s.hubSel {
				st = a.stSel()
				mark = "●"
				cv.fill(x-1, y, fieldW+2, 1, st)
			}
			cv.text(x, y, 2, mark, style{fg: a.th.sigText, bg: st.bg, rev: st.rev})
			lw := cv.text(x+2, y, fieldW-2, clip(o.label, fieldW-4), st)
			if o.note != "" && fieldW-lw-6 > 6 {
				cv.textR(x+fieldW, y, fieldW-lw-6, clip(o.note, fieldW-lw-6), style{fg: a.th.muted, bg: st.bg, rev: st.rev})
			}
			a.onHit(x-1, y, fieldW+2, 1, func() { s.hubSel = i; s.err = "" })
			y++
		}
		if s.discovering {
			cv.text(x, y, w, "searching this network…", a.stMuted())
		} else {
			a.button(cv, x, y, "↻ search again (r)", a.stMuted(), a.discover)
		}
		y += 2
		o := opts[min(s.hubSel, len(opts)-1)]
		switch o.kind {
		case 1:
			input("address", &s.addr, false, true, "192.168.1.20 or tls://hub.example.net")
		case 2:
			body("On the machine that should be the hub, run:  bch serve   — it appears in this list within a second or two (press r to look again). For a hub on the internet run  bch serve -public  and type tls://HOST here. PUBLIC-SERVER.md has the details.")
		}
	case stDevice:
		title("Name this device.")
		body("The name shows up when you send from here and in your list of devices. This device also gets its own encryption key; the private half never leaves it.")
		input("device name", &s.name, false, true, "e.g. mac, work-pc")
		if s.c != nil {
			cfg := s.c.snapshot()
			cv.text(x, y, 14, "device key", a.stMuted())
			cv.text(x+14, y, w-14, keyFingerprint(cfg.E2EPub), a.stSig())
			y += 2
			body("Compare this with what a teammate sees under /verify YOURNAME to be sure messages in encrypted channels reach you and nobody else.")
		}
	case stAccount:
		title("Who are you on this hub?")
		tabs := []string{"Sign in", "I have an invite", "Guest token"}
		tx := x
		for i, t := range tabs {
			i := i
			st := a.stPanel()
			if i == s.mode {
				st = a.stBadge()
			}
			if a.th.panel == colDefault && i != s.mode {
				st = style{}
			}
			tx += a.button(cv, tx, y, " "+t+" ", st, func() { s.mode, s.focus, s.err = i, 1, "" }) + 1
		}
		if s.focus == 0 {
			cv.text(tx+1, y, w-(tx-x)-1, "←/→", a.stMuted())
		}
		y += 2
		flds := s.accountFields()
		labels := s.accountLabels()
		for i, ed := range flds {
			i := i
			mask := labels[i] == "password" || labels[i] == "repeat it"
			hint := ""
			if s.mode == 0 && mask && s.c != nil && s.c.snapshot().Session != "" {
				hint = "leave empty to keep the saved session"
			}
			fy := y
			input(labels[i], ed, mask, s.focus == i+1, hint)
			a.onHit(x, fy, fieldW, 1, func() { s.focus = i + 1 })
		}
		switch s.mode {
		case 0:
			body("Your password is only used to get a session for this device. The hub stores a hash, never the password.")
		case 1:
			body("Someone on this hub sent you a code. It works once and expires; it puts you in the channel it was made for.")
		case 2:
			body("A shared token gives guest access, if the hub allows it. Guests have no account and see only the default channel.")
		}
		if s.busy {
			cv.text(x, y, w, "signing in…", a.stMuted())
		}
	case stLook:
		title("Pick a look.")
		body("It applies as you move through the list. Change it later with /theme or in Manage → Appearance.")
		for i, t := range themes {
			if y >= bot-3 {
				break
			}
			i := i
			st := a.stBase()
			if i == s.themeSel {
				st = a.stSel()
				cv.fill(x-1, y, fieldW+2, 1, st)
			}
			cv.text(x, y, 2, map[bool]string{true: "●", false: "○"}[i == s.themeSel], style{fg: a.th.sigText, bg: st.bg, rev: st.rev})
			cv.text(x+2, y, 20, t.name, st)
			// swatches of the theme's own colours
			sx := x + 24
			for _, col := range []colorV{t.bg, t.panel, t.ink, t.muted, t.sig} {
				if col != colDefault {
					cv.text(sx, y, 2, "  ", style{bg: col})
				}
				sx += 3
			}
			a.onHit(x-1, y, fieldW+2, 1, func() { s.themeSel = i; a.setThemeID(themes[i].id) })
			y++
		}
		y++
		a.button(cv, x, y, fmt.Sprintf("[%s] mouse and touch input (m)", map[bool]string{true: "x", false: " "}[a.mouse]), a.stCopy(), func() { a.setMouse(!a.mouse) })
		y += 2
		body("With mouse input on, hold Shift while dragging to select text.")
	case stReady:
		title("You are set up.")
		cfg := s.c.snapshot()
		transport := "plain TCP"
		if cfg.TLS {
			transport = "TLS"
			if cfg.FP != "" {
				transport += ", certificate pinned " + clip(cfg.FP, 23)
			}
		}
		acct := cfg.User
		if acct == "" {
			acct = "guest"
		}
		for _, kv := range [][2]string{{"hub", cfg.Hub}, {"transport", transport}, {"account", acct}, {"device", cfg.Name}, {"device key", keyFingerprint(cfg.E2EPub)}, {"look", a.th.name}} {
			cv.text(x, y, 14, kv[0], a.stMuted())
			cv.text(x+14, y, w-16, clip(kv[1], w-16), a.stCopy())
			cv.text(x+w-2, y, 2, "✓", a.stSig())
			y++
		}
		y++
		body("Type to talk. Drag a file into the window to send it. Start with / to see commands, press Ctrl-K for the full list, F2 for Manage. Everything is saved in " + s.dir + ".")
	}

	if s.err != "" {
		cv.text(x, bot-1, w, clip("✗ "+s.err, w), style{fg: a.th.danger, bg: a.th.bg})
	}
	next := " Continue ↵ "
	if s.step == stReady {
		next = " Open bch ↵ "
	}
	if s.busy {
		next = " Working… "
	}
	cv.fill(0, a.h-1, a.w, 1, a.stBase())
	if s.step > stHub {
		a.button(cv, 1, a.h-1, " ← Back ", a.stPanel(), func() { a.setupGo(s.step - 1) })
	}
	a.button(cv, a.w-1-strWidth(next), a.h-1, next, a.stBadge(), a.setupNext)
	if a.w >= 80 {
		cv.text(12, a.h-1, a.w-30, "Esc back · Tab next field · Ctrl-C quit", a.stMuted())
	}
	return cur
}
