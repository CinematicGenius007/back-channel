package main

// End-to-end tests against a real hub on a loopback port: accounts, channels, invites,
// the no-existence-oracle rule, moderation, guests, files, rate limits, persistence.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	hashIter = 1000 // keep tests fast; production uses 600k
	dummyHash = hashPassword("-")
	os.Exit(m.Run())
}

type testHub struct {
	h    *hub
	port int
	dir  string
	pass string // owner password
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func startHub(t *testing.T) *testHub {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "files"), 0o755)
	for {
		port := freePort(t)
		h := newHub(dir, 100, port)
		_, pass := h.loadOrInit()
		h.st.Settings.Limits.LoginRate = 100
		h.loadMessages()
		h.indexFiles()
		ln, err := h.listen(port)
		if err != nil {
			continue
		}
		hln, err := net.Listen("tcp", fmt.Sprintf(":%d", port+1))
		if err != nil {
			ln.Close()
			continue
		}
		go h.acceptLoop(ln)
		go h.acceptHTTP(hln)
		t.Cleanup(func() {
			ln.Close()
			hln.Close()
			h.mu.Lock()
			if h.logf != nil {
				h.logf.Close()
				h.logf = nil
			}
			h.mu.Unlock()
		})
		return &testHub{h: h, port: port, dir: dir, pass: pass}
	}
}

type tc struct {
	t     *testing.T
	conn  net.Conn
	sc    *bufio.Scanner
	rid   int
	queue []Msg // frames that arrived while waiting for a command reply
}

func (th *testHub) dial(t *testing.T) *tc {
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", th.port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	return &tc{t: t, conn: conn, sc: sc}
}

func (c *tc) send(m Msg) {
	b, _ := json.Marshal(m)
	c.conn.Write(append(b, '\n'))
}

// recv returns the next frame (queued ones first), failing after a timeout.
func (c *tc) recv() Msg {
	if len(c.queue) > 0 {
		m := c.queue[0]
		c.queue = c.queue[1:]
		return m
	}
	return c.recvNet()
}

