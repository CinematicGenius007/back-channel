package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseFocusReports(t *testing.T) {
	evs, rest := parseInput([]byte("\x1b[I\x1b[Oa"), false)
	if len(rest) != 0 || len(evs) != 3 || evs[0].kind != evFocus || !evs[0].focus || evs[1].kind != evFocus || evs[1].focus {
		t.Fatalf("focus reports: %+v rest %q", evs, rest)
	}
	r := newRig(t, 100, 30)
	r.a.handle(evs[1])
	if !r.c.unfocused.Load() {
		t.Fatal("focus-out should mark the client unfocused")
	}
	r.a.handle(evs[0])
	if r.c.unfocused.Load() {
		t.Fatal("focus-in should clear it")
	}
}

func TestAppTabCompletion(t *testing.T) {
	r := newRig(t, 100, 30) // #design: online ayush, mina, sam; ayush is me
	try := func(typed, want string, tabs int) {
		t.Helper()
		r.a.in.set("")
		r.typeText(typed)
		for i := 0; i < tabs; i++ {
			r.key(keyTab)
		}
		if got := r.a.in.String(); got != want {
			t.Fatalf("%q + %d×Tab: got %q want %q", typed, tabs, got, want)
		}
	}
	try("hi @m", "hi @mina ", 1)
	try("hi @", "hi @mina ", 1)
	try("hi @", "hi @sam ", 2)  // Tab again cycles
	try("hi @", "hi @mina ", 3) // and wraps
	try("@a", "@a", 1)          // only me: nothing to complete
	try("/kick s", "/kick sam ", 1)
	try("/c o", "/c ops ", 1)
	try("/watch #gen", "/watch general ", 1)
	// authors count too, not only who is online
	gen := r.c.chanByName("general")
	r.c.switchTo(gen)
	r.c.renderMsg(gen, Msg{T: "msg", Ch: "general", ID: 900, From: "zoe", Text: "x", Hist: true})
	try("cc @z", "cc @zoe ", 1)
}

func TestAppDraftsStayWithTheirChannel(t *testing.T) {
	r := newRig(t, 100, 30)
	r.a.afterTurn() // the input belongs to #design from here
	r.typeText("half a thought")
	r.c.switchTo(r.c.chanByName("general"))
	r.a.afterTurn()
	if !r.a.in.empty() {
		t.Fatalf("#general should start empty, got %q", r.a.in.String())
	}
	r.typeText("other")
	r.c.switchTo(r.c.chanByName("design"))
	r.a.afterTurn()
	if got := r.a.in.String(); got != "half a thought" {
		t.Fatalf("draft for #design: %q", got)
	}
	r.c.switchTo(r.c.chanByName("general"))
	r.a.afterTurn()
	if got := r.a.in.String(); got != "other" {
		t.Fatalf("draft for #general: %q", got)
	}
}

func TestAppTapFileLine(t *testing.T) {
	var opened []string
	old := openFolder
	openFolder = func(p string) { opened = append(opened, p) }
	defer func() { openFolder = old }()

	r := newRig(t, 100, 30)
	gen := r.c.chanByName("general")
	r.c.switchTo(gen)
	now := time.Now().UnixMilli()
	gen.files[300] = Msg{T: "file", Ch: "general", ID: 300, TS: now, From: "mina", FID: "f1", Name: "spec.pdf", Size: 10}
	r.c.addLineID(gen, 300, now, fmtFile(Msg{ID: 300, TS: now, From: "mina", Name: "spec.pdf", Size: 10}))
	row := -1
	for y, l := range r.screenText() {
		if strings.Contains(l, "spec.pdf") {
			row = y
		}
	}
	if row < 0 {
		t.Fatal("file line not on screen")
	}
	r.c.httpAddr = "127.0.0.1:1" // the download fails fast; only the command matters here
	r.tap(r.a.paneX+4, row)
	if h := r.a.hist; len(h) == 0 || h[len(h)-1] != "/get 300" {
		t.Fatalf("tap on an unsaved file should run /get 300; history %q", h)
	}
	gen.saved[300] = r.c.dir + "/inbox/general/spec.pdf"
	r.tap(r.a.paneX+4, row)
	if h := r.a.hist; h[len(h)-1] != "/open 300" || len(opened) != 1 || !strings.HasSuffix(strings.ReplaceAll(opened[0], "\\", "/"), "inbox/general") {
		t.Fatalf("tap on a saved file should open its folder; history %q opened %q", h, opened)
	}
}

