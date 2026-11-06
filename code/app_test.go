package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- input parser -----------------------------------------------------------------

func TestParseInputKeys(t *testing.T) {
	cases := []struct {
		in   string
		want event
	}{
		{"\x1b[A", event{kind: evKey, key: keyUp}},
		{"\x1bOB", event{kind: evKey, key: keyDown}},
		{"\x1b[1;5C", event{kind: evKey, key: keyRight, ctrl: true}},
		{"\x1b[5~", event{kind: evKey, key: keyPgUp}},
		{"\x1b[Z", event{kind: evKey, key: keyBacktab}},
		{"\x1bOP", event{kind: evKey, key: keyF1}},
		{"\r", event{kind: evKey, key: keyEnter}},
		{"\x7f", event{kind: evKey, key: keyBackspace}},
		{"\x0b", event{kind: evKey, key: keyRune, r: 'k', ctrl: true}},
		{"\x1bb", event{kind: evKey, key: keyRune, r: 'b', alt: true}},
		{"é", event{kind: evKey, key: keyRune, r: 'é'}},
		{"\x1b[<0;10;5M", event{kind: evMouse, act: mPress, x: 9, y: 4}},
		{"\x1b[<0;10;6m", event{kind: evMouse, act: mRelease, x: 9, y: 5}},
		{"\x1b[<32;11;7M", event{kind: evMouse, act: mDrag, x: 10, y: 6}},
		{"\x1b[<64;1;1M", event{kind: evMouse, act: mWheelUp}},
		{"\x1b[<65;1;1M", event{kind: evMouse, act: mWheelDown}},
		{"\x1b[200~/tmp/a file.png\x1b[201~", event{kind: evPaste, text: "/tmp/a file.png"}},
	}
	for _, c := range cases {
		evs, rest := parseInput([]byte(c.in), false)
		if len(rest) != 0 || len(evs) != 1 || evs[0] != c.want {
			t.Errorf("%q: got %+v rest %q, want %+v", c.in, evs, rest, c.want)
		}
	}
}

func TestParseInputPartial(t *testing.T) {
	// a lone Esc waits for more bytes, then counts as Esc once the input goes quiet
	evs, rest := parseInput([]byte("\x1b"), false)
	if len(evs) != 0 || string(rest) != "\x1b" {
		t.Fatalf("lone esc: %+v %q", evs, rest)
	}
	evs, _ = parseInput(rest, true)
	if len(evs) != 1 || evs[0].key != keyEsc {
		t.Fatalf("final esc: %+v", evs)
	}
	// a CSI sequence split across reads
	evs, rest = parseInput([]byte("a\x1b[1;"), false)
	if len(evs) != 1 || string(rest) != "\x1b[1;" {
		t.Fatalf("split csi: %+v %q", evs, rest)
	}
	evs, rest = parseInput(append(rest, []byte("2A")...), false)
	if len(evs) != 1 || evs[0].key != keyUp || !evs[0].shift || len(rest) != 0 {
		t.Fatalf("joined csi: %+v %q", evs, rest)
	}
	// a UTF-8 rune split across reads
	b := []byte("ü")
	evs, rest = parseInput(b[:1], false)
	if len(evs) != 0 || len(rest) != 1 {
		t.Fatalf("split utf8: %+v %q", evs, rest)
	}
	evs, _ = parseInput(append(rest, b[1:]...), false)
	if len(evs) != 1 || evs[0].r != 'ü' {
		t.Fatalf("joined utf8: %+v", evs)
	}
	// an unfinished paste is held back
	evs, rest = parseInput([]byte("\x1b[200~half"), false)
	if len(evs) != 0 || len(rest) == 0 {
		t.Fatalf("partial paste: %+v %q", evs, rest)
	}
}

// ---- editor -----------------------------------------------------------------------