// recvNet reads the next frame from the socket, bypassing the queue.
func (c *tc) recvNet() Msg {
	c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if !c.sc.Scan() {
		c.t.Fatalf("no frame: %v", c.sc.Err())
	}
	var m Msg
	if err := json.Unmarshal(c.sc.Bytes(), &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

// recvT skips frames until one of the given types arrives.
func (c *tc) recvT(types ...string) Msg {
	for i := 0; i < 200; i++ {
		m := c.recv()
		for _, ty := range types {
			if m.T == ty {
				return m
			}
		}
	}
	c.t.Fatalf("never got %v", types)
	return Msg{}
}

// expectNone asserts no frame arrives within d.
func (c *tc) expectNone(d time.Duration) {
	if len(c.queue) > 0 {
		c.t.Fatalf("unexpected frame: %+v", c.queue[0])
	}
	c.conn.SetReadDeadline(time.Now().Add(d))
	if c.sc.Scan() {
		c.t.Fatalf("unexpected frame: %s", c.sc.Text())
	}
	// a timed-out Scanner is stuck on its error; nothing was buffered, so start a fresh one
	c.sc = bufio.NewScanner(c.conn)
	c.sc.Buffer(make([]byte, 64<<10), 8<<20)
}

func (c *tc) login(user, pass string) Msg {
	c.send(Msg{T: "hello", V: 2, Device: "test", Auth: &Auth{User: user, Pass: pass}})
	return c.recvT("ok", "err")
}

func (c *tc) cmd(name, ch string, args map[string]string) Msg {
	c.rid++
	rid := fmt.Sprintf("r%d", c.rid)
	c.send(Msg{T: "cmd", RID: rid, Name: name, Ch: ch, Args: args})
	for i := 0; i < 200; i++ {
		m := c.recvNet()
		if m.T == "res" && m.RID == rid {
			return m
		}
		c.queue = append(c.queue, m) // an event that arrived before the reply; hand it out later
	}
	c.t.Fatal("no res")
	return Msg{}
}

func (c *tc) sub(ch string) {
	c.send(Msg{T: "sub", Ch: ch})
	c.recvT("synced", "err")
}

func mustOK(t *testing.T, m Msg) Msg {
	t.Helper()
	if !m.OK {
		t.Fatalf("command failed: %s %s", m.Code, m.Text)
	}
	return m
}

// ---- tests ------------------------------------------------------------------

func TestLoginAndSession(t *testing.T) {
	th := startHub(t)
	c := th.dial(t)
	if m := c.login("owner", "wrong"); m.T != "err" || m.Code != codeAuth {
		t.Fatalf("bad password accepted: %+v", m)
	}
	c = th.dial(t)
	ok := c.login("owner", th.pass)
	if ok.T != "ok" || ok.User != "owner" || ok.Role != "owner" || ok.Session == "" || ok.V != 2 {
		t.Fatalf("login: %+v", ok)
	}
	if len(ok.Channels) != 1 || ok.Channels[0].Name != "general" || ok.Channels[0].Role != "owner" {
		t.Fatalf("channels: %+v", ok.Channels)
	}
	// resume with the session token
	c2 := th.dial(t)
	c2.send(Msg{T: "hello", V: 2, Device: "test2", Auth: &Auth{Session: ok.Session}})
	if m := c2.recvT("ok", "err"); m.T != "ok" || m.Session != "" {
		t.Fatalf("session resume: %+v", m)
	}
	// revoked session is refused
	mustOK(t, c2.cmd("logout", "", nil))
	c3 := th.dial(t)
	c3.send(Msg{T: "hello", V: 2, Auth: &Auth{Session: ok.Session}})
	if m := c3.recvT("ok", "err"); m.T != "err" || m.Code != codeAuth {
		t.Fatalf("revoked session accepted: %+v", m)
	}
}

func TestChannelsInvitesAndSecrecy(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	owner.login("owner", th.pass)
	owner.sub("general")

	res := mustOK(t, owner.cmd("create", "", map[string]string{"name": "design", "topic": "pixels"}))
	if len(res.Channels) != 1 || res.Channels[0].Name != "design" || res.Channels[0].Role != "owner" {
		t.Fatalf("create: %+v", res)
	}
	owner.sub("design")
	mustOK(t, owner.cmd("create", "", map[string]string{"name": "private"}))

	// invite that allows sign-up
	inv := mustOK(t, owner.cmd("invite", "design", map[string]string{"uses": "1", "hours": "1"}))
	code := ""
	for _, l := range inv.Lines {
		if strings.HasPrefix(l, "invite code:") {
			code = strings.Fields(l)[2]
		}
	}
	if code == "" {
		t.Fatalf("no code in %v", inv.Lines)
	}

	// bob registers through the invite
	bob := th.dial(t)
	bob.send(Msg{T: "hello", V: 2, Device: "bobpc", Auth: &Auth{Invite: code, User: "Bob", Pass: "bobpassword"}})
	ok := bob.recvT("ok", "err")
	if ok.T != "ok" || ok.User != "bob" || ok.Session == "" {
		t.Fatalf("signup: %+v", ok)
	}
	if len(ok.Channels) != 1 || ok.Channels[0].Name != "design" || ok.Channels[0].Role != "member" {
		t.Fatalf("bob should only see #design: %+v", ok.Channels)
	}
	// the code was single-use
	x := th.dial(t)
	x.send(Msg{T: "hello", V: 2, Auth: &Auth{Invite: code, User: "eve", Pass: "evepassword"}})
	if m := x.recvT("ok", "err"); m.T != "err" {
		t.Fatalf("used invite accepted: %+v", m)
	}

	bob.sub("design")
	owner.recvT("join") // bob joined #design

	// messaging in a channel reaches subscribers of that channel only
	owner.send(Msg{T: "msg", Ch: "design", Text: "hello design"})
	if m := bob.recvT("msg"); m.Text != "hello design" || m.Ch != "design" || m.From != "owner" || m.ID == 0 {
		t.Fatalf("bob got %+v", m)
	}
	owner.recvT("msg")
	owner.send(Msg{T: "msg", Ch: "general", Text: "general only"})
	owner.recvT("msg")
	bob.expectNone(300 * time.Millisecond)

	// THE ORACLE RULE: not-a-member and does-not-exist must be byte-identical
	bob.send(Msg{T: "sub", Ch: "private", RID: "a"})
	e1 := bob.recv()
	bob.send(Msg{T: "sub", Ch: "nosuch", RID: "a"})
	e2 := bob.recv()
	e1.Ch, e2.Ch = "", ""
	b1, _ := json.Marshal(e1)
	b2, _ := json.Marshal(e2)
	if !bytes.Equal(b1, b2) || e1.Code != codeNoChan {
		t.Fatalf("existence oracle: %s vs %s", b1, b2)
	}
	r1 := bob.cmd("members", "private", nil)
	r2 := bob.cmd("members", "nosuch", nil)
	r1.RID, r2.RID = "", ""
	b1, _ = json.Marshal(r1)
	b2, _ = json.Marshal(r2)
	if !bytes.Equal(b1, b2) || r1.Code != codeNoChan {
		t.Fatalf("cmd existence oracle: %s vs %s", b1, b2)
	}
	// and bob cannot post to a channel he isn't in
	bob.send(Msg{T: "msg", Ch: "general", Text: "sneaky", RID: "z"})
	if m := bob.recvT("err"); m.Code != codeNoChan {
		t.Fatalf("post to non-member channel: %+v", m)
	}
	// channels list shows only memberships
	cl := mustOK(t, bob.cmd("channels", "", nil))
	if len(cl.Channels) != 1 {
		t.Fatalf("bob sees %d channels", len(cl.Channels))
	}

	// permissions: bob (member) can't set topic or kick
	if m := bob.cmd("topic", "design", map[string]string{"text": "x"}); m.OK || m.Code != codePerm {
		t.Fatalf("member set topic: %+v", m)
	}
	if m := bob.cmd("kick", "design", map[string]string{"user": "owner"}); m.OK {
		t.Fatalf("member kicked owner")
	}
	// promote bob to mod; mod still can't kick the owner (server owner is untouchable)
	mustOK(t, owner.cmd("role", "design", map[string]string{"user": "bob", "role": "mod"}))
	if m := bob.recvT("role"); m.Role != "mod" || m.From != "bob" {
		t.Fatalf("role event: %+v", m)
	}
	if m := bob.cmd("kick", "design", map[string]string{"user": "owner"}); m.OK || m.Code != codePerm {
		t.Fatalf("mod kicked server owner: %+v", m)
	}
	// mods may set the topic
	mustOK(t, bob.cmd("topic", "design", map[string]string{"text": "new topic"}))

	// kick bob: he is told, loses the channel, and can't come back without an invite
	mustOK(t, owner.cmd("kick", "design", map[string]string{"user": "bob", "reason": "test"}))
	if m := bob.recvT("kicked"); m.From != "bob" || m.By != "owner" || m.Text != "test" {
		t.Fatalf("kicked event: %+v", m)
	}
	bob.send(Msg{T: "sub", Ch: "design"})
	if m := bob.recv(); m.Code != codeNoChan {
		t.Fatalf("kicked user still subscribes: %+v", m)
	}
	// ban blocks invite redemption
	mustOK(t, owner.cmd("add", "design", map[string]string{"user": "bob"}))
	bob.recvT("invited")
	mustOK(t, owner.cmd("ban", "design", map[string]string{"user": "bob", "days": "1", "reason": "bye"}))
	bob.recvT("banned")
	inv = mustOK(t, owner.cmd("invite", "design", map[string]string{}))
	for _, l := range inv.Lines {
		if strings.HasPrefix(l, "invite code:") {
			code = strings.Fields(l)[2]
		}
	}
	if m := bob.cmd("join", "", map[string]string{"code": code}); m.OK {
		t.Fatal("banned user joined via invite")
	}
	if m := owner.cmd("add", "design", map[string]string{"user": "bob"}); m.OK {
		t.Fatal("banned user added")
	}
	mustOK(t, owner.cmd("unban", "design", map[string]string{"user": "bob"}))
	mustOK(t, bob.cmd("join", "", map[string]string{"code": code}))

	// audit log recorded the moderation
	au := mustOK(t, owner.cmd("audit", "", map[string]string{"n": "50"}))
	joined := strings.Join(au.Lines, "\n")
	for _, want := range []string{"kick", "ban", "unban", "role", "invite", "create"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit missing %q:\n%s", want, joined)
		}
	}
	// non-admin can't read it
	if m := bob.cmd("audit", "", nil); m.OK {
		t.Fatal("member read audit log")
	}
}

func TestMessageDeleteAndReplay(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	owner.login("owner", th.pass)
	owner.sub("general")
	owner.send(Msg{T: "msg", Ch: "general", Text: "one"})
	m1 := owner.recvT("msg")
	owner.send(Msg{T: "msg", Ch: "general", Text: "two"})
	m2 := owner.recvT("msg")
	mustOK(t, owner.cmd("del", "general", map[string]string{"id": fmt.Sprint(m1.ID)}))
	if d := owner.recvT("deleted"); d.ID != m1.ID {
		t.Fatalf("deleted event %+v", d)
	}
	// a client that was offline replays the tombstone, not the text
	late := th.dial(t)
	late.login("owner", th.pass)
	late.send(Msg{T: "sub", Ch: "general", Since: 0})
	var got []Msg
	for {
		m := late.recv()
		if m.T == "synced" {
			break
		}
		got = append(got, m)
	}
	if len(got) != 2 || got[0].T != "deleted" || got[0].Text != "" || got[1].ID != m2.ID || !got[1].Hist {
		t.Fatalf("replay: %+v", got)
	}
	// since=m1 replays only m2
	late.send(Msg{T: "sub", Ch: "general", Since: m1.ID})
	if m := late.recv(); m.ID != m2.ID {
		t.Fatalf("since replay: %+v", m)
	}
	late.recvT("synced")

	// persistence: restart the hub from the same dir
	th.h.mu.Lock()
	th.h.save()
	th.h.mu.Unlock()
	h2 := newHub(th.dir, 100, 0)
	first, _ := h2.loadOrInit()
	if first {
		t.Fatal("state.json not found on restart")
	}
	h2.loadMessages()
	defer h2.logf.Close()
	gen := h2.defaultChan()
	if gen == nil || len(h2.msgs[gen.ID]) != 2 || h2.msgs[gen.ID][0].T != "deleted" || h2.nextID != m2.ID {
		t.Fatalf("reload: %+v next=%d", h2.msgs[gen.ID], h2.nextID)
	}
	if h2.userByName("owner") == nil {
		t.Fatal("owner lost on reload")
	}
}

func TestReadOnlyMuteAndRateLimit(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	owner.login("owner", th.pass)
	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "carol", "pass": "carolpass1"}))
	carol := th.dial(t)
	carol.login("carol", "carolpass1")
	carol.sub("general")
	owner.sub("general")
	owner.recvT("join")

	// read-only channel: members can't post, mods can
	mustOK(t, owner.cmd("settings", "general", map[string]string{"readonly": "on"}))
	carol.send(Msg{T: "msg", Ch: "general", Text: "x", RID: "1"})
	if m := carol.recvT("err"); m.Code != codeReadonly {
		t.Fatalf("readonly: %+v", m)
	}
	mustOK(t, owner.cmd("settings", "general", map[string]string{"readonly": "off"}))

	// mute
	mustOK(t, owner.cmd("mute", "general", map[string]string{"user": "carol", "minutes": "5"}))
	carol.recvT("muted")
	carol.send(Msg{T: "msg", Ch: "general", Text: "x", RID: "2"})
	if m := carol.recvT("err"); m.Code != codeMuted {
		t.Fatalf("muted: %+v", m)
	}
	mustOK(t, owner.cmd("mute", "general", map[string]string{"user": "carol", "minutes": "0"}))
	carol.recvT("muted")

	// rate limit: 2 msg/s burst 3 → the 4th instant message is refused, then auto-mute after many hits
	th.h.mu.Lock()
	th.h.st.Settings.Limits.MsgRate, th.h.st.Settings.Limits.MsgBurst = 0.001, 3
	th.h.st.Settings.Limits.AutoMuteHits = 5
	th.h.mu.Unlock()
	for i := 0; i < 3; i++ {
		carol.send(Msg{T: "msg", Ch: "general", Text: fmt.Sprint("m", i)})
		carol.recvT("msg")
	}
	limited := false
	for i := 0; i < 8; i++ {
		carol.send(Msg{T: "msg", Ch: "general", Text: "flood", RID: "f"})
		m := carol.recvT("err", "muted")
		if m.T == "err" && m.Code == codeRateLimit {
			limited = true
		}
		if m.T == "muted" && m.From == "carol" && m.By == "hub" {
			return // auto-mute kicked in
		}
	}
	if !limited {
		t.Fatal("never rate limited")
	}
	t.Fatal("flooding never triggered auto-mute")
}

