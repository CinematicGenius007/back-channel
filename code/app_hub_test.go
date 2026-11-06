package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAppSetupAgainstRealHub drives the setup screens with key events against a hub on
// loopback: sign up with an invite, pick a theme, open the main screen, connect, and
// send a message the channel owner receives.
func TestAppSetupAgainstRealHub(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	owner.login("owner", th.pass)
	mustOK(t, owner.cmd("create", "", map[string]string{"name": "design"}))
	owner.sub("design")
	inv := mustOK(t, owner.cmd("invite", "design", map[string]string{"uses": "1", "hours": "1"}))
	code := ""
	for _, l := range inv.Lines {
		if strings.HasPrefix(l, "invite code:") {
			code = strings.Fields(l)[2]
		}
	}

	a := newApp()
	a.out = &bytes.Buffer{}
	a.w, a.h = 100, 30
	dir := t.TempDir()
	s := &setupFlow{cfg: Config{Hub: fmt.Sprintf("127.0.0.1:%d", th.port), Name: "laptop", AutoMB: 50}, dir: dir,
		results: make(chan setupResult, 4), focus: 1}
	s.name.set("laptop")
	a.setup, a.mode = s, scrSetup
	press := func(k keyCode) { a.handle(event{kind: evKey, key: k}); a.render() }
	typ := func(str string) {
		for _, r := range str {
			a.handle(event{kind: evKey, key: keyRune, r: r})
		}
	}
	wait := func() {
		select {
		case r := <-s.results:
			a.setupResult(r)
		case <-time.After(5 * time.Second):
			t.Fatal("setup result never arrived")
		}
	}

	a.render()
	press(keyEnter) // hub: the saved address is selected
	if s.step != stDevice || s.c == nil {
		t.Fatalf("after hub: step %d, err %q", s.step, s.err)
	}
	press(keyEnter) // device: keep "laptop"
	if s.step != stAccount {
		t.Fatalf("after device: step %d, err %q", s.step, s.err)
	}
	// switch to the invite tab with the mode selector, then fill the four fields
	s.focus = 0
	press(keyRight)
	if s.mode != 1 {
		t.Fatalf("mode %d", s.mode)
	}
	press(keyTab)
	typ(code)
	press(keyTab)
	typ("carol")
	press(keyTab)
	typ("carolpassword")
	press(keyTab)
	typ("carolpassword")
	press(keyEnter)
	if !s.busy {
		t.Fatalf("sign-up did not start: %q", s.err)
	}
	wait()
	if s.step != stLook || s.c.snapshot().Session == "" || s.c.snapshot().User != "carol" {
		t.Fatalf("after sign-up: step %d err %q cfg %+v", s.step, s.err, s.c.snapshot())
	}
	press(keyDown) // site light
	press(keyEnter)
	press(keyEnter) // ready → open
	if !s.done {
		t.Fatalf("setup not done: step %d", s.step)
	}
	saved := loadConfig(dir)
	if saved.Theme != "site-light" || saved.Session == "" || saved.Name != "laptop" {
		t.Fatalf("saved config: %+v", saved)
	}

	// main screen against the live hub
	c := s.c
	a.c, c.ui, a.mode, a.setup = c, a, scrMain, nil
	c.active = c.ensureChan("…")
	go c.connectLoop()
	defer func() {
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
		}
		c.mu.Unlock()
	}()
	deadline := time.After(5 * time.Second)
	for c.active == nil || c.active.name != "design" {
		select {
		case m := <-c.events:
			c.handleEvent(m)
			a.flush()
		case <-deadline:
			t.Fatal("never joined #design")
		}
	}
	typ("hello from the app")
	press(keyEnter)
	for i := 0; i < 50; i++ {
		m := owner.recv()
		if m.T == "msg" && m.From == "carol" && m.Text == "hello from the app" {
			return
		}
	}
	t.Fatal("owner never received the message")
}
