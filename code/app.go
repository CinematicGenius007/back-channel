package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The app TUI: a full-screen, raw-mode interface with mouse and touch input, a setup
// flow, a command palette, Manage screens and themes. It sits behind the same ui
// interface as the classic line-mode TUI (tui.go), so every command, frame and piece
// of client state is shared; only how they are shown and entered differs.
//
// The classic TUI is still used when stdin/stdout are not a terminal, when raw mode is
// unavailable, or when asked for with -classic / "ui": "classic" in config.json.

// ui is everything the client asks of a screen.
type ui interface {
	init()
	cleanup()
	clear()
	checkResize()
	print(s string)
	redraw(lines []string)
	setStatus(s string)
	setBar(s string)
	setPrompt(p string)
	resetInput()
	bell()
	secret(on bool) // hide what is typed (passwords)
	flush()         // draw pending changes (called once per main-loop turn)
}

func (t *tui) secret(on bool) {
	if on {
		echoOff()
	} else {
		echoOn()
	}
}
func (t *tui) flush() {}

type screenMode int

const (
	scrMain screenMode = iota
	scrManage
	scrSetup
)

type overlay int

const (
	ovNone overlay = iota
	ovPalette
	ovChannels
	ovHelp
	ovForm
	ovVerify
)

// hit is a clickable/tappable area registered while drawing a frame.
type hit struct {
	x, y, w, h int
	fn         func()
}

type app struct {
	c       *client
	out     io.Writer
	events  chan event
	restore func()

	w, h   int
	prev   []string
	depth  int
	th     *theme
	pal    *[16]colorV
	mouse  bool
	dirty  bool
	closed bool

	mode    screenMode
	ov      overlay
	status  string
	prompt  string
	hidden  bool
	in      editor
	hist    []string
	histPos int
	draft   string

	scroll   int // rows scrolled up from the bottom of the message pane
	msgTotal int // wrapped message rows in the last frame
	unseen   int // lines that arrived while scrolled up
	paneRows int // height of the message pane in the last frame
	paneY    int
	paneX    int
	paneW    int

	sugSel int // selected "/" suggestion

	hits     []hit
	press    *hit
	pressX   int
	pressY   int
	dragFrom int // scroll value when a drag started in the message pane
	dragging bool

	pq   editor // palette query
	psel int

	msec int // manage section
	msel int // manage action

	form *form
	help int // help page scroll

	setup    *setupFlow
	quit     bool
	whoAsked map[string]bool

	comp    *completion       // Tab completion cycle (app_extra.go)
	drafts  map[string]string // channel → unsent input
	draftCh string            // channel the input line belongs to

	findOn, findJump bool // find bar open; scroll to the current match on next draw
	findEd           editor
	findIdx          int // 0 = newest match

	ver *verifyView
}

func newApp() *app {
	a := &app{out: os.Stdout, events: make(chan event, 256), depth: colorDepth(), mouse: true, prompt: "> ",
		whoAsked: map[string]bool{}}
	a.setThemeID("")
	return a
}

func (a *app) setThemeID(id string) {
	a.th = themeByID(id)
	a.pal = a.th.palette()
	a.prev = nil
	a.dirty = true
}

// base styles
func (a *app) stBase() style  { return style{fg: a.th.ink, bg: a.th.bg} }
func (a *app) stCopy() style  { return style{fg: a.th.copy, bg: a.th.bg} }
func (a *app) stMuted() style { return style{fg: a.th.muted, bg: a.th.bg} }
func (a *app) stRule() style  { return style{fg: a.th.rule, bg: a.th.bg} }
func (a *app) stSig() style   { return style{fg: a.th.sigText, bg: a.th.bg, bold: true} }
func (a *app) stPanel() style { return style{fg: a.th.ink, bg: a.th.panel} }
func (a *app) stBadge() style { return style{fg: a.th.sigInk, bg: a.th.sig, bold: true} }