func TestEditor(t *testing.T) {
	var e editor
	for _, r := range "hello big world" {
		e.key(event{kind: evKey, key: keyRune, r: r})
	}
	e.key(event{kind: evKey, key: keyRune, r: 'w', ctrl: true})
	if e.String() != "hello big " {
		t.Fatalf("ctrl-w: %q", e.String())
	}
	e.key(event{kind: evKey, key: keyHome})
	e.insert(">")
	e.key(event{kind: evKey, key: keyEnd})
	e.key(event{kind: evKey, key: keyBackspace})
	if e.String() != ">hello big" {
		t.Fatalf("home/insert/end/backspace: %q", e.String())
	}
	e.key(event{kind: evKey, key: keyLeft, ctrl: true})
	e.key(event{kind: evKey, key: keyRune, r: 'u', ctrl: true})
	if e.String() != "big" || e.pos != 0 {
		t.Fatalf("ctrl-u: %q %d", e.String(), e.pos)
	}
}

// ---- canvas -----------------------------------------------------------------------

func TestWidthAndWrap(t *testing.T) {
	if runeWidth('a') != 1 || runeWidth('界') != 2 || runeWidth('📎') != 2 || runeWidth('́') != 0 {
		t.Fatal("runeWidth")
	}
	rs := ansiRuns("\x1b[2m10:42\x1b[22m \x1b[36m\x1b[1mmina\x1b[0m the quick brown fox jumps over the lazy dog 界界界 again and again", style{}, nil)
	for _, w := range []int{8, 13, 20, 37} {
		for _, row := range wrapRuns(rs, w, 6) {
			n := 0
			for _, r := range row {
				n += runeWidth(r.r)
			}
			if n > w {
				t.Fatalf("width %d: row is %d wide", w, n)
			}
		}
	}
	if rgbTo256(0, 0, 0) != 16 || rgbTo256(255, 255, 255) != 231 {
		t.Fatalf("rgbTo256: %d %d", rgbTo256(0, 0, 0), rgbTo256(255, 255, 255))
	}
}

// ---- app --------------------------------------------------------------------------

// testRig is an app wired to a client whose connection is one end of a pipe, so a
// test can see exactly which frames the UI sends.
type testRig struct {
	a      *app
	c      *client
	mu     sync.Mutex
	frames []Msg
}

func newRig(t *testing.T, w, h int) *testRig {
	t.Helper()
	priv, err := genDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	c := &client{cfg: Config{Hub: "192.168.1.20", Name: "mac", User: "ayush", E2EPub: pubFromPriv(priv), AutoMB: 50},
		dir: t.TempDir(), events: make(chan Msg, 64), quietWho: map[string]bool{},
		readDirty: map[string]int64{}, waiting: map[string]func(Msg){}}
	c.maxMsg.Store(64 << 10)
	c.me, c.role = "ayush", "owner"
	gen := c.ensureChan("general")
	gen.role, gen.members, gen.online = "member", 6, []string{"ayush", "mina"}
	des := c.ensureChan("design")
	des.role, des.e2e, des.epoch, des.topic, des.online = "owner", true, 3, "redesign working room", []string{"ayush", "mina", "sam"}
	ops := c.ensureChan("ops")
	ops.unread = 2
	for i, txt := range []string{"meeting is 3pm", "spec.pdf (1.2 MB)", "pushed the build", "checked the channel bar on a 100-column window, it wraps cleanly now"} {
		c.addLineID(des, int64(212+i), time.Now().UnixMilli(), fmtMsg(Msg{ID: int64(212 + i), TS: time.Now().UnixMilli(), From: []string{"ayush", "mina"}[i%2], Text: txt}, false))
	}
	c.active = des
	c.online.Store(true)

	r := &testRig{c: c}
	local, remote := net.Pipe()
	c.conn = local
	go func() {
		sc := bufio.NewScanner(remote)
		for sc.Scan() {
			var m Msg
			json.Unmarshal(sc.Bytes(), &m)
			r.mu.Lock()
			r.frames = append(r.frames, m)
			r.mu.Unlock()
		}
	}()
	t.Cleanup(func() { local.Close(); remote.Close() })

	a := newApp()
	a.out = &bytes.Buffer{}
	a.w, a.h = w, h
	a.c = c
	c.ui = a
	a.mode = scrMain
	r.a = a
	return r
}

func (r *testRig) sent() []Msg {
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Msg(nil), r.frames...)
}

func (r *testRig) typeText(s string) {
	for _, ch := range s {
		r.a.handle(event{kind: evKey, key: keyRune, r: ch})
	}
}