func TestGuestAccessAndV1Client(t *testing.T) {
	th := startHub(t)
	th.h.mu.Lock()
	th.h.st.Settings.GuestAccess, th.h.st.Settings.LegacyToken = true, "tok123"
	th.h.mu.Unlock()
	owner := th.dial(t)
	owner.login("owner", th.pass)
	owner.sub("general")

	// v1-shaped hello: no `v`, token + name + since
	v1 := th.dial(t)
	v1.send(Msg{T: "hello", Name: "oldpc", Token: "tok123", Since: 0})
	ok := v1.recvT("ok")
	if ok.V != 0 || ok.Name != "oldpc" || len(ok.Users) == 0 {
		t.Fatalf("v1 ok: %+v", ok)
	}
	owner.recvT("join")
	v1.send(Msg{T: "msg", Text: "from v1"}) // no ch → default channel
	if m := owner.recvT("msg"); m.Text != "from v1" || m.From != "oldpc" || m.Ch != "general" {
		t.Fatalf("v1 msg: %+v", m)
	}
	v1.recvT("msg")
	// guests can't run commands or reach other channels
	mustOK(t, owner.cmd("create", "", map[string]string{"name": "staff"}))
	v1.send(Msg{T: "cmd", RID: "g", Name: "create", Args: map[string]string{"name": "x"}})
	if m := v1.recvT("res"); m.OK || m.Code != codeGuest {
		t.Fatalf("guest ran a command: %+v", m)
	}
	v1.send(Msg{T: "sub", Ch: "staff"})
	if m := v1.recvT("err"); m.Code != codeNoChan {
		t.Fatalf("guest saw a channel: %+v", m)
	}
	// wrong token
	bad := th.dial(t)
	bad.send(Msg{T: "hello", Name: "x", Token: "nope"})
	if m := bad.recvT("err"); m.Code != codeAuth {
		t.Fatalf("bad token: %+v", m)
	}
	// guest access off → refused
	th.h.mu.Lock()
	th.h.st.Settings.GuestAccess = false
	th.h.mu.Unlock()
	off := th.dial(t)
	off.send(Msg{T: "hello", V: 2, Name: "x", Token: "tok123"})
	if m := off.recvT("err"); m.Code != codeAuth {
		t.Fatalf("guest access off: %+v", m)
	}
}