// stSel is the selected-row style; the terminal theme has no panel colour, so it uses reverse video.
func (a *app) stSel() style {
	if a.th.panel == colDefault {
		return style{rev: true}
	}
	return style{fg: a.th.ink, bg: a.th.panel, bold: true}
}

// ---- ui interface -------------------------------------------------------------

func (a *app) init() {
	enableVT()
	a.h, a.w = termSize()
	io.WriteString(a.out, "\x1b[?1049h\x1b[?7l\x1b[?2004h\x1b[?1004h\x1b[?25l")
	a.setMouse(a.mouse)
	a.dirty = true
}

func (a *app) setMouse(on bool) {
	a.mouse = on
	if on {
		io.WriteString(a.out, "\x1b[?1000h\x1b[?1002h\x1b[?1006h")
	} else {
		io.WriteString(a.out, "\x1b[?1006l\x1b[?1002l\x1b[?1000l")
	}
}

func (a *app) cleanup() {
	if a.closed {
		return
	}
	a.closed = true
	io.WriteString(a.out, "\x1b[?1006l\x1b[?1002l\x1b[?1000l\x1b[?2004l\x1b[?1004l\x1b[?7h\x1b[0m\x1b[?25h\x1b[?1049l")
	if a.restore != nil {
		a.restore()
	}
}

func (a *app) clear() { a.scroll, a.unseen, a.dirty = 0, 0, true }

func (a *app) checkResize() {
	h, w := termSize()
	if w != a.w || h != a.h {
		a.w, a.h, a.prev = w, h, nil
		a.dirty = true
	}
}

func (a *app) print(string) {
	if a.scroll > 0 {
		a.unseen++
		a.scroll++ // keep the view still while new lines arrive below
	}
	a.dirty = true
}

func (a *app) redraw([]string) { a.scroll, a.unseen, a.dirty = 0, 0, true }

func (a *app) setStatus(s string) {
	if s == "" && a.status != "" && !strings.HasPrefix(a.status, "connected") {
		s = "connected"
	}
	if s != "" {
		a.status = s
	}
	a.dirty = true
}

func (a *app) setBar(string) { a.dirty = true }

func (a *app) setPrompt(p string) {
	a.prompt = p
	a.dirty = true
}

func (a *app) resetInput()    {}
func (a *app) bell()          { io.WriteString(a.out, "\a") }
func (a *app) secret(on bool) { a.hidden = on; a.dirty = true }

func (a *app) flush() {
	if a.dirty && !a.closed {
		a.render()
	}
}

// ---- input plumbing -------------------------------------------------------------

// readInput turns stdin bytes into events. A lone Esc is only reported after the
// terminal has been quiet for 30 ms, so Esc+[ from an arrow key is never split.
func (a *app) readInput() {
	raw := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				raw <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(raw)
				return
			}
		}
	}()
	var pend []byte
	var timer <-chan time.Time
	waits := 0
	for {
		select {
		case b, ok := <-raw:
			if !ok {
				a.events <- event{kind: evKey, key: keyRune, r: 'd', ctrl: true}
				return
			}
			pend = append(pend, b...)
			evs, rest := parseInput(pend, false)
			for _, e := range evs {
				a.events <- e
			}
			pend = rest
			timer, waits = nil, 0
			if len(pend) > 0 {
				timer = time.After(30 * time.Millisecond)
			}
		case <-timer:
			// an unfinished paste keeps waiting (up to ~3 s) for its end marker
			if strings.HasPrefix(string(pend), pasteStart) && waits < 100 {
				waits++
				timer = time.After(30 * time.Millisecond)
				continue
			}
			if strings.HasPrefix(string(pend), pasteStart) {
				a.events <- event{kind: evPaste, text: strings.TrimPrefix(string(pend), pasteStart)}
				pend = nil
			} else {
				evs, _ := parseInput(pend, true)
				for _, e := range evs {
					a.events <- e
				}
				pend = nil
			}
			timer = nil
		}
	}
}

