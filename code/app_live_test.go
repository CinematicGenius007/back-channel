package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveApp is an app TUI on a real client connected to a real hub on loopback.
type liveApp struct {
	a        *app
	c        *client
	statuses []string // every status-line text the client produced
}

func startLiveApp(t *testing.T, th *testHub, device string) *liveApp {
	t.Helper()
	c := newClient(Config{Hub: fmt.Sprintf("127.0.0.1:%d", th.port), Name: device, AutoMB: 50}, t.TempDir())
	ok, err := c.loginOnce(Auth{User: "owner", Pass: th.pass})
	if err != nil {
		t.Fatal(err)
	}
	c.update(func(cf *Config) { cf.User, cf.Session = ok.User, ok.Session })
	a := newApp()
	a.out = &bytes.Buffer{}
	a.w, a.h = 110, 32
	a.c, c.ui, a.mode = c, a, scrMain
	c.active = c.ensureChan("…")
	go c.connectLoop()
	t.Cleanup(func() {
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
		}
		c.mu.Unlock()
	})
	return &liveApp{a: a, c: c}
}

// pump runs the event loops of every app until cond holds.
func pump(t *testing.T, what string, cond func() bool, apps ...*liveApp) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		for _, l := range apps {
		drain:
			for {
				select {
				case m := <-l.c.events:
					if m.T == "_status" && m.Text != "" {
						l.statuses = append(l.statuses, m.Text)
					}
					l.c.handleEvent(m)
					l.a.afterTurn()
				default:
					break drain
				}
			}
			l.a.flush()
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (l *liveApp) tapText(t *testing.T, needle string) {
	t.Helper()
	l.a.prev = nil
	l.a.render()
	for y, row := range l.a.prev {
		if strings.Contains(escRE.ReplaceAllString(row, ""), needle) {
			l.a.handle(event{kind: evMouse, act: mPress, x: l.a.paneX + 4, y: y})
			l.a.handle(event{kind: evMouse, act: mRelease, x: l.a.paneX + 4, y: y})
			return
		}
	}
	t.Fatalf("%q not on screen", needle)
}

// TestAppLiveFilesFindAndVerify runs the new app features end to end against a real
// hub: upload with progress, tap-to-download, tap-to-open, find, and the verify
// screen marking a second device of the same account.
func TestAppLiveFilesFindAndVerify(t *testing.T) {
	var opened []string
	old := openFolder
	openFolder = func(p string) { opened = append(opened, p) }
	defer func() { openFolder = old }()

	th := startHub(t)
	raw := th.dial(t)
	raw.login("owner", th.pass)
	mustOK(t, raw.cmd("create", "", map[string]string{"name": "drop"}))

	pc := startLiveApp(t, th, "win")
	phone := startLiveApp(t, th, "phone")
	pump(t, "both clients connected with #drop", func() bool {
		return pc.c.chanByName("drop") != nil && phone.c.chanByName("drop") != nil && pc.c.online.Load() && phone.c.online.Load()
	}, pc, phone)
	pc.c.switchTo(pc.c.chanByName("drop"))

	// upload: the status line shows a progress bar
	src := filepath.Join(t.TempDir(), "report.bin")
	os.WriteFile(src, bytes.Repeat([]byte("backchannel "), 40000), 0o644)
	pc.a.submit("/send " + src)
	drop := pc.c.chanByName("drop")
	pump(t, "file line in #drop", func() bool { return len(drop.files) == 1 }, pc, phone)
	sawBar := false
	for _, s := range pc.statuses {
		if strings.HasPrefix(s, "uploading report.bin to #drop  [") {
			sawBar = true
		}
	}
	if !sawBar {
		t.Fatalf("no upload progress in the status line: %q", pc.statuses)
	}

	// tap the file line: our own upload isn't auto-downloaded, so this runs /get
	var id int64
	for k := range drop.files {
		id = k
	}
	pc.statuses = nil
	pc.tapText(t, "report.bin")
	pump(t, "download saved", func() bool { return drop.saved[id] != "" }, pc, phone)
	got, _ := os.ReadFile(drop.saved[id])
	want, _ := os.ReadFile(src)
	if !bytes.Equal(got, want) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(want))
	}
	if len(pc.statuses) == 0 || !strings.HasPrefix(pc.statuses[0], "downloading report.bin") {
		t.Fatalf("no download progress: %q", pc.statuses)
	}
	// tap again: opens the folder it was saved in
	pc.tapText(t, "report.bin")
	if len(opened) != 1 || opened[0] != filepath.Dir(drop.saved[id]) {
		t.Fatalf("opened %q, want %q", opened, filepath.Dir(drop.saved[id]))
	}

	// find
	pc.a.submit("/find REPORT")
	// the file line plus the "saved …" and "opened the folder holding …" notes
	if n := len(pc.a.findMatches()); n != 3 {
		t.Fatalf("find REPORT: %d matches", n)
	}
	pc.a.findOn = false

	// verify: an e2e channel, then mark the phone's key from the PC's verify screen
	// (the phone needn't know #vault exists: e2epeers lists every connected device of a member)
	pc.a.submit("/create vault e2e=on")
	pump(t, "#vault created and keyed", func() bool {
		v := pc.c.chanByName("vault")
		if v == nil {
			return false
		}
		_, k := pc.c.chanKey("vault", v.epoch)
		return k
	}, pc, phone)
	pc.c.switchTo(pc.c.chanByName("vault"))
	pc.a.submit("/verify")
	if pc.a.ov != ovVerify {
		t.Fatal("verify screen did not open")
	}
	pump(t, "peer list", func() bool { return !pc.a.ver.loading }, pc, phone)
	if pc.a.ver.err != "" || len(pc.a.ver.peers) != 1 || pc.a.ver.peers[0].Device != "phone" {
		t.Fatalf("peers: %+v err %q", pc.a.ver.peers, pc.a.ver.err)
	}
	phoneFP := keyFingerprint(phone.c.snapshot().E2EPub)
	if keyFingerprint(pc.a.ver.peers[0].Pub) != phoneFP {
		t.Fatal("the hub relayed a different key for the phone than the phone holds")
	}
	pc.a.handle(event{kind: evKey, key: keyEnter})
	pump(t, "phone marked verified", func() bool { return pc.c.isVerified("owner", phoneFP) }, pc, phone)
	if !strings.Contains(strings.Join(pc.a.prev, ""), "✓ verified") {
		pc.a.prev = nil
		pc.a.render()
		if !strings.Contains(strings.Join(pc.a.prev, ""), "✓ verified") {
			t.Fatal("verify screen should show the mark")
		}
	}
	if loadConfig(pc.c.dir).Verified[verifyKey("owner", phoneFP)] != true {
		t.Fatal("mark not saved to config.json")
	}
}
