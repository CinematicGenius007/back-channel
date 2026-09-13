package main

// The hub: one process, one port, many channels. Each client holds one TCP connection
// (a "session") and a set of channel subscriptions; fan-out is "every session
// subscribed to this channel". A single mutex guards all state — at this scale
// (hundreds of users, thousands of messages/hour) contention is a non-issue and the
// simplicity buys correctness.

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const replayCap = 400 // max messages replayed per channel per subscribe

type hub struct {
	mu       sync.Mutex
	dir      string
	st       *State
	byName   map[string]*User
	chByName map[string]*Channel
	msgs     map[int64][]Msg // channel id → messages (last `keep`), ascending id
	nextID   int64
	keep     int
	logf     *os.File
	auditLog []auditRec
	dirty    bool
	migrated []string
	started  time.Time

	// files
	fileRefs   map[string]map[int64]int // fid → channel id → referencing messages
	used       map[int64]int64          // user id → bytes charged against quota
	hashes     map[string]string        // sha256 → fid (dedup)
	totalBytes int64
	uploading  map[int64]int // user id → concurrent uploads

	// live
	sessions map[*session]struct{}
	presence map[int64]map[int64]int // channel id → user id → subscribed sessions
	rateHits map[string][]int64
	removed  int       // messages expired since the last compaction
	lastComp time.Time // last compaction

	// transport / limits
	tlsCfg    *tls.Config
	fp        string
	tcpPort   int
	msgLim    *limiter // (user, channel) message rate
	upLim     *limiter // per user uploads
	connLim   *limiter // per IP connections
	loginLim  *limiter // per username failed logins
	inviteLim *limiter // per user invite creation
}

type session struct {
	h        *hub
	conn     net.Conn
	out      chan []byte
	ip       string
	user     *User // never nil; guests get a transient User{Role:"guest"}
	guest    bool
	v        int
	device   string
	sessHash string
	subs     map[int64]bool
	closed   bool // set under h.mu when the session is torn down
}

func newHub(dir string, keep, port int) *hub {
	return &hub{dir: dir, keep: keep, tcpPort: port, started: time.Now(),
		msgs: map[int64][]Msg{}, fileRefs: map[string]map[int64]int{}, used: map[int64]int64{},
		hashes: map[string]string{}, uploading: map[int64]int{}, sessions: map[*session]struct{}{},
		presence: map[int64]map[int64]int{}, rateHits: map[string][]int64{},
		msgLim: newLimiter(5, 20), upLim: newLimiter(0.5, 5), connLim: newLimiter(0.33, 10),
		loginLim: newLimiter(0.083, 5), inviteLim: newLimiter(0.003, 10)}
}

func runHub(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 7777, "TCP port (HTTP files = port+1, LAN discovery UDP = port+2)")
	dir := fs.String("dir", filepath.Join(defaultDir(), "hub"), "data dir (state, messages, files)")
	keep := fs.Int("history", 2000, "messages kept per channel for replay")
	useTLS := fs.Bool("tls", false, "encrypt with a self-signed cert; clients pin its fingerprint")
	public := fs.Bool("public", false, "shorthand for -tls plus stricter default limits (use on a VPS)")
	resetOwner := fs.Bool("reset-owner", false, "set and print a new password for the owner account, then exit")
	token := fs.String("token", envOr("BCH_TOKEN", ""), "legacy shared token for guest access (turns guest_access on)")
	guest := fs.String("guest", "", "guest access: on|off (v1 clients and -token users land in the default channel)")
	maxMsg := fs.Int("max-msg", 64, "max text message size, KB")
	maxFile := fs.Int64("max-file", 2048, "max upload size, MB")
	quota := fs.Int64("quota", 5120, "storage quota per user, MB (0 = unlimited)")
	msgRate := fs.Float64("msg-rate", 5, "text messages/second per user per channel (burst 20)")
	upRate := fs.Float64("upload-rate", 30, "uploads/minute per user")
	connRate := fs.Float64("conn-rate", 20, "new connections/minute per IP (stops brute force)")
	fs.Parse(args)

	os.MkdirAll(filepath.Join(*dir, "files"), 0o755)
	dummyHash = hashPassword("-")
	h := newHub(*dir, *keep, *port)
	first, ownerPass := h.loadOrInit()
	S := &h.st.Settings

	if *resetOwner {
		pw := randAlnum(14)
		for _, u := range h.st.Users {
			if u.Role == "owner" {
				u.Pass = hashPassword(pw)
				u.Status = "active"
				h.save()
				fmt.Printf("owner account %q — new password: %s\n", u.Name, pw)
				return
			}
		}
		fmt.Println("no owner account found")
		return
	}

	if first && *public { // stricter defaults for an internet-facing hub, only on first start
		S.Limits.MsgRate, S.Limits.UploadRate = 3, 12
	}
	fs.Visit(func(f *flag.Flag) { // explicit flags always win and are persisted
		switch f.Name {
		case "max-msg":
			S.Limits.MaxMsgKB = *maxMsg
		case "max-file":
			S.Limits.MaxFileMB = *maxFile
		case "quota":
			S.Limits.QuotaMB = *quota
		case "msg-rate":
			S.Limits.MsgRate = *msgRate
		case "upload-rate":
			S.Limits.UploadRate = *upRate
		case "conn-rate":
			S.Limits.ConnRate = *connRate
		case "token":
			S.LegacyToken, S.GuestAccess = *token, true
		case "guest":
			S.GuestAccess = *guest == "on"
		}
	})
	if S.GuestAccess && S.LegacyToken == "" {
		S.LegacyToken = randHex(6)
	}
	if *public {
		*useTLS = true
	}
	h.save()

	if *useTLS {
		cert, fp, err := loadOrCreateCert(*dir)
		if err != nil {
			log.Fatal("tls: ", err)
		}
		h.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
		h.fp = fp
	}
	h.loadMessages()
	h.indexFiles()
	h.loadAudit()
	h.sweep()

	ln, err := h.listen(*port)
	if err != nil {
		log.Fatal(err)
	}
	go h.serveHTTP(*port + 1)
	go h.serveDiscovery(*port + 2)
	go h.flusher()
	go h.janitor()

	h.banner(first, ownerPass, *public)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		h.mu.Lock()
		h.save()
		if h.logf != nil {
			h.logf.Close()
		}
		fmt.Println("\nhub stopped (state saved)")
		os.Exit(0)
	}()

	h.acceptLoop(ln)
}