func (r *testRig) key(k keyCode) { r.a.handle(event{kind: evKey, key: k}) }

func (r *testRig) tap(x, y int) {
	r.a.render()
	r.a.handle(event{kind: evMouse, act: mPress, x: x, y: y})
	r.a.handle(event{kind: evMouse, act: mRelease, x: x, y: y})
}

var escRE = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// screenText renders and returns the visible text of each row.
func (r *testRig) screenText() []string {
	r.a.prev = nil
	r.a.render()
	cv := r.a.prev
	out := make([]string, len(cv))
	for i, row := range cv {
		out[i] = escRE.ReplaceAllString(row, "")
	}
	return out
}

func TestAppRendersEveryScreenAtEverySize(t *testing.T) {
	sizes := [][2]int{{24, 8}, {40, 12}, {60, 20}, {80, 24}, {100, 30}, {140, 40}, {220, 60}}
	for _, sz := range sizes {
		for _, th := range themes {
			r := newRig(t, sz[0], sz[1])
			r.a.setThemeID(th.id)
			check := func(what string) {
				for y, row := range r.screenText() {
					if w := strWidth(row); w != sz[0] {
						t.Fatalf("%dx%d %s %s: row %d is %d cells wide: %q", sz[0], sz[1], th.id, what, y, w, row)
					}
				}
			}
			check("main")
			r.typeText("/cr")
			check("suggestions")
			r.a.in.set("")
			r.a.scroll = 2
			check("scrolled")
			for _, ov := range []overlay{ovPalette, ovChannels, ovHelp} {
				r.a.ov = ov
				check("overlay")
			}
			r.a.closeOverlay()
			r.a.openForm(newChannelForm())
			check("form")
			r.a.closeOverlay()
			for i := range r.a.sections() {
				r.a.openManage(i)
				check("manage " + sectionIDs[i])
			}
			r.a.mode = scrSetup
			r.a.setup = &setupFlow{c: r.c, cfg: r.c.snapshot(), dir: r.c.dir, results: make(chan setupResult, 1)}
			for st := stHub; st <= stReady; st++ {
				r.a.setup.step, r.a.setup.reached = st, st
				for mode := 0; mode < 3; mode++ {
					r.a.setup.mode = mode
					check("setup")
				}
			}
		}
	}
}

func TestAppSendsMessagesAndCommands(t *testing.T) {
	r := newRig(t, 100, 30)
	r.c.switchTo(r.c.chanByName("general")) // #design is e2e and this device has no key for it
	r.typeText("hello there")
	r.key(keyEnter)
	// "/" suggestions: Tab completes, Enter runs
	r.typeText("/wh")
	if s := r.a.suggestions(); len(s) == 0 || s[0].name != "who" {
		t.Fatalf("suggestions for /wh: %+v", s)
	}
	r.key(keyEnter)
	r.typeText("/inv")
	r.key(keyTab)
	if got := r.a.in.String(); got != "/invite " {
		t.Fatalf("tab completion: %q", got)
	}
	r.a.in.set("")

	var sawMsg, sawWho bool
	for _, m := range r.sent() {
		switch {
		case m.T == "msg" && m.Ch == "general" && m.Text == "hello there":
			sawMsg = true
		case m.T == "who" && m.Ch == "general":
			sawWho = true
		}
	}
	if !sawMsg || !sawWho {
		t.Fatalf("frames: %+v", r.sent())
	}
}