func TestAppFind(t *testing.T) {
	r := newRig(t, 100, 30)
	gen := r.c.chanByName("general")
	r.c.switchTo(gen)
	r.a.afterTurn()
	r.c.addLine(gen, "the needle is here")
	for i := 0; i < 80; i++ {
		r.c.addLine(gen, "hay")
	}
	r.c.addLine(gen, "another NEEDLE, newer")
	r.a.handle(event{kind: evKey, key: keyRune, r: 'f', ctrl: true})
	if !r.a.findOn {
		t.Fatal("Ctrl-F should open find")
	}
	r.typeText("needle")
	if n := len(r.a.findMatches()); n != 2 {
		t.Fatalf("matches: %d", n)
	}
	screen := strings.Join(r.screenText(), "\n")
	if !strings.Contains(screen, "1 of 2") || !strings.Contains(screen, "NEEDLE, newer") {
		t.Fatalf("newest match first:\n%s", screen)
	}
	r.key(keyEnter) // older
	screen = strings.Join(r.screenText(), "\n")
	if !strings.Contains(screen, "2 of 2") || !strings.Contains(screen, "the needle is here") || r.a.scroll == 0 {
		t.Fatalf("Enter should scroll to the older match (scroll %d):\n%s", r.a.scroll, screen)
	}
	if !r.a.in.empty() {
		t.Fatal("typing in find must not touch the message input")
	}
	r.key(keyEsc)
	if r.a.findOn {
		t.Fatal("Esc closes find")
	}
	r.typeText("/find hay")
	r.key(keyEnter)
	if !r.a.findOn || r.a.findEd.String() != "hay" {
		t.Fatalf("/find hay: on %v query %q", r.a.findOn, r.a.findEd.String())
	}
}

func TestProgressText(t *testing.T) {
	got := progressText("uploading a.zip", 50, 100)
	if !strings.Contains(got, "[##########..........] 50%") {
		t.Fatalf("progress: %q", got)
	}
	if got := progressText("downloading", 2048, 0); !strings.Contains(got, humanSize(2048)) {
		t.Fatalf("unknown total: %q", got)
	}
}

func TestAppVerifyScreen(t *testing.T) {
	r := newRig(t, 100, 30) // #design is e2e
	peerPriv, _ := genDeviceKey()
	peers := []Peer{{User: "ayush", Device: "mac", Pub: r.c.snapshot().E2EPub}, {User: "mina", Device: "laptop", Pub: pubFromPriv(peerPriv)}}
	answer := func() {
		t.Helper()
		var rid string
		for _, m := range r.sent() {
			if m.T == "cmd" && m.Name == "e2epeers" {
				rid = m.RID
			}
		}
		if rid == "" {
			t.Fatal("no e2epeers request sent")
		}
		r.c.handleEvent(Msg{T: "res", RID: rid, OK: true, Peers: peers})
	}
	r.typeText("/verify")
	r.key(keyEnter)
	if r.a.ov != ovVerify {
		t.Fatal("/verify in an e2e channel opens the verify screen")
	}
	answer()
	if len(r.a.ver.peers) != 1 || r.a.ver.peers[0].User != "mina" {
		t.Fatalf("this device should be listed separately: %+v", r.a.ver.peers)
	}
	fp := keyFingerprint(peers[1].Pub)
	screen := strings.Join(r.screenText(), "\n")
	if !strings.Contains(screen, fp) || !strings.Contains(screen, "not verified") {
		t.Fatalf("verify screen:\n%s", screen)
	}
	r.key(keyEnter) // runs /verify mina FP, which checks the fingerprint with the hub
	answer()
	if !r.c.isVerified("mina", fp) {
		t.Fatal("mina's device should now be marked verified")
	}
	if !strings.Contains(strings.Join(r.screenText(), "\n"), "✓ verified") {
		t.Fatal("screen should show the mark")
	}
	r.key(keyEnter) // /unverify is local
	if r.c.isVerified("mina", fp) {
		t.Fatal("second Enter removes the mark")
	}
	// a wrong fingerprint is refused
	r.a.closeOverlay()
	r.c.handleInput("/verify mina 0000-0000-0000-0000")
	answer()
	if r.c.isVerified("mina", "0000-0000-0000-0000") {
		t.Fatal("a fingerprint no online device has must not be marked")
	}
}