func (a *app) hitAt(x, y int) *hit {
	for i := len(a.hits) - 1; i >= 0; i-- { // last drawn wins (overlays)
		h := &a.hits[i]
		if x >= h.x && x < h.x+h.w && y >= h.y && y < h.y+h.h {
			return h
		}
	}
	return nil
}

func (a *app) onHit(x, y, w, h int, fn func()) {
	a.hits = append(a.hits, hit{x: x, y: y, w: w, h: h, fn: fn})
}

// handleMouse turns presses, releases, drags and wheel reports into taps and scrolls.
// A tap fires on release over the same target it started on, so a swipe that begins
// on a button does not trigger it.
func (a *app) handleMouse(ev event) {
	inPane := a.mode == scrMain && a.ov == ovNone && ev.x >= a.paneX && ev.x < a.paneX+a.paneW && ev.y >= a.paneY && ev.y < a.paneY+a.paneRows
	switch ev.act {
	case mWheelUp:
		a.wheel(3)
	case mWheelDown:
		a.wheel(-3)
	case mPress:
		a.press = a.hitAt(ev.x, ev.y)
		a.pressX, a.pressY = ev.x, ev.y
		a.dragging = inPane
		a.dragFrom = a.scroll
	case mDrag:
		if a.dragging {
			a.scroll = a.dragFrom + (ev.y - a.pressY)
			a.clampScroll()
			a.dirty = true
		}
	case mRelease:
		moved := abs(ev.x-a.pressX) + abs(ev.y-a.pressY)
		if a.press != nil && moved <= 1 {
			if h := a.hitAt(ev.x, ev.y); h != nil && h.x == a.press.x && h.y == a.press.y && h.w == a.press.w {
				h.fn()
			}
		}
		a.press, a.dragging = nil, false
		a.dirty = true
	}
}

func (a *app) wheel(d int) {
	switch {
	case a.ov == ovHelp:
		a.help -= d
		if a.help < 0 {
			a.help = 0
		}
	case a.ov == ovPalette:
		a.psel -= d / 3
		if a.psel < 0 {
			a.psel = 0
		}
	case a.mode == scrMain && a.ov == ovNone:
		a.scroll += d
		a.clampScroll()
	}
	a.dirty = true
}