func TestAppTapAndSwipe(t *testing.T) {
	r := newRig(t, 100, 30)
	// sidebar: CHANNELS label on row 2, channels from row 3
	r.tap(4, 3)
	if r.c.active.name != "general" {
		t.Fatalf("tap on #general: active is %s", r.c.active.name)
	}
	r.tap(4, 4)
	for i := 0; i < 120; i++ {
		r.c.addLine(r.c.active, "line "+strings.Repeat("x", i%40))
	}
	r.a.render()
	y := r.a.paneY + 2
	r.a.handle(event{kind: evMouse, act: mPress, x: r.a.paneX + 5, y: y})
	r.a.handle(event{kind: evMouse, act: mDrag, x: r.a.paneX + 5, y: y + 6})
	r.a.handle(event{kind: evMouse, act: mRelease, x: r.a.paneX + 5, y: y + 6})
	if r.a.scroll != 6 {
		t.Fatalf("swipe down 6 rows: scroll %d", r.a.scroll)
	}
	r.a.handle(event{kind: evMouse, act: mWheelDown})
	if r.a.scroll != 3 {
		t.Fatalf("wheel down: scroll %d", r.a.scroll)
	}
	// a new line while scrolled up keeps the view and counts as unseen
	r.c.addLine(r.c.active, "new")
	if r.a.unseen != 1 || r.a.scroll != 4 {
		t.Fatalf("unseen %d scroll %d", r.a.unseen, r.a.scroll)
	}
}

func TestAppFormBuildsCommand(t *testing.T) {
	r := newRig(t, 100, 30)
	r.a.openForm(newChannelForm())
	r.typeText("Launch")
	r.key(keyTab)
	r.typeText("q4 launch")
	r.key(keyTab)
	r.key(keyRight) // encryption: on
	r.key(keyTab)
	r.key(keyRight) // self-destruct: 10m
	r.key(keyRight) // 2h
	if got := r.a.form.build(r.a.form.values()); got != "/create launch q4 launch e2e=on expire=2h" {
		t.Fatalf("form command: %q", got)
	}
	r.key(keyTab)
	r.key(keyEnter) // last field: submit
	if r.a.ov != ovNone || r.a.mode != scrMain {
		t.Fatalf("form should close and return to main: ov %d mode %d", r.a.ov, r.a.mode)
	}
	var got *Msg
	for _, m := range r.sent() {
		if m.T == "cmd" && m.Name == "create" {
			m := m
			got = &m
		}
	}
	if got == nil || got.Args["name"] != "launch" || got.Args["e2e"] != "on" || got.Args["expire"] != "2h" || got.Args["topic"] != "q4 launch" {
		t.Fatalf("create frame: %+v", got)
	}
}

func TestAppPaletteThemeAndManage(t *testing.T) {
	r := newRig(t, 100, 30)
	r.a.handle(event{kind: evKey, key: keyRune, r: 'k', ctrl: true})
	if r.a.ov != ovPalette {
		t.Fatal("ctrl-k should open the palette")
	}
	r.typeText("theme amber")
	r.key(keyEnter)
	if r.a.th.id != "amber" || r.c.snapshot().Theme != "amber" {
		t.Fatalf("theme: %s saved %q", r.a.th.id, r.c.snapshot().Theme)
	}
	r.a.handle(event{kind: evKey, key: keyF2})
	if r.a.mode != scrManage {
		t.Fatal("F2 should open Manage")
	}
	r.key(keyTab) // people
	r.key(keyEnter)
	if r.a.mode != scrMain {
		t.Fatal("running a command from Manage returns to the channel")
	}
	found := false
	for _, m := range r.sent() {
		if m.T == "who" {
			found = true
		}
	}
	if !found {
		t.Fatal("Manage → People → first action should send who")
	}
	// /theme and /mouse are handled by the app, not sent to the hub
	r.typeText("/theme telex")
	r.key(keyEnter)
	if r.a.th.id != "telex" {
		t.Fatalf("/theme telex: %s", r.a.th.id)
	}
}

func TestAppPasswordPromptIsMasked(t *testing.T) {
	r := newRig(t, 100, 30)
	r.typeText("/passwd")
	r.key(keyEnter)
	if !r.a.hidden {
		t.Fatal("/passwd should hide input")
	}
	r.typeText("secret-pass")
	for _, row := range r.screenText() {
		if strings.Contains(row, "secret-pass") {
			t.Fatalf("password visible on screen: %q", row)
		}
	}
	r.a.handle(event{kind: evKey, key: keyRune, r: 'c', ctrl: true}) // clears input
	r.a.handle(event{kind: evKey, key: keyRune, r: 'c', ctrl: true}) // cancels the prompt
	if r.c.pending != nil || r.a.hidden {
		t.Fatal("ctrl-c should cancel a password prompt")
	}
}
