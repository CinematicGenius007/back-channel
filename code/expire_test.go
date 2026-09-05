package main

import (
	"bytes"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestParseExpire(t *testing.T) {
	cases := map[string]int64{"10m": 600, "2h": 7200, "7d": 7 * 86400, "1w": 7 * 86400, "off": 0, "0": 0, "30s": 60}
	for in, want := range cases {
		got, ok := parseExpire(in)
		if !ok || got != want {
			t.Errorf("parseExpire(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"abc", "-5m", "5x", "m"} {
		if _, ok := parseExpire(bad); ok {
			t.Errorf("parseExpire(%q) accepted", bad)
		}
	}
	for _, s := range []string{"10m", "2h", "3d", "1w", "off"} {
		sec, _ := parseExpire(s)
		if fmtExpire(sec) != s {
			t.Errorf("fmtExpire(%d) = %s want %s", sec, fmtExpire(sec), s)
		}
	}
}

// Messages (and their files) in a self-destruct channel vanish after expire_sec, and
// subscribers are told which ids went away.
func TestSelfDestructChannel(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	ok := owner.login("owner", th.pass)
	res := mustOK(t, owner.cmd("create", "", map[string]string{"name": "burn", "expire": "10m"}))
	if res.Channels[0].Expire != 600 {
		t.Fatalf("expire not set: %+v", res.Channels[0])
	}
	owner.sub("burn")

	// a bad value is refused; a valid change is broadcast to subscribers
	if m := owner.cmd("settings", "burn", map[string]string{"expire": "soon"}); m.OK {
		t.Fatal("bad expire accepted")
	}
	mustOK(t, owner.cmd("settings", "burn", map[string]string{"expire": "2h"}))
	if ev := owner.recvT("settings"); ev.Channels[0].Expire != 7200 {
		t.Fatalf("settings event: %+v", ev)
	}

	owner.send(Msg{T: "msg", Ch: "burn", Text: "gone soon"})
	m1 := owner.recvT("msg")
	// upload a file into the channel
	httpAddr := fmt.Sprintf("127.0.0.1:%d", th.port+1)
	dial := func(a string) (net.Conn, error) { return net.DialTimeout("tcp", a, 2*time.Second) }
	body := []byte("burn after reading")
	st, _, _, conn, err := httpDo(dial, httpAddr, "PUT", "/up?ch=burn&name=b.txt", map[string]string{"Authorization": "Bearer " + ok.Session}, bytes.NewReader(body), int64(len(body)))
	if err != nil || st != 200 {
		t.Fatalf("upload: %v %d", err, st)
	}
	conn.Close()
	fev := owner.recvT("file")
	owner.send(Msg{T: "msg", Ch: "burn", Text: "still fresh"})
	m3 := owner.recvT("msg")

	// backdate the first two messages past the expiry and run the sweep
	th.h.mu.Lock()
	ch := th.h.chanByName("burn")
	old := nowMs() - 3*3600*1000
	th.h.msgs[ch.ID][0].TS = old
	th.h.msgs[ch.ID][1].TS = old
	th.h.expireMessages()
	remaining := len(th.h.msgs[ch.ID])
	fileGone := th.h.st.Files[fev.FID] == nil
	th.h.mu.Unlock()

	ev := owner.recvT("expired")
	if len(ev.IDs) != 2 || ev.IDs[0] != m1.ID || ev.IDs[1] != fev.ID || ev.Ch != "burn" {
		t.Fatalf("expired event: %+v", ev)
	}
	if remaining != 1 || !fileGone {
		t.Fatalf("remaining=%d fileGone=%v", remaining, fileGone)
	}
	if matches, _ := filepath.Glob(filepath.Join(th.dir, "files", fev.FID+"_*")); len(matches) != 0 {
		t.Fatal("expired file still on disk")
	}
	// a fresh subscriber only sees the survivor
	late := th.dial(t)
	late.login("owner", th.pass)
	late.send(Msg{T: "sub", Ch: "burn"})
	first := late.recv()
	if first.ID != m3.ID {
		t.Fatalf("replay after expiry: %+v", first)
	}
}

func TestClientForgetAndLocalExpiry(t *testing.T) {
	ch := &chanState{name: "x", texts: map[int64]string{}, files: map[int64]Msg{}, saved: map[int64]string{}, expire: 60}
	now := nowMs()
	ch.lines = []line{{id: 1, ts: now - 120_000, text: "old"}, {id: 0, ts: now - 120_000, text: "old sys"}, {id: 2, ts: now, text: "new"}}
	ch.texts[1], ch.texts[2] = "old", "new"
	if n := ch.expireLocal(); n != 2 || len(ch.lines) != 1 || ch.lines[0].id != 2 || ch.texts[1] != "" {
		t.Fatalf("expireLocal: n=%d lines=%+v", n, ch.lines)
	}
	if n := ch.forget([]int64{2}); n != 1 || len(ch.lines) != 0 || len(ch.texts) != 0 {
		t.Fatalf("forget: n=%d", n)
	}
}