func TestFilesMembershipDedupQuota(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	ok := owner.login("owner", th.pass)
	mustOK(t, owner.cmd("create", "", map[string]string{"name": "design"}))
	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "dave", "pass": "davepass1"}))
	dave := th.dial(t)
	dok := dave.login("dave", "davepass1") // member of #general only
	owner.sub("design")

	httpAddr := fmt.Sprintf("127.0.0.1:%d", th.port+1)
	dial := func(a string) (net.Conn, error) { return net.DialTimeout("tcp", a, 2*time.Second) }
	auth := func(sess string) map[string]string { return map[string]string{"Authorization": "Bearer " + sess} }
	content := []byte("hello file content")

	put := func(sess, ch string, body []byte) (int, string) {
		st, _, rd, conn, err := httpDo(dial, httpAddr, "PUT", "/up?ch="+ch+"&name=a.txt", auth(sess), bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		b, _ := io.ReadAll(rd)
		return st, string(b)
	}
	get := func(sess, fid string) int {
		st, _, rd, conn, err := httpDo(dial, httpAddr, "GET", "/f/"+fid, auth(sess), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.ReadAll(rd)
		return st
	}

	// dave can't upload into #design (not a member) — same text as nonexistent
	if st, body := put(dok.Session, "design", content); st != 404 || !strings.Contains(body, textNoChan) {
		t.Fatalf("non-member upload: %d %s", st, body)
	}
	if st, _ := put(dok.Session, "nosuch", content); st != 404 {
		t.Fatalf("nonexistent upload: %d", st)
	}
	// owner uploads; subscribers get a file event
	st, body := put(ok.Session, "design", content)
	if st != 200 {
		t.Fatalf("upload: %d %s", st, body)
	}
	var r struct{ FID string }
	json.Unmarshal([]byte(body), &r)
	ev := owner.recvT("file")
	if ev.FID != r.FID || ev.Size != int64(len(content)) || ev.Ch != "design" {
		t.Fatalf("file event: %+v", ev)
	}
	// dave can't download it (no membership in a channel that references it)
	if st := get(dok.Session, r.FID); st != 404 {
		t.Fatalf("non-member download: %d", st)
	}
	if st := get(ok.Session, r.FID); st != 200 {
		t.Fatalf("member download: %d", st)
	}
	if st := get("badsession", r.FID); st != 401 {
		t.Fatalf("no auth download: %d", st)
	}
	// dedup: same bytes from dave into #general reuse the fid, dave is charged nothing
	st, body = put(dok.Session, "general", content)
	if st != 200 || !strings.Contains(body, r.FID) {
		t.Fatalf("dedup: %d %s", st, body)
	}
	th.h.mu.Lock()
	used := th.h.used[th.h.userByName("dave").ID]
	files := len(th.h.st.Files)
	th.h.mu.Unlock()
	if used != 0 || files != 1 {
		t.Fatalf("dedup accounting: used=%d files=%d", used, files)
	}
	// now dave IS a member of a channel referencing the file → may download
	if st := get(dok.Session, r.FID); st != 200 {
		t.Fatalf("download via general: %d", st)
	}
	// quota: 1 MB quota, 2 MB upload refused before the body is sent
	mustOK(t, owner.cmd("usermod", "", map[string]string{"user": "dave", "quota_mb": "1"}))
	big := bytes.Repeat([]byte("x"), 2<<20)
	if st, body := put(dok.Session, "general", big); st != 413 || !strings.Contains(body, "quota") {
		t.Fatalf("quota: %d %s", st, body)
	}
	// per-channel max file
	mustOK(t, owner.cmd("settings", "design", map[string]string{"max_file_mb": "1"}))
	if st, _ := put(ok.Session, "design", big); st != 413 {
		t.Fatalf("channel max file: %d", st)
	}
	// deleting the only referencing messages removes the file and refunds
	mustOK(t, owner.cmd("purge", "design", map[string]string{"user": "owner"}))
	owner.recvT("purged")
	mustOK(t, owner.cmd("purge", "general", map[string]string{"user": "dave"}))
	th.h.mu.Lock()
	files = len(th.h.st.Files)
	ownerUsed := th.h.used[th.h.userByName("owner").ID]
	th.h.mu.Unlock()
	if files != 0 || ownerUsed != 0 {
		t.Fatalf("file not reclaimed: files=%d used=%d", files, ownerUsed)
	}
	if st := get(ok.Session, r.FID); st != 404 {
		t.Fatalf("deleted file still served: %d", st)
	}
}

func TestClipboardRoutingAndRead(t *testing.T) {
	th := startHub(t)
	mac := th.dial(t)
	ok := mac.login("owner", th.pass)
	win := th.dial(t)
	win.send(Msg{T: "hello", V: 2, Device: "win", Auth: &Auth{Session: ok.Session}})
	win.recvT("ok")
	mustOK(t, mac.cmd("useradd", "", map[string]string{"name": "erin", "pass": "erinpass1"}))
	erin := th.dial(t)
	erin.login("erin", "erinpass1")

	mac.send(Msg{T: "clip", Text: "secret paste"})
	if m := win.recvT("clip"); m.Text != "secret paste" || m.From != "test" {
		t.Fatalf("clip: %+v", m)
	}
	mac.expectNone(200 * time.Millisecond)  // not echoed to the sender
	erin.expectNone(200 * time.Millisecond) // never to another user
	th.h.mu.Lock()
	n := len(th.h.msgs[th.h.defaultChan().ID])
	th.h.mu.Unlock()
	if n != 0 {
		t.Fatal("clip was stored in history")
	}

	// read sync between devices
	mac.sub("general")
	win.sub("general")
	mac.send(Msg{T: "msg", Ch: "general", Text: "hi"})
	m := mac.recvT("msg")
	win.recvT("msg")
	mac.send(Msg{T: "read", Ch: "general", ID: m.ID})
	if r := win.recvT("read"); r.ID != m.ID || r.Ch != "general" {
		t.Fatalf("read sync: %+v", r)
	}
	// a fresh login reports zero unread
	c := th.dial(t)
	if o := c.login("owner", th.pass); o.Channels[0].Unread != 0 || o.Channels[0].Last != m.ID {
		t.Fatalf("unread: %+v", o.Channels[0])
	}
}

func TestServerAdminAndOwnerRules(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	owner.login("owner", th.pass)
	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "adam", "pass": "adampass1"}))
	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "zed", "pass": "zedpass11"}))
	adam := th.dial(t)
	adam.login("adam", "adampass1")
	// plain user can't do server admin things
	if m := adam.cmd("useradd", "", map[string]string{"name": "x", "pass": "xxxxxxxx"}); m.OK || m.Code != codePerm {
		t.Fatalf("user ran useradd: %+v", m)
	}
	if m := adam.cmd("create", "", map[string]string{"name": "mine"}); m.OK {
		t.Fatal("user created channel with allow_user_channels off")
	}
	mustOK(t, owner.cmd("set", "", map[string]string{"allow_user_channels": "on"}))
	mustOK(t, adam.cmd("create", "", map[string]string{"name": "mine"}))
	// only the owner promotes admins; admins can't ban the owner or each other
	if m := adam.cmd("usermod", "", map[string]string{"user": "zed", "role": "admin"}); m.OK {
		t.Fatal("user promoted someone")
	}
	mustOK(t, owner.cmd("usermod", "", map[string]string{"user": "adam", "role": "admin"}))
	if m := adam.cmd("userban", "", map[string]string{"user": "owner"}); m.OK || m.Code != codePerm {
		t.Fatalf("admin banned owner: %+v", m)
	}
	mustOK(t, adam.cmd("userban", "", map[string]string{"user": "zed", "reason": "spam"}))
	zed := th.dial(t)
	if m := zed.login("zed", "zedpass11"); m.T != "err" || m.Code != codeBanned {
		t.Fatalf("banned user logged in: %+v", m)
	}
	mustOK(t, adam.cmd("userunban", "", map[string]string{"user": "zed"}))
	zed = th.dial(t)
	if m := zed.login("zed", "zedpass11"); m.T != "ok" {
		t.Fatalf("unbanned user refused: %+v", m)
	}
	// server admin sees every channel and can act inside it
	cl := mustOK(t, adam.cmd("channels", "", nil))
	names := []string{}
	for _, c := range cl.Channels {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "mine") || !strings.Contains(strings.Join(names, ","), "general") {
		t.Fatalf("admin channel list: %v", names)
	}
	// passwd
	if m := adam.cmd("passwd", "", map[string]string{"old": "wrong", "new": "newpass123"}); m.OK {
		t.Fatal("passwd with wrong old password")
	}
	mustOK(t, adam.cmd("passwd", "", map[string]string{"old": "adampass1", "new": "newpass123"}))
	a2 := th.dial(t)
	if m := a2.login("adam", "newpass123"); m.T != "ok" {
		t.Fatalf("new password refused: %+v", m)
	}
	// admin password reset logs the user out everywhere and the new password works
	mustOK(t, adam.cmd("usermod", "", map[string]string{"user": "zed", "pass": "resetpass99"}))
	if m := zed.recvT("err"); m.Code != codeAuth {
		t.Fatalf("reset should log zed out: %+v", m)
	}
	z3 := th.dial(t)
	if m := z3.login("zed", "resetpass99"); m.T != "ok" {
		t.Fatalf("reset password refused: %+v", m)
	}
	// stats and settings listing work
	mustOK(t, owner.cmd("stats", "", nil))
	if m := mustOK(t, owner.cmd("set", "", nil)); len(m.Lines) < 3 {
		t.Fatal("settings listing")
	}
}

func TestPasswordHashing(t *testing.T) {
	h := hashPassword("correct horse")
	if !checkPassword(h, "correct horse") || checkPassword(h, "wrong") || checkPassword("garbage", "x") {
		t.Fatal("password hashing broken")
	}
	if !strings.HasPrefix(h, "$pbkdf2-sha256$") {
		t.Fatal(h)
	}
}