func (h *hub) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handleConn(c)
	}
}

func (h *hub) banner(first bool, ownerPass string, public bool) {
	S := h.st.Settings
	scheme := ""
	if h.tlsCfg != nil {
		scheme = "tls://"
	}
	nmsg := 0
	for _, ms := range h.msgs {
		nmsg += len(ms)
	}
	fmt.Printf("%s hub %s\n  chat :%d   files :%d   discovery udp :%d   tls: %v\n  dir: %s\n",
		appName, version, h.tcpPort, h.tcpPort+1, h.tcpPort+2, h.tlsCfg != nil, h.dir)
	fmt.Printf("  users: %d   channels: %d   messages: %d   files: %d (%s)\n",
		len(h.st.Users), len(h.st.Channels), nmsg, len(h.st.Files), humanSize(h.totalBytes))
	if h.fp != "" {
		fmt.Printf("  fingerprint: %s\n", h.fp)
	}
	if first {
		fmt.Printf("\n  ★ owner account created —  username: owner   password: %s\n", ownerPass)
		fmt.Printf("    (shown once; change it with /passwd inside the app; recover with `%s serve -reset-owner`)\n", binName)
	}
	for _, m := range h.migrated {
		fmt.Printf("  ↳ %s\n", m)
	}
	if S.GuestAccess {
		fmt.Printf("  guest access: ON — token %s  (v1 clients and `-token` users land in #%s; turn off with /admin set guest_access off)\n", S.LegacyToken, S.DefaultChannel)
	} else {
		fmt.Printf("  guest access: off (accounts only)\n")
	}
	fmt.Println("  connect with:")
	for _, ip := range localIPs() {
		fmt.Printf("    %s -hub %s%s:%d -user owner\n", binName, scheme, ip, h.tcpPort)
	}
	if public {
		fmt.Printf("    (public) %s -hub tls://<YOUR-PUBLIC-IP>:%d -user NAME -fingerprint %s\n", binName, h.tcpPort, h.fp)
		fmt.Printf("  firewall: allow TCP %d and %d inbound\n", h.tcpPort, h.tcpPort+1)
	}
	L := S.Limits
	fmt.Printf("  limits: msg %dKB · file %dMB · quota %dMB/user · %.0f msg/s · %.0f uploads/min · %.0f conns/min/IP · registration: %s\n\n",
		L.MaxMsgKB, L.MaxFileMB, L.QuotaMB, L.MsgRate, L.UploadRate, L.ConnRate, S.Registration)
}

func (h *hub) listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil || h.tlsCfg == nil {
		return ln, err
	}
	return tls.NewListener(ln, h.tlsCfg), nil
}

func remoteIP(c net.Conn) string {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}

// connAllow charges n connection tokens for an IP against the live conn_rate setting.
func (h *hub) connAllow(ip string, n float64) bool {
	h.mu.Lock()
	rate := h.lim().ConnRate
	h.mu.Unlock()
	return h.connLim.allowRate(ip, rate/60, 10, n)
}