func (a *app) clampScroll() {
	if a.scroll < 0 {
		a.scroll = 0
	}
	if m := a.maxScroll(); a.scroll > m {
		a.scroll = m
	}
	if a.scroll == 0 {
		a.unseen = 0
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- running ----------------------------------------------------------------------

// runApp runs the client with the app TUI. It returns false, having touched nothing,
// when the terminal can't do raw mode; the caller then uses the classic TUI.
func runApp(cfg Config, dir string, f clientFlags) bool {
	if !isTerminal() || os.Getenv("TERM") == "dumb" {
		return false
	}
	restore, err := rawMode()
	if err != nil {
		return false
	}
	a := newApp()
	a.restore = restore
	a.setThemeID(cfg.Theme)
	a.mouse = cfg.Mouse != "off"
	a.init()
	defer a.cleanup()
	defer func() {
		if r := recover(); r != nil {
			a.cleanup()
			panic(r)
		}
	}()
	go a.readInput()

	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	needSetup := f.setup || cfg.Hub == "" || (cfg.Session == "" && cfg.Token == "") || f.invite != "" ||
		(f.user != "" && (cfg.Session == "" || !strings.EqualFold(cfg.User, f.user)))
	var c *client
	if needSetup {
		c = a.runSetup(cfg, dir, f, winch, sig)
		if c == nil {
			return true
		}
	} else {
		c = newClient(cfg, dir)
	}
	a.c = c
	c.ui = a
	a.mode = scrMain
	a.setThemeID(c.snapshot().Theme)
	c.clipOn.Store(c.snapshot().ClipSync)

	go c.connectLoop()
	go c.watchOutbox()
	go c.watchClipboard()

	cfg = c.snapshot()
	who := cfg.User
	if who == "" {
		who = cfg.Name + " (guest)"
	}
	c.active = c.ensureChan("…")
	c.sys(fmt.Sprintf("%s %s · hub %s · you are %s · inbox %s", appName, version, cfg.Hub, bold(who), filepath.Join(c.dir, "inbox")))
	c.sys("type to chat · drag a file in to send it · / for commands · Ctrl-K for the command list · F1 for keys")
	a.setStatus(fmt.Sprintf("connecting to %s…", c.tcpAddr))

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for !a.quit {
		select {
		case <-sig:
			return true
		case <-winch:
			a.checkResize()
		case m := <-c.events:
			c.handleEvent(m)
		case <-tick.C:
			c.flushReads()
		case ev := <-a.events:
			a.handle(ev)
		}
		a.afterTurn()
		a.flush()
	}
	return true
}

// afterTurn keeps presence for the open channel filled in without printing the reply.
func (a *app) afterTurn() {
	a.swapDraft()
	c := a.c
	if c == nil || c.legacy || !c.online.Load() || c.active == nil || c.active.name == "…" {
		return
	}
	if !a.whoAsked[c.active.name] {
		a.whoAsked[c.active.name] = true
		c.quietWho[c.active.name] = true
		c.write(Msg{T: "who", Ch: c.active.name})
	}
}

// submit sends a finished input line through the same path as the classic TUI.
func (a *app) submit(line string) {
	c := a.c
	if c.pending != nil {
		c.feedPrompt(line)
		return
	}
	if strings.TrimSpace(line) != "" {
		if n := len(a.hist); n == 0 || a.hist[n-1] != line {
			a.hist = append(a.hist, line)
			if len(a.hist) > 200 {
				a.hist = a.hist[1:]
			}
		}
	}
	a.histPos = len(a.hist)
	a.scroll, a.unseen = 0, 0
	if a.appCommand(line) {
		return
	}
	if !c.handleInput(line) {
		a.quit = true
	}
}

// appCommand handles the few commands that only exist in the app TUI.
func (a *app) appCommand(line string) bool {
	f := fields(line)
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return false
	}
	switch strings.ToLower(f[0]) {
	case "/manage":
		a.openManage(0)
	case "/theme":
		if len(f) == 1 {
			a.openManage(sectionIndex("appearance"))
			return true
		}
		for _, t := range themes {
			if strings.EqualFold(t.id, f[1]) || strings.EqualFold(strings.ReplaceAll(t.name, " ", "-"), f[1]) {
				a.applyTheme(t.id)
				a.c.sys("theme: " + t.name)
				return true
			}
		}
		var ids []string
		for _, t := range themes {
			ids = append(ids, t.id)
		}
		a.c.sys("themes: " + strings.Join(ids, ", "))
	case "/mouse":
		on := len(f) < 2 || f[1] != "off"
		a.applyMouse(on)
		if on {
			a.c.sys("mouse and touch on (hold Shift to select text)")
		} else {
			a.c.sys("mouse and touch off — normal text selection works again")
		}
	case "/find":
		a.openFind(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), f[0])))
	case "/verify":
		if len(f) > 1 || a.c.active == nil || !a.c.active.e2e {
			return false // /verify NAME [FINGERPRINT] goes to the client as usual
		}
		a.openVerify()
	case "/help", "/h", "/?":
		if len(f) == 1 {
			a.ov, a.help = ovHelp, 0
			return true
		}
		return false
	default:
		return false
	}
	return true
}

func (a *app) applyTheme(id string) {
	a.setThemeID(id)
	if a.c != nil {
		a.c.update(func(cf *Config) { cf.Theme = id })
	}
}

func (a *app) applyMouse(on bool) {
	a.setMouse(on)
	v := ""
	if !on {
		v = "off"
	}
	if a.c != nil {
		a.c.update(func(cf *Config) { cf.Mouse = v })
	}
	a.dirty = true
}