// ---- session plumbing -------------------------------------------------------

// push queues a frame for the session without blocking. A client that can't keep up
// (queue full) is dropped rather than allowed to make the hub buffer without bound.
// Call while holding h.mu, or from the session's own reader goroutine.
func (s *session) push(m Msg) {
	if s.closed {
		return
	}
	b, _ := json.Marshal(m)
	b = append(b, '\n')
	select {
	case s.out <- b:
	default:
		s.conn.Close()
	}
}

func (s *session) writer() {
	for b := range s.out {
		if _, err := s.conn.Write(b); err != nil {
			s.conn.Close() // keep draining so pushes never block
		}
	}
}

// toChan fans a frame out to every session subscribed to the channel. Caller holds h.mu.
func (h *hub) toChan(cid int64, m Msg) {
	for s := range h.sessions {
		if s.subs[cid] {
			s.push(m)
		}
	}
}

// toUser sends to every session of a user (all their devices), optionally skipping one.
func (h *hub) toUser(uid int64, m Msg, except *session) {
	for s := range h.sessions {
		if s.user.ID == uid && s != except {
			s.push(m)
		}
	}
}

// kickSessions sends a final frame to every session of a user and closes them.
func (h *hub) kickSessions(uid int64, m Msg) {
	for s := range h.sessions {
		if s.user.ID == uid {
			s.push(m)
			go func(c net.Conn) { time.Sleep(300 * time.Millisecond); c.Close() }(s.conn)
		}
	}
}

func (h *hub) onlineIn(cid int64) []string {
	var out []string
	for uid := range h.presence[cid] {
		if u := h.st.Users[uid]; u != nil {
			out = append(out, u.Name)
		} else { // guest: find a live session for the nick
			for s := range h.sessions {
				if s.user.ID == uid {
					out = append(out, s.user.Name)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func (h *hub) sessionCount(uid int64) int {
	n := 0
	for s := range h.sessions {
		if s.user.ID == uid {
			n++
		}
	}
	return n
}

// chanFor resolves a channel name for a session. Empty name → the default channel.
// Returns nil when the channel doesn't exist OR the user can't see it — callers must
// answer both with the identical `nochan` error.
func (h *hub) chanFor(s *session, name string) *Channel {
	var ch *Channel
	if name == "" {
		ch = h.defaultChan()
	} else {
		ch = h.chanByName(name)
	}
	if ch == nil || h.rankIn(s.user, ch) == rankNone {
		return nil
	}
	return ch
}

func errNoChan(rid, ch string) Msg {
	return Msg{T: "err", Code: codeNoChan, Text: textNoChan, RID: rid, Ch: ch}
}

// ---- connection lifecycle ---------------------------------------------------

func (h *hub) handleConn(conn net.Conn) {
	defer conn.Close()
	ip := remoteIP(conn)
	if !h.connAllow(ip, 1) {
		return // silently drop connection floods
	}
	tuneTCP(conn)
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if !sc.Scan() {
		return
	}
	var hello Msg
	if json.Unmarshal(sc.Bytes(), &hello) != nil || hello.T != "hello" {
		writeJSON(conn, Msg{T: "err", Code: codeBadArg, Text: "expected a hello frame"})
		return
	}
	s, rawSession, errm := h.authenticate(conn, ip, hello)
	if errm != nil {
		writeJSON(conn, *errm)
		return
	}
	conn.SetReadDeadline(time.Time{})
	go s.writer()

	h.mu.Lock()
	h.sessions[s] = struct{}{}
	label := appName + " hub " + version
	if s.v < 2 {
		// v1 client: it never sends `sub`, so put it in the default channel now, replay
		// what it missed (it told us `since`), then answer in the v1 `ok` shape.
		if def := h.defaultChan(); def != nil {
			h.subscribe(s, def, hello.Since)
			s.push(Msg{T: "ok", Name: s.user.Name, Users: h.onlineIn(def.ID), Text: label})
		}
	} else {
		s.push(Msg{T: "ok", V: protoVersion, User: s.user.Name, Role: s.user.Role, Session: rawSession,
			Channels: h.channelsFor(s.user), Limits: h.limitsFor(s.user), Text: label})
	}
	h.mu.Unlock()

	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.T {
		case "ping":
			s.push(Msg{T: "pong"})
		case "msg":
			h.handleMsg(s, m)
		case "clip":
			h.handleClip(s, m)
		case "sub":
			h.handleSub(s, m)
		case "unsub":
			h.mu.Lock()
			if ch := h.chanByName(m.Ch); ch != nil {
				h.unsubscribe(s, ch.ID)
			}
			h.mu.Unlock()
		case "read":
			h.handleRead(s, m)
		case "keyreq":
			h.handleKeyReq(s, m)
		case "keyshare":
			h.handleKeyShare(s, m)
		case "who":
			h.mu.Lock()
			if ch := h.chanFor(s, m.Ch); ch != nil {
				s.push(Msg{T: "who", Ch: ch.Name, Users: h.onlineIn(ch.ID)})
			} else {
				s.push(errNoChan(m.RID, m.Ch))
			}
			h.mu.Unlock()
		case "cmd":
			s.push(h.handleCmd(s, m))
		}
	}

	h.mu.Lock()
	for cid := range s.subs {
		h.unsubscribe(s, cid)
	}
	delete(h.sessions, s)
	s.closed = true
	close(s.out)
	h.mu.Unlock()
}

var dummyHash string // used to equalise timing for unknown usernames

// authenticate turns a hello into a session. Password hashing (~0.3 s) happens outside
// the hub lock so a login never stalls everyone else. Returns the raw session token when
// a new session was created (password or invite login).
func (h *hub) authenticate(conn net.Conn, ip string, hello Msg) (*session, string, *Msg) {
	fail := func(code, text string) (*session, string, *Msg) {
		return nil, "", &Msg{T: "err", Code: code, Text: text}
	}
	s := &session{h: h, conn: conn, out: make(chan []byte, 8192), ip: ip, v: hello.V, device: strings.TrimSpace(hello.Device), subs: map[int64]bool{}}
	if s.device == "" {
		s.device = strings.TrimSpace(hello.Name)
	}
	if s.device == "" {
		s.device = "device"
	}
	a := hello.Auth
	now := nowMs()

	switch {
	case a != nil && a.Session != "":
		h.mu.Lock()
		defer h.mu.Unlock()
		sh := sessHash(a.Session)
		se := h.st.Sessions[sh]
		if se == nil {
			return fail(codeAuth, "session expired or revoked — log in again with -user NAME")
		}
		u := h.st.Users[se.UserID]
		if u == nil {
			delete(h.st.Sessions, sh)
			return fail(codeAuth, "account no longer exists")
		}
		if msg := h.userBlocked(u); msg != "" {
			return fail(codeBanned, msg)
		}
		se.LastSeen, se.IP, u.LastSeen = now, ip, now
		h.markDirty()
		s.user, s.sessHash = u, sh
		return h.finishAuth(s, hello, "")

	case a != nil && a.User != "" && (a.Invite != "" || a.Register):
		name := strings.ToLower(strings.TrimSpace(a.User))
		if !validName(name, 2, 24, true) {
			return fail(codeBadArg, "username: 2-24 chars, a-z 0-9 _ . -")
		}
		if len(a.Pass) < 8 {
			return fail(codeBadArg, "password: at least 8 characters")
		}
		h.mu.Lock()
		L := *h.lim()
		key := "signup:" + ip
		if h.loginLim.exhausted(key) {
			h.mu.Unlock()
			return fail(codeRateLimit, "too many attempts — wait a minute")
		}
		h.loginLim.allowRate(key, L.LoginRate/60, L.LoginRate, 1)
		h.mu.Unlock()
		ph := hashPassword(a.Pass) // slow; outside the lock
		h.mu.Lock()
		defer h.mu.Unlock()
		S := &h.st.Settings
		if h.userByName(name) != nil {
			return fail(codeExists, "that username is taken")
		}
		if len(h.st.Users) >= S.MaxUsers {
			return fail(codeLimit, "this hub is full")
		}
		var ch *Channel
		var inv *Invite
		if a.Invite != "" {
			inv = h.validInvite(a.Invite)
			if inv == nil || !inv.Signup {
				return fail(codeAuth, "invalid or expired invite")
			}
			ch = h.st.Channels[inv.ChannelID]
			if ch == nil {
				return fail(codeAuth, "invalid or expired invite")
			}
		} else {
			if S.Registration != "open" {
				return fail(codeAuth, "registration is by invite only — ask a member for an invite code")
			}
			ch = h.defaultChan()
		}
		u := h.newUserHashed(name, ph, "user", 0)
		if inv != nil {
			u.CreatedBy = inv.CreatedBy
			ch.Members[u.ID] = &Membership{Role: inv.Role, JoinedAt: now, InvitedBy: inv.CreatedBy}
			h.consumeInvite(inv)
			h.audit(u, "signup", u.Name, ch, "invite from "+h.nameOf(inv.CreatedBy))
		} else if ch != nil {
			ch.Members[u.ID] = &Membership{Role: "member", JoinedAt: now}
			h.audit(u, "signup", u.Name, ch, "open registration")
		}
		s.user = u
		raw := h.newSession(u, s.device, ip)
		return h.finishAuth(s, hello, raw)

	case a != nil && a.User != "":
		name := strings.ToLower(strings.TrimSpace(a.User))
		h.mu.Lock()
		L := *h.lim()
		key := "login:" + name
		if h.loginLim.exhausted(key) {
			h.mu.Unlock()
			return fail(codeRateLimit, "too many login attempts — wait a minute")
		}
		u := h.userByName(name)
		stored := dummyHash
		if u != nil {
			stored = u.Pass
		}
		h.mu.Unlock()
		ok := checkPassword(stored, a.Pass) && u != nil // constant work for unknown names too
		h.mu.Lock()
		defer h.mu.Unlock()
		if !ok {
			h.loginLim.allowRate(key, L.LoginRate/60, L.LoginRate, 1)
			h.audit(nil, "login_fail", name, nil, "from "+ip)
			return fail(codeAuth, "bad username or password")
		}
		if u = h.userByName(name); u == nil {
			return fail(codeAuth, "bad username or password")
		}
		if msg := h.userBlocked(u); msg != "" {
			return fail(codeBanned, msg)
		}
		s.user = u
		raw := h.newSession(u, s.device, ip)
		return h.finishAuth(s, hello, raw)

	case hello.Token != "":
		h.mu.Lock()
		defer h.mu.Unlock()
		S := h.st.Settings
		if !S.GuestAccess || S.LegacyToken == "" || hello.Token != S.LegacyToken {
			if hello.V == 0 {
				return fail(codeAuth, "bad token")
			}
			return fail(codeAuth, "guest access is off or the token is wrong — ask for an account: "+binName+" -hub HOST -user NAME")
		}
		nick := strings.ToLower(sanitizeName(strings.TrimSpace(hello.Name)))
		if nick == "" || nick == "file" {
			nick = "guest"
		}
		if h.userByName(nick) != nil {
			nick += "~" // don't impersonate an account
		}
		s.user = &User{ID: guestID(nick), Name: nick, Role: "guest", Status: "active"}
		s.guest = true
		return h.finishAuth(s, hello, "")
	}
	return fail(codeAuth, "no credentials — use -user NAME (account) or -token X (guest)")
}

// finishAuth applies per-user connection limits and registers this device's E2E
// public key (if it sent one), so other devices can find it to share a channel key.
// Caller holds h.mu.
func (h *hub) finishAuth(s *session, hello Msg, raw string) (*session, string, *Msg) {
	if max := h.lim().ConnsPerUser; max > 0 && h.sessionCount(s.user.ID) >= max {
		return nil, "", &Msg{T: "err", Code: codeLimit, Text: fmt.Sprintf("too many simultaneous connections (max %d) — /sessions on another device", max)}
	}
	if hello.Pub != "" {
		id := deviceKeyID(s.user.ID, s.device)
		if dk := h.st.DeviceKeys[id]; dk == nil || dk.Pub != hello.Pub {
			h.st.DeviceKeys[id] = &DeviceKey{Pub: hello.Pub, Updated: nowMs()}
			h.markDirty()
		}
	}
	return s, raw, nil
}

func (h *hub) userBlocked(u *User) string {
	switch u.Status {
	case "banned":
		if u.BanUntil > 0 && u.BanUntil < nowMs() {
			u.Status, u.BanUntil, u.BanReason = "active", 0, ""
			h.markDirty()
			return ""
		}
		s := "you are banned from this hub"
		if u.BanReason != "" {
			s += ": " + u.BanReason
		}
		if u.BanUntil > 0 {
			s += " (until " + time.UnixMilli(u.BanUntil).Format("2006-01-02") + ")"
		}
		return s
	case "disabled":
		return "this account is disabled"
	}
	return ""
}

func (h *hub) newUserHashed(name, passHash, role string, by int64) *User {
	u := &User{ID: h.st.NextUser, Name: strings.ToLower(name), Pass: passHash, Role: role,
		Status: "active", CreatedAt: nowMs(), CreatedBy: by}
	h.st.NextUser++
	h.st.Users[u.ID] = u
	h.byName[u.Name] = u
	return u
}

// newSession creates a session for u, evicting the oldest beyond sessions_max. Caller holds h.mu.
func (h *hub) newSession(u *User, device, ip string) string {
	raw, hash := newSessionToken()
	now := nowMs()
	h.st.Sessions[hash] = &Session{UserID: u.ID, Device: device, IP: ip, CreatedAt: now, LastSeen: now}
	var mine []string
	for k, se := range h.st.Sessions {
		if se.UserID == u.ID {
			mine = append(mine, k)
		}
	}
	if max := h.lim().SessionsMax; max > 0 && len(mine) > max {
		sort.Slice(mine, func(i, j int) bool { return h.st.Sessions[mine[i]].CreatedAt < h.st.Sessions[mine[j]].CreatedAt })
		for _, k := range mine[:len(mine)-max] {
			delete(h.st.Sessions, k)
		}
	}
	u.LastSeen = now
	h.save()
	return raw
}

func (h *hub) validInvite(code string) *Invite {
	code = strings.TrimSpace(code)
	inv := h.st.Invites[code]
	if inv == nil {
		inv = h.st.Invites[strings.ReplaceAll(code, "-", "")]
	}
	if inv == nil {
		return nil
	}
	if (inv.ExpiresAt > 0 && inv.ExpiresAt < nowMs()) || inv.UsesLeft == 0 {
		delete(h.st.Invites, inv.Code)
		h.markDirty()
		return nil
	}
	return inv
}

func (h *hub) consumeInvite(inv *Invite) {
	if inv.UsesLeft > 0 {
		inv.UsesLeft--
		if inv.UsesLeft == 0 {
			delete(h.st.Invites, inv.Code)
		}
	}
	h.markDirty()
}

// ---- channels: info, subscribe, presence ------------------------------------

func (h *hub) chanInfoFor(u *User, ch *Channel) ChanInfo {
	ci := ChanInfo{Name: ch.Name, Topic: ch.Topic, Readonly: ch.Readonly, Members: len(ch.Members),
		Default: ch.Default, Last: h.lastID(ch.ID), Expire: ch.ExpireSec, E2E: ch.E2E, Epoch: ch.KeyEpoch}
	if m := ch.Members[u.ID]; m != nil {
		ci.Role = m.Role
		ci.Unread = h.unreadCount(ch.ID, m.LastRead)
	} else if srvRank(u) == srvGuest {
		ci.Role = "guest"
	}
	return ci
}

// channelsFor lists the channels a user may see: their memberships, or everything for
// server admins. Sorted by creation. Never includes channels the user can't see.
func (h *hub) channelsFor(u *User) []ChanInfo {
	var out []ChanInfo
	for _, ch := range h.st.Channels {
		if h.rankIn(u, ch) == rankNone {
			continue
		}
		out = append(out, h.chanInfoFor(u, ch))
	}
	sort.Slice(out, func(i, j int) bool { return h.chanByName(out[i].Name).ID < h.chanByName(out[j].Name).ID })
	return out
}

// subscribe attaches a session to a channel, replays messages after `since` (capped),
// and announces presence if this is the user's first session in the channel. Caller holds h.mu.
func (h *hub) subscribe(s *session, ch *Channel, since int64) {
	s.subs[ch.ID] = true
	ms := h.msgs[ch.ID]
	i := sort.Search(len(ms), func(i int) bool { return ms[i].ID > since })
	if len(ms)-i > replayCap {
		i = len(ms) - replayCap
	}
	cut := int64(0)
	if ch.ExpireSec > 0 {
		cut = nowMs() - ch.ExpireSec*1000
	}
	for ; i < len(ms); i++ {
		m := ms[i]
		if m.TS < cut {
			continue // past its self-destruct time, the sweep just hasn't run yet
		}
		m.Hist, m.CID = true, 0
		s.push(m)
	}
	s.push(Msg{T: "synced", Ch: ch.Name, Last: h.lastID(ch.ID)})
	p := h.presence[ch.ID]
	if p == nil {
		p = map[int64]int{}
		h.presence[ch.ID] = p
	}
	p[s.user.ID]++
	if p[s.user.ID] == 1 {
		h.toChan(ch.ID, Msg{T: "join", Ch: ch.Name, From: s.user.Name})
	}
}

func (h *hub) unsubscribe(s *session, cid int64) { h.unsub(s, cid, false) }

// unsub detaches a session; quiet suppresses the `part` event (used when a kick/ban
// event is about to explain the departure instead).
func (h *hub) unsub(s *session, cid int64, quiet bool) {
	if !s.subs[cid] {
		return
	}
	delete(s.subs, cid)
	p := h.presence[cid]
	if p == nil {
		return
	}
	p[s.user.ID]--
	if p[s.user.ID] <= 0 {
		delete(p, s.user.ID)
		if ch := h.st.Channels[cid]; ch != nil && !quiet {
			h.toChan(cid, Msg{T: "part", Ch: ch.Name, From: s.user.Name})
		}
	}
}

// unsubUser detaches every session of a user from a channel and tells them why.
func (h *hub) unsubUser(uid, cid int64, why Msg, quiet bool) {
	for s := range h.sessions {
		if s.user.ID == uid {
			h.unsub(s, cid, quiet)
			s.push(why)
		}
	}
}

// post assigns id/ts, persists, and fans out a message or file announcement. Caller holds h.mu.
func (h *hub) post(ch *Channel, m Msg) Msg {
	h.nextID++
	m.ID, m.TS, m.Ch, m.CID = h.nextID, nowMs(), ch.Name, ch.ID
	ms := append(h.msgs[ch.ID], m)
	if len(ms) > h.keep+h.keep/4 {
		ms = append([]Msg(nil), ms[len(ms)-h.keep:]...)
	}
	h.msgs[ch.ID] = ms
	h.appendLog(m)
	out := m
	out.CID = 0
	h.toChan(ch.ID, out)
	return m
}

// ---- frame handlers ---------------------------------------------------------

func (h *hub) handleSub(s *session, m Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.guest && m.Ch != "" && !strings.EqualFold(m.Ch, h.st.Settings.DefaultChannel) {
		s.push(errNoChan(m.RID, m.Ch))
		return
	}
	ch := h.chanFor(s, m.Ch)
	if ch == nil {
		s.push(errNoChan(m.RID, m.Ch))
		return
	}
	if s.subs[ch.ID] { // re-sub = just replay the gap
		delete(s.subs, ch.ID)
		h.presence[ch.ID][s.user.ID]--
	}
	h.subscribe(s, ch, m.Since)
}

func (h *hub) handleMsg(s *session, m Msg) {
	if strings.TrimSpace(m.Text) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.chanFor(s, m.Ch)
	if ch == nil {
		s.push(errNoChan(m.RID, m.Ch))
		return
	}
	if len(m.Text) > h.maxMsgBytes() {
		s.push(Msg{T: "err", Code: codeTooLong, RID: m.RID, Ch: ch.Name, Text: fmt.Sprintf("message too long (max %d KB)", h.lim().MaxMsgKB)})
		return
	}
	rank := h.rankIn(s.user, ch)
	if !canSend(rank, ch) {
		code, text := codePerm, "you can't post in #"+ch.Name
		if ch.Readonly || rank == rankReadonly {
			code, text = codeReadonly, "#"+ch.Name+" is read-only for you"
		}
		s.push(Msg{T: "err", Code: code, RID: m.RID, Ch: ch.Name, Text: text})
		return
	}
	if mem := ch.Members[s.user.ID]; mem != nil && mem.MutedUntil > nowMs() {
		s.push(Msg{T: "err", Code: codeMuted, RID: m.RID, Ch: ch.Name, Until: mem.MutedUntil,
			Text: "you are muted in #" + ch.Name + " for another " + humanDur(time.Until(time.UnixMilli(mem.MutedUntil)))})
		return
	}
	rate, burst := h.msgRateFor(s.user, ch, rank)
	if !h.msgLim.allowRate(fmt.Sprintf("%d/%d", s.user.ID, ch.ID), rate, burst, 1) {
		s.push(Msg{T: "err", Code: codeRateLimit, RID: m.RID, Ch: ch.Name, Text: "slow down — rate limited"})
		h.noteRateHit(s.user, ch)
		return
	}
	h.post(ch, Msg{T: "msg", From: s.user.Name, Text: m.Text, Epoch: m.Epoch})
}

// handleKeyReq relays "I don't have this channel's key" to every other device currently
// subscribed to the channel, so any one of them holding it can wrap and send it back via
// a keyshare frame. The hub never sees a channel key itself, only requests and wrapped blobs.
func (h *hub) handleKeyReq(s *session, m Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.chanFor(s, m.Ch)
	if ch == nil || !ch.E2E {
		return
	}
	L := h.lim()
	if !h.msgLim.allowRate(fmt.Sprintf("keyreq/%d", s.user.ID), L.MsgRate, L.MsgBurst, 1) {
		return
	}
	pub := ""
	if dk := h.st.DeviceKeys[deviceKeyID(s.user.ID, s.device)]; dk != nil {
		pub = dk.Pub
	}
	req := Msg{T: "keyreq", Ch: ch.Name, Epoch: m.Epoch, FromUser: s.user.Name, FromDev: s.device, FromPub: pub}
	for other := range h.sessions {
		if other != s && other.subs[ch.ID] {
			other.push(req)
		}
	}
}

// handleKeyShare relays a wrapped channel key from one device to one specific other
// device of a member — never stored, never broadcast wider than that one target.
func (h *hub) handleKeyShare(s *session, m Msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.chanFor(s, m.Ch)
	if ch == nil || !ch.E2E {
		return
	}
	target := h.userByName(m.ToUser)
	if target == nil || h.rankIn(target, ch) == rankNone {
		return // don't hand key material to someone who isn't (or is no longer) a member
	}
	out := Msg{T: "keyshare", Ch: ch.Name, Epoch: m.Epoch, ToUser: m.ToUser, ToDevice: m.ToDevice, FromPub: m.FromPub, Wrapped: m.Wrapped}
	for other := range h.sessions {
		if other.user.ID == target.ID && other.device == m.ToDevice {
			other.push(out)
		}
	}
}

// handleClip relays a clipboard payload to the user's *other* devices. Never stored,
// never replayed, never sent to anyone else — a clipboard often holds passwords.
func (h *hub) handleClip(s *session, m Msg) {
	if m.Text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(m.Text) > h.maxMsgBytes() {
		s.push(Msg{T: "err", Code: codeTooLong, RID: m.RID, Text: fmt.Sprintf("clipboard too large to sync (max %d KB)", h.lim().MaxMsgKB)})
		return
	}
	L := h.lim()
	if !h.msgLim.allowRate(fmt.Sprintf("clip/%d", s.user.ID), L.MsgRate, L.MsgBurst, 1) {
		s.push(Msg{T: "err", Code: codeRateLimit, RID: m.RID, Text: "clipboard sync rate limited"})
		return
	}
	h.toUser(s.user.ID, Msg{T: "clip", From: s.device, Text: m.Text}, s)
}

// handleRead records how far a user has read a channel and tells their other devices.
func (h *hub) handleRead(s *session, m Msg) {
	if s.guest || m.ID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.chanFor(s, m.Ch)
	if ch == nil {
		return
	}
	mem := ch.Members[s.user.ID]
	if mem == nil || m.ID <= mem.LastRead {
		return
	}
	mem.LastRead = m.ID
	h.markDirty()
	h.toUser(s.user.ID, Msg{T: "read", Ch: ch.Name, ID: m.ID}, s)
}

// ---- files: reference counting ----------------------------------------------

func (h *hub) refFile(fid string, cid int64) {
	m := h.fileRefs[fid]
	if m == nil {
		m = map[int64]int{}
		h.fileRefs[fid] = m
	}
	m[cid]++
}

// unrefFile drops one reference; when nothing references the file any more it is
// deleted from disk and the uploader's quota is refunded.
func (h *hub) unrefFile(fid string, cid int64) {
	m := h.fileRefs[fid]
	if m != nil {
		m[cid]--
		if m[cid] <= 0 {
			delete(m, cid)
		}
		if len(m) > 0 {
			return
		}
	}
	delete(h.fileRefs, fid)
	h.deleteFile(fid)
}

func (h *hub) deleteFile(fid string) {
	if meta := h.st.Files[fid]; meta != nil {
		h.used[meta.Owner] -= meta.Size
		if h.used[meta.Owner] < 0 {
			h.used[meta.Owner] = 0
		}
		h.totalBytes -= meta.Size
		delete(h.hashes, meta.Sha)
		delete(h.st.Files, fid)
		h.markDirty()
	}
	matches, _ := filepath.Glob(filepath.Join(h.dir, "files", fid+"_*"))
	for _, p := range matches {
		os.Remove(p)
	}
	os.Remove(filepath.Join(h.dir, "files", fid+".sha256"))
}

func (h *hub) fileRefCount(fid string) int {
	n := 0
	for _, c := range h.fileRefs[fid] {
		n += c
	}
	return n
}

// indexFiles reconciles disk, state.json and the message log: sidecar hashes → dedup
// map, unknown files → metadata (v1 uploads), references → from messages in memory.
func (h *hub) indexFiles() {
	ents, _ := os.ReadDir(filepath.Join(h.dir, "files"))
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".sha256") {
			fid := strings.TrimSuffix(n, ".sha256")
			if b, err := os.ReadFile(filepath.Join(h.dir, "files", n)); err == nil {
				h.hashes[strings.TrimSpace(string(b))] = fid
				if meta := h.st.Files[fid]; meta != nil && meta.Sha == "" {
					meta.Sha = strings.TrimSpace(string(b))
				}
			}
			continue
		}
		fid, name, ok := strings.Cut(n, "_")
		if !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if h.st.Files[fid] == nil { // pre-v2 upload: adopt it, owned by nobody
			h.st.Files[fid] = &FileMeta{FID: fid, Name: name, Size: fi.Size(), CreatedAt: fi.ModTime().UnixMilli()}
			h.markDirty()
		}
	}
	for fid, meta := range h.st.Files {
		if meta.Sha != "" {
			h.hashes[meta.Sha] = fid
		}
		h.used[meta.Owner] += meta.Size
		h.totalBytes += meta.Size
	}
	for cid, ms := range h.msgs {
		for _, m := range ms {
			if m.T == "file" && m.FID != "" {
				h.refFile(m.FID, cid)
			}
		}
	}
}
