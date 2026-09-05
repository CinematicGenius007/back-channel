package main

// The client: one persistent connection, many channels, a cooked-mode TUI. All channel
// state is touched only by the main loop goroutine (events + input); network goroutines
// talk to it through the `events` channel. Config is guarded by cfgMu because the
// connect loop reads it while the main loop may be updating a login.

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxLines = 500 // scrollback kept per channel

// line is one rendered scrollback entry. id/ts let self-destruct channels remove it later.
type line struct {
	id, ts int64
	text   string
}

type chanState struct {
	name, topic, role string
	lastID, lastRead  int64
	unread            int
	readonly          bool
	expire            int64 // seconds; >0 = self-destruct channel
	texts             map[int64]string
	files             map[int64]Msg
	saved             map[int64]string // message id → path we downloaded to (for cleanup on expiry)
	lines             []line
}

func (ch *chanState) rendered() []string {
	out := make([]string, len(ch.lines))
	for i, l := range ch.lines {
		out[i] = l.text
	}
	return out
}

// forget drops messages by id from the channel's maps and scrollback; returns how many
// lines went away (so the caller can redraw). In self-destruct channels it also removes
// files this client downloaded automatically for those messages.
func (ch *chanState) forget(ids []int64) int {
	gone := map[int64]bool{}
	for _, id := range ids {
		gone[id] = true
		delete(ch.texts, id)
		delete(ch.files, id)
		if p, ok := ch.saved[id]; ok {
			if ch.expire > 0 {
				os.Remove(p)
			}
			delete(ch.saved, id)
		}
	}
	n := 0
	kept := ch.lines[:0]
	for _, l := range ch.lines {
		if l.id != 0 && gone[l.id] {
			n++
			continue
		}
		kept = append(kept, l)
	}
	ch.lines = kept
	return n
}

// expireLocal removes lines older than the channel's expiry (runs on the 2 s tick, so
// messages vanish on time even when the hub is unreachable).
func (ch *chanState) expireLocal() int {
	if ch.expire <= 0 {
		return 0
	}
	cut := nowMs() - ch.expire*1000
	var ids []int64
	for _, l := range ch.lines {
		if l.ts > 0 && l.ts < cut {
			ids = append(ids, l.id)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	n := 0
	kept := ch.lines[:0]
	for _, l := range ch.lines {
		if l.ts > 0 && l.ts < cut {
			n++
			if l.id != 0 {
				delete(ch.texts, l.id)
				delete(ch.files, l.id)
				if p, ok := ch.saved[l.id]; ok {
					os.Remove(p)
					delete(ch.saved, l.id)
				}
			}
			continue
		}
		kept = append(kept, l)
	}
	ch.lines = kept
	return n
}

type client struct {
	cfgMu    sync.Mutex
	cfg      Config
	dir      string
	tcpAddr  string
	httpAddr string
	tlsCfg   *tls.Config

	mu          sync.Mutex // guards conn
	conn        net.Conn
	online      atomic.Bool
	legacy      bool // talking to a v1 hub: one channel, frames carry no `ch`
	legacySince atomic.Int64
	events      chan Msg
	ui          *tui

	chans     []*chanState
	active    *chanState
	me, role  string
	maxMsg    atomic.Int64
	quotaLeft int64

	wake      chan struct{}
	authWait  atomic.Bool
	pending   *prompt
	readDirty map[string]int64
	rid       int
	waiting   map[string]func(Msg)

	clipMu           sync.Mutex
	clipLast, clipIn string
	clipOn           atomic.Bool
}

type clientFlags struct {
	user, invite, ch string
}

// parseClientFlags fills cfg from saved config, then flags, then defaults.
func parseClientFlags(name string, args []string) (Config, string, clientFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "data dir (config, inbox, outbox)")
	hub := fs.String("hub", "", "hub host[:port] or tls://host[:port]  (default: saved config, else LAN discovery)")
	device := fs.String("name", "", "label for this device, e.g. mac, win-work (default: saved, else hostname)")
	user := fs.String("user", "", "account name — asks for the password once and saves a session")
	invite := fs.String("invite", "", "invite code — creates a new account (with -user NAME)")
	token := fs.String("token", envOr("BCH_TOKEN", ""), "legacy shared token: join as a guest")
	auto := fs.Int("auto", 0, "auto-download files up to N MB (default 50)")
	fp := fs.String("fingerprint", "", "pin the hub's certificate fingerprint (printed by `serve -tls`)")
	useTLS := fs.Bool("tls", false, "connect with TLS (same as a tls:// hub address)")
	ch := fs.String("ch", "", "channel to send to (send only; default: last active channel)")
	fs.Parse(args)

	cfg := loadConfig(*dir)
	if *hub != "" {
		cfg.Hub = *hub
	}
	if *device != "" {
		cfg.Name = *device
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *auto != 0 {
		cfg.AutoMB = *auto
	}
	if *fp != "" {
		cfg.FP = *fp
	}
	if *useTLS || strings.HasPrefix(cfg.Hub, "tls://") {
		cfg.TLS = true
	}
	if cfg.AutoMB == 0 {
		cfg.AutoMB = 50
	}
	if cfg.Name == "" {
		hn, _ := os.Hostname()
		cfg.Name = strings.ToLower(strings.Split(hn, ".")[0])
		if cfg.Name == "" {
			cfg.Name = envOr("USER", envOr("USERNAME", "me"))
		}
	}
	if cfg.Hub == "" {
		fmt.Fprint(os.Stderr, "no hub configured, looking on the LAN... ")
		found := discover(1500)
		if len(found) == 0 {
			fmt.Fprintf(os.Stderr, "none found.\nStart one with `%s serve` or pass -hub HOST[:PORT].\n", binName)
			os.Exit(1)
		}
		cfg.Hub = found[0]
		fmt.Fprintln(os.Stderr, "found", cfg.Hub)
	}
	saveConfig(*dir, cfg)
	return cfg, *dir, clientFlags{user: *user, invite: *invite, ch: *ch}
}

func newClient(cfg Config, dir string) *client {
	c := &client{cfg: cfg, dir: dir, events: make(chan Msg, 512), wake: make(chan struct{}, 1),
		readDirty: map[string]int64{}, waiting: map[string]func(Msg){}}
	c.maxMsg.Store(64 << 10)
	var useTLS bool
	c.tcpAddr, c.httpAddr, useTLS = splitHub(cfg.Hub, 7777)
	if useTLS || cfg.TLS {
		c.cfg.TLS = true
		c.tlsCfg = clientTLSConfig(&c.cfg.FP, func(fp string) {
			c.cfgMu.Lock()
			saveConfig(dir, c.cfg)
			c.cfgMu.Unlock()
			if c.ui == nil {
				fmt.Fprintln(os.Stderr, "pinned hub fingerprint", fp, "(trust on first use)")
			} else {
				c.status("pinned hub fingerprint " + fp + " (trust on first use)")
			}
		})
	}
	os.MkdirAll(filepath.Join(dir, "inbox"), 0o755)
	os.MkdirAll(filepath.Join(dir, "outbox", "sent"), 0o755)
	return c
}

func (c *client) snapshot() Config {
	c.cfgMu.Lock()
	defer c.cfgMu.Unlock()
	return c.cfg
}

func (c *client) update(f func(*Config)) {
	c.cfgMu.Lock()
	f(&c.cfg)
	saveConfig(c.dir, c.cfg)
	c.cfgMu.Unlock()
}

// ---- credentials ------------------------------------------------------------

// bootstrapAuth runs before the TUI: it makes sure we have a session (asking for a
// password on the plain terminal) or a guest token, and exits with a clear message otherwise.
func (c *client) bootstrapAuth(f clientFlags) {
	cfg := c.snapshot()
	login := func(a Auth) {
		ok, err := c.loginOnce(a)
		if err != nil {
			fmt.Fprintln(os.Stderr, "login failed:", err)
			os.Exit(1)
		}
		c.update(func(cf *Config) { cf.User, cf.Session, cf.Token = ok.User, ok.Session, "" })
		fmt.Fprintf(os.Stderr, "logged in as %s (session saved in %s)\n", ok.User, filepath.Join(c.dir, "config.json"))
	}
	switch {
	case f.invite != "":
		user := f.user
		if user == "" {
			user = promptLine("choose a username: ")
		}
		pw := promptSecret("choose a password (8+ characters): ")
		if pw != promptSecret("repeat it: ") {
			fmt.Fprintln(os.Stderr, "passwords don't match")
			os.Exit(1)
		}
		login(Auth{Invite: f.invite, User: user, Pass: pw})
	case f.user != "" && (cfg.Session == "" || !strings.EqualFold(cfg.User, f.user)):
		login(Auth{User: f.user, Pass: promptSecret(fmt.Sprintf("password for %s@%s: ", f.user, cfg.Hub))})
	case cfg.Session == "" && cfg.Token == "" && cfg.User != "":
		login(Auth{User: cfg.User, Pass: promptSecret(fmt.Sprintf("password for %s@%s: ", cfg.User, cfg.Hub))})
	case cfg.Session == "" && cfg.Token == "":
		fmt.Fprintf(os.Stderr, "no credentials. Use  -user NAME  (account)  or  -token X  (guest, if the hub allows it).\n")
		os.Exit(1)
	}
}

// loginOnce opens a throwaway connection, authenticates, and returns the `ok` frame
// (which carries the new session token). Used before the TUI and by /login.
func (c *client) loginOnce(a Auth) (Msg, error) {
	conn, err := c.dial(c.tcpAddr)
	if err != nil {
		return Msg{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	writeJSON(conn, Msg{T: "hello", V: protoVersion, Device: c.snapshot().Name, Auth: &a})
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.T {
		case "ok":
			return m, nil
		case "err":
			return m, errors.New(m.Text)
		}
	}
	return Msg{}, fmt.Errorf("no reply from hub — wrong port, or the hub uses TLS (try -hub tls://HOST)")
}

func promptLine(label string) string {
	fmt.Fprint(os.Stderr, label)
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	return strings.TrimSpace(sc.Text())
}

func promptSecret(label string) string {
	fmt.Fprint(os.Stderr, label)
	echoOff()
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	echoOn()
	fmt.Fprintln(os.Stderr)
	return strings.TrimRight(sc.Text(), "\r\n")
}

// ---- connection -------------------------------------------------------------

func (c *client) dial(addr string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	tuneTCP(conn)
	if c.tlsCfg == nil {
		return conn, nil
	}
	tc := tls.Client(conn, c.tlsCfg)
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	tc.SetDeadline(time.Time{})
	return tc, nil
}

func (c *client) connectLoop() {
	backoff := time.Second
	for {
		if c.authWait.Load() {
			select {
			case <-c.wake:
			case <-time.After(60 * time.Second):
			}
			c.authWait.Store(false)
		}
		cfg := c.snapshot()
		var hello Msg
		switch {
		case cfg.Session != "":
			hello = Msg{T: "hello", V: protoVersion, Device: cfg.Name, Auth: &Auth{Session: cfg.Session}}
		case cfg.Token != "":
			hello = Msg{T: "hello", V: protoVersion, Name: cfg.Name, Device: cfg.Name, Token: cfg.Token, Since: c.legacySince.Load()}
		default:
			c.status("no credentials — /login NAME")
			c.authWait.Store(true)
			continue
		}
		conn, err := c.dial(c.tcpAddr)
		if err != nil {
			var fe *errFingerprint
			if errors.As(err, &fe) {
				c.events <- Msg{T: "_sys", Text: fe.Error()}
				c.events <- Msg{T: "_status", Text: "REFUSING TO CONNECT: hub certificate changed"}
				time.Sleep(30 * time.Second)
				continue
			}
			c.events <- Msg{T: "_status", Text: fmt.Sprintf("offline — retrying %s (%v)", c.tcpAddr, shortErr(err))}
			time.Sleep(backoff)
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		c.write(hello)

		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var m Msg
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			if m.T == "ok" {
				backoff = time.Second
				c.online.Store(true)
			}
			if m.T == "err" && !c.online.Load() { // refused at hello
				hint := ""
				switch m.Code {
				case codeAuth, codeBanned, codeLimit:
					if m.Code == codeAuth && cfg.Session != "" {
						c.update(func(cf *Config) { cf.Session = "" })
					}
					if m.Code == codeAuth {
						hint = " — /login NAME to sign in"
					}
					c.authWait.Store(true)
				}
				c.events <- Msg{T: "_status", Text: "hub refused: " + m.Text}
				c.events <- Msg{T: "_sys", Text: "hub refused: " + m.Text + hint}
				conn.Close()
				break
			}
			c.events <- m
		}
		wasOnline := c.online.Load()
		c.online.Store(false)
		conn.Close()
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		if wasOnline {
			c.events <- Msg{T: "_status", Text: "disconnected — reconnecting"}
		} else if !c.authWait.Load() && c.tlsCfg == nil {
			c.events <- Msg{T: "_status", Text: "hub closed the connection before hello — if it runs with -tls/-public, use -hub tls://HOST"}
		}
		if !c.authWait.Load() {
			time.Sleep(backoff)
		}
	}
}

func (c *client) write(m Msg) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("not connected")
	}
	b, _ := json.Marshal(m)
	_, err := c.conn.Write(append(b, '\n'))
	return err
}

// reconnect drops the current connection; connectLoop dials again with the current config.
func (c *client) reconnect() {
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

// cmd sends a command frame; cb (optional) receives the `res`.
func (c *client) cmd(name string, args map[string]string, cb func(Msg)) {
	c.rid++
	rid := fmt.Sprintf("c%d", c.rid)
	ch := ""
	if c.active != nil && !c.legacy {
		ch = c.active.name
	}
	if cb != nil {
		c.waiting[rid] = cb
	}
	if err := c.write(Msg{T: "cmd", RID: rid, Name: name, Ch: ch, Args: args}); err != nil {
		delete(c.waiting, rid)
		c.sys("offline — command not sent")
	}
}

// ---- files ------------------------------------------------------------------

func (c *client) authHeaders() map[string]string {
	cfg := c.snapshot()
	if cfg.Session != "" {
		return map[string]string{"Authorization": "Bearer " + cfg.Session}
	}
	return map[string]string{"X-Token": cfg.Token}
}

func (c *client) upload(chName, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory (zip it first)", filepath.Base(path))
	}
	cfg := c.snapshot()
	target := "/up?name=" + escape(filepath.Base(path)) + "&from=" + escape(cfg.Name) + "&device=" + escape(cfg.Name)
	if chName != "" && !c.legacy {
		target += "&ch=" + escape(chName)
	}
	c.status(fmt.Sprintf("uploading %s (%s) to #%s…", filepath.Base(path), humanSize(st.Size()), chName))
	status, _, body, conn, err := httpDo(c.dial, c.httpAddr, "PUT", target, c.authHeaders(), f, st.Size())
	if err != nil {
		return err
	}
	defer conn.Close()
	if status != 200 {
		b, _ := io.ReadAll(body)
		return fmt.Errorf("hub: %d %s", status, strings.TrimSpace(string(b)))
	}
	c.status("")
	return nil
}

func (c *client) download(m Msg) (string, error) {
	status, _, body, conn, err := httpDo(c.dial, c.httpAddr, "GET", "/f/"+m.FID, c.authHeaders(), nil, 0)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if status != 200 {
		return "", fmt.Errorf("hub: status %d", status)
	}
	dir := filepath.Join(c.dir, "inbox")
	if m.Ch != "" && !c.legacy {
		dir = filepath.Join(dir, sanitizeName(m.Ch))
	}
	os.MkdirAll(dir, 0o755)
	dest := uniquePath(filepath.Join(dir, sanitizeName(m.Name)))
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	os.Rename(tmp, dest)
	return dest, nil
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		q := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(q); err != nil {
			return q
		}
	}
}

// watchOutbox uploads files dropped into <dir>/outbox (→ active channel) or
// <dir>/outbox/<channel>/ (→ that channel) once their size is stable across two polls.
func (c *client) watchOutbox() {
	outbox := filepath.Join(c.dir, "outbox")
	prev := map[string]int64{}
	for {
		time.Sleep(time.Second)
		if !c.online.Load() {
			continue
		}
		cur := map[string]int64{}
		scan := func(dir, ch string) {
			ents, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range ents {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".part") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				p := filepath.Join(dir, e.Name())
				cur[p] = info.Size()
				if sz, ok := prev[p]; ok && sz == info.Size() {
					target := ch
					if target == "" {
						target = c.activeName()
					}
					if err := c.upload(target, p); err != nil {
						c.status("outbox: " + err.Error())
						continue
					}
					os.Rename(p, uniquePath(filepath.Join(outbox, "sent", e.Name())))
					delete(cur, p)
				}
			}
		}
		scan(outbox, "")
		if ents, err := os.ReadDir(outbox); err == nil {
			for _, e := range ents {
				if e.IsDir() && e.Name() != "sent" && !strings.HasPrefix(e.Name(), ".") {
					scan(filepath.Join(outbox, e.Name()), e.Name())
				}
			}
		}
		prev = cur
	}
}

// activeName is safe to call from other goroutines (it only reads the saved config).
func (c *client) activeName() string { return c.snapshot().Active }

// ---- main loop --------------------------------------------------------------

func (c *client) status(s string) {
	select {
	case c.events <- Msg{T: "_status", Text: s}:
	default:
	}
}

// sys prints a dim system line into the active channel.
func (c *client) sys(s string) { c.addLine(c.active, dim("— "+s)) }

// sysIn prints a dim system line into a specific channel.
func (c *client) sysIn(ch *chanState, s string) { c.addLine(ch, dim("— "+s)) }

func runClient(args []string) {
	debug.SetGCPercent(50) // this process idles 99% of the time; trade CPU for heap
	cfg, dir, f := parseClientFlags(binName, args)
	c := newClient(cfg, dir)
	c.bootstrapAuth(f)
	c.clipOn.Store(c.snapshot().ClipSync)

	c.ui = newTUI()
	c.ui.init()
	defer c.ui.cleanup()
	defer echoOn()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	input := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			input <- sc.Text()
		}
		close(input)
	}()
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
	c.sys("type to chat · drag a file here + Enter to send · /1 /2 … or /c NAME to switch channels · /help for everything")
	c.ui.setStatus(fmt.Sprintf("connecting to %s…", c.tcpAddr))

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			return
		case <-winch:
			c.ui.checkResize()
		case m := <-c.events:
			c.handleEvent(m)
		case <-tick.C:
			c.flushReads()
		case line, ok := <-input:
			if !ok {
				return
			}
			c.ui.resetInput()
			if c.pending != nil {
				c.feedPrompt(line)
				continue
			}
			if !c.handleInput(line) {
				return
			}
		}
	}
}

// ---- channel state ----------------------------------------------------------

func (c *client) chanByName(name string) *chanState {
	for _, ch := range c.chans {
		if strings.EqualFold(ch.name, name) {
			return ch
		}
	}
	return nil
}

func (c *client) ensureChan(name string) *chanState {
	if ch := c.chanByName(name); ch != nil {
		return ch
	}
	ch := &chanState{name: name, texts: map[int64]string{}, files: map[int64]Msg{}, saved: map[int64]string{}}
	c.chans = append(c.chans, ch)
	return ch
}

// chanFor maps an incoming frame to a channel; frames without `ch` belong to the active
// channel (v1 hubs) or the default.
func (c *client) chanFor(name string) *chanState {
	if name == "" {
		if c.active != nil {
			return c.active
		}
		return c.ensureChan("main")
	}
	return c.ensureChan(name)
}

func (c *client) addLine(ch *chanState, text string) { c.addLineID(ch, 0, nowMs(), text) }

func (c *client) addLineID(ch *chanState, id, ts int64, text string) {
	if ch == nil {
		ch = c.active
	}
	if ch == nil {
		return
	}
	ch.lines = append(ch.lines, line{id: id, ts: ts, text: text})
	if len(ch.lines) > maxLines {
		ch.lines = ch.lines[len(ch.lines)-maxLines:]
	}
	if ch == c.active {
		c.ui.print(text)
	}
}

func (c *client) switchTo(ch *chanState) {
	if ch == nil {
		return
	}
	c.active = ch
	c.markRead(ch)
	c.ui.redraw(ch.rendered())
	c.drawBar()
	c.drawStatus()
	if !c.legacy {
		c.update(func(cf *Config) { cf.Active = ch.name })
	}
}

func (c *client) markRead(ch *chanState) {
	ch.unread = 0
	if ch.lastID > ch.lastRead {
		ch.lastRead = ch.lastID
		c.readDirty[ch.name] = ch.lastID
	}
}

func (c *client) flushReads() {
	for _, ch := range c.chans {
		if ch.expireLocal() > 0 && ch == c.active {
			c.ui.redraw(ch.rendered())
		}
	}
	if c.legacy || !c.online.Load() {
		return
	}
	for name, id := range c.readDirty {
		c.write(Msg{T: "read", Ch: name, ID: id})
		delete(c.readDirty, name)
	}
}

func (c *client) drawBar() {
	if c.legacy && len(c.chans) <= 1 {
		c.ui.setBar("")
		return
	}
	var b strings.Builder
	for i, ch := range c.chans {
		label := " #" + ch.name
		if i < 9 {
			label = fmt.Sprintf(" %d:#%s", i+1, ch.name)
		}
		if ch.unread > 0 {
			label += fmt.Sprintf("(%d)", ch.unread)
		}
		label += " "
		switch {
		case ch == c.active:
			b.WriteString("\x1b[7m\x1b[1m" + label + "\x1b[0m")
		case ch.unread > 0:
			b.WriteString("\x1b[1m" + label + "\x1b[0m")
		default:
			b.WriteString(dim(label))
		}
	}
	c.ui.setBar(b.String())
}

func (c *client) drawStatus() {
	cfg := c.snapshot()
	if !c.online.Load() {
		return
	}
	who := c.me
	if who == "" {
		who = cfg.Name
	}
	s := fmt.Sprintf("connected · %s@%s", who, cfg.Hub)
	if c.active != nil && !c.legacy {
		s += " · #" + c.active.name
		if c.active.role != "" {
			s += " (" + c.active.role + ")"
		}
		if c.active.expire > 0 {
			s += " · 🔥 " + fmtExpire(c.active.expire)
		}
		if c.active.topic != "" {
			s += " — " + c.active.topic
		}
	}
	if c.clipOn.Load() {
		s += " · 📋 sync"
	}
	c.ui.setStatus(s)
}

func (c *client) isMe(from string) bool {
	if c.me != "" {
		return strings.EqualFold(from, c.me)
	}
	return strings.EqualFold(from, c.snapshot().Name)
}

func (c *client) watched(name string) bool {
	for _, w := range c.snapshot().Watch {
		if strings.EqualFold(w, name) {
			return true
		}
	}
	return false
}

// ---- events -----------------------------------------------------------------

func (c *client) handleEvent(m Msg) {
	switch m.T {
	case "_status":
		if m.Text == "" {
			c.drawStatus()
		} else {
			c.ui.setStatus(m.Text)
		}
	case "_sys":
		if m.Ch != "" {
			c.sysIn(c.chanFor(m.Ch), m.Text)
		} else {
			c.sys(m.Text)
		}
	case "_saved": // a download finished
		ch := c.chanFor(m.Ch)
		ch.saved[m.ID] = m.Text
		note := "saved " + m.Text
		if ch.expire > 0 {
			note += "  (self-destruct channel: removed again when the message expires)"
		}
		c.sysIn(ch, note)
	case "ok":
		c.onOK(m)
	case "msg", "file":
		c.onMsg(m)
	case "synced":
		ch := c.chanFor(m.Ch)
		if m.Last > ch.lastID {
			ch.lastID = m.Last
		}
		if ch == c.active {
			c.markRead(ch)
			c.flushReads()
		}
		c.drawBar()
	case "deleted":
		ch := c.chanFor(m.Ch)
		if ch.forget([]int64{m.ID}) > 0 && ch == c.active {
			c.ui.redraw(ch.rendered())
		}
		if !m.Hist {
			c.sysIn(ch, fmt.Sprintf("#%d deleted by %s", m.ID, m.By))
		}
	case "purged":
		ch := c.chanFor(m.Ch)
		if ch.forget(m.IDs) > 0 && ch == c.active {
			c.ui.redraw(ch.rendered())
		}
		c.sysIn(ch, fmt.Sprintf("%d messages purged by %s", len(m.IDs), m.By))
	case "expired": // self-destruct: vanish silently
		ch := c.chanFor(m.Ch)
		if ch.forget(m.IDs) > 0 && ch == c.active {
			c.ui.redraw(ch.rendered())
		}
	case "settings":
		for _, ci := range m.Channels {
			ch := c.chanFor(ci.Name)
			ch.topic, ch.readonly, ch.expire = ci.Topic, ci.Readonly, ci.Expire
			what := "expire=" + fmtExpire(ci.Expire)
			if ci.Readonly {
				what += " read-only"
			}
			c.sysIn(ch, fmt.Sprintf("%s changed channel settings: %s", m.By, what))
			if ch.expireLocal() > 0 && ch == c.active {
				c.ui.redraw(ch.rendered())
			}
		}
		c.drawStatus()
	case "join":
		if !c.isMe(m.From) {
			c.sysIn(c.chanFor(m.Ch), m.From+" joined")
		}
	case "part":
		if !c.isMe(m.From) {
			c.sysIn(c.chanFor(m.Ch), m.From+" left")
		}
	case "kicked", "banned":
		ch := c.chanFor(m.Ch)
		what := m.T
		if m.Text != "" {
			what += " (" + m.Text + ")"
		}
		if c.isMe(m.From) {
			c.dropChan(ch, fmt.Sprintf("you were %s from #%s by %s", what, ch.name, m.By))
		} else {
			c.sysIn(ch, fmt.Sprintf("%s was %s by %s", m.From, what, m.By))
		}
	case "removed":
		c.dropChan(c.chanFor(m.Ch), fmt.Sprintf("#%s: %s", m.Ch, m.Text))
	case "invited":
		for _, ci := range m.Channels {
			ch := c.applyInfo(ci)
			c.write(Msg{T: "sub", Ch: ch.name, Since: ch.lastID})
			if m.By != "" {
				c.sysIn(ch, "you were added to #"+ch.name+" by "+m.By)
			}
			c.sys(fmt.Sprintf("joined #%s — /c %s to open it", ch.name, ch.name))
		}
		c.drawBar()
	case "muted":
		ch := c.chanFor(m.Ch)
		if m.Until == 0 {
			c.sysIn(ch, m.From+" unmuted by "+m.By)
		} else {
			d := humanDur(time.Until(time.UnixMilli(m.Until)))
			if c.isMe(m.From) {
				c.sysIn(ch, fmt.Sprintf("you are muted here for %s by %s %s", d, m.By, m.Text))
			} else {
				c.sysIn(ch, fmt.Sprintf("%s muted for %s by %s %s", m.From, d, m.By, m.Text))
			}
		}
	case "role":
		ch := c.chanFor(m.Ch)
		if c.isMe(m.From) {
			ch.role = m.Role
			c.drawStatus()
		}
		c.sysIn(ch, fmt.Sprintf("%s is now %s (by %s)", m.From, m.Role, m.By))
	case "topic":
		ch := c.chanFor(m.Ch)
		ch.topic = m.Text
		c.sysIn(ch, fmt.Sprintf("%s set the topic: %s", m.From, m.Text))
		c.drawStatus()
	case "read": // my other device read up to here
		ch := c.chanFor(m.Ch)
		if m.ID > ch.lastRead {
			ch.lastRead = m.ID
			if ch.lastRead >= ch.lastID {
				ch.unread = 0
			}
			c.drawBar()
		}
	case "quota":
		c.sys("⚠ " + m.Text)
	case "clip":
		c.onClip(m)
	case "who":
		c.sysIn(c.chanFor(m.Ch), "online: "+strings.Join(m.Users, ", "))
	case "res", "err":
		if cb := c.waiting[m.RID]; cb != nil {
			delete(c.waiting, m.RID)
			cb(m)
			return
		}
		if m.T == "err" {
			c.sys("hub: " + m.Text)
			return
		}
		c.printRes(m)
	}
}

func (c *client) printRes(m Msg) {
	if !m.OK {
		c.sys("✗ " + m.Text)
		return
	}
	if m.Text != "" {
		c.sys(m.Text)
	}
	for _, l := range m.Lines {
		c.addLine(c.active, "  "+l)
	}
}

func (c *client) applyInfo(ci ChanInfo) *chanState {
	ch := c.ensureChan(ci.Name)
	ch.topic, ch.role, ch.readonly, ch.expire = ci.Topic, ci.Role, ci.Readonly, ci.Expire
	if ci.Last > ch.lastID {
		ch.lastID = ci.Last
	}
	if ch != c.active {
		ch.unread = ci.Unread
	}
	return ch
}

func (c *client) onOK(m Msg) {
	c.legacy = m.V == 0
	c.me, c.role = m.User, m.Role
	if m.Limits != nil {
		c.maxMsg.Store(int64(m.Limits.MaxMsg))
		c.quotaLeft = m.Limits.QuotaLeft
	}
	placeholder := c.chanByName("…")
	if c.legacy {
		main := c.ensureChan("main")
		if placeholder != nil && placeholder != main {
			main.lines = append(placeholder.lines, main.lines...)
			c.removeChan(placeholder)
		}
		c.active = main
		c.ui.redraw(main.rendered())
		c.drawBar()
		c.drawStatus()
		c.sys("online: " + strings.Join(m.Users, ", ") + "  (v1 hub: single channel)")
		return
	}
	seen := map[string]bool{}
	for _, ci := range m.Channels {
		c.applyInfo(ci)
		seen[ci.Name] = true
	}
	var keep []*chanState
	var startup []line
	for _, ch := range c.chans {
		if seen[ch.name] {
			keep = append(keep, ch)
		} else if ch.name == "…" {
			startup = ch.lines
		}
	}
	c.chans = keep
	if len(c.chans) == 0 {
		c.active = c.ensureChan("…")
		c.sys("you're not in any channel yet — /join CODE with an invite, or ask an admin to /add you")
		c.drawStatus()
		return
	}
	// order: hub order (creation), which is what m.Channels gives us
	if c.active == nil || !seen[c.active.name] {
		want := c.snapshot().Active
		c.active = c.chanByName(want)
		if c.active == nil {
			c.active = c.chans[0]
		}
		if len(startup) > 0 {
			c.active.lines = append(startup, c.active.lines...)
		}
	}
	if c.snapshot().Active != c.active.name {
		name := c.active.name
		c.update(func(cf *Config) { cf.Active = name })
	}
	for _, ch := range c.chans {
		c.write(Msg{T: "sub", Ch: ch.name, Since: ch.lastID})
	}
	c.ui.redraw(c.active.rendered())
	c.drawBar()
	c.drawStatus()
}

func (c *client) removeChan(ch *chanState) {
	for i, x := range c.chans {
		if x == ch {
			c.chans = append(c.chans[:i], c.chans[i+1:]...)
			break
		}
	}
}

func (c *client) dropChan(ch *chanState, why string) {
	c.removeChan(ch)
	if c.active == ch {
		if len(c.chans) > 0 {
			c.active = c.chans[0]
			c.ui.redraw(c.active.rendered())
		} else {
			c.active = c.ensureChan("…")
			c.ui.redraw(nil)
		}
	}
	c.sys(why)
	c.drawBar()
	c.drawStatus()
}

func (c *client) onMsg(m Msg) {
	ch := c.chanFor(m.Ch)
	if m.ID > ch.lastID {
		ch.lastID = m.ID
	}
	if c.legacy && m.ID > c.legacySince.Load() {
		c.legacySince.Store(m.ID)
	}
	mine := c.isMe(m.From)
	mention := !mine && c.me != "" && strings.Contains(strings.ToLower(m.Text), "@"+strings.ToLower(c.me))
	var text string
	if m.T == "msg" {
		ch.texts[m.ID] = m.Text
		text = fmtMsg(m, mention)
	} else {
		ch.files[m.ID] = m
		text = fmtFile(m)
	}
	c.addLineID(ch, m.ID, m.TS, text)
	if ch == c.active {
		if !m.Hist {
			c.markRead(ch)
		}
	} else if !m.Hist {
		ch.unread++
		c.drawBar()
		if c.snapshot().Notify != "off" && (mention || !mine) {
			c.ui.bell()
		}
	} else if mention {
		c.ui.bell()
	}
	if m.T != "file" || m.Hist {
		return
	}
	fromThisDevice := mine && (m.Device == "" || strings.EqualFold(m.Device, c.snapshot().Name))
	if fromThisDevice {
		return
	}
	if m.Size <= int64(c.snapshot().AutoMB)<<20 && (ch == c.active || c.watched(ch.name)) {
		go func() {
			p, err := c.download(m)
			if err != nil {
				c.events <- Msg{T: "_sys", Ch: m.Ch, Text: "download failed: " + err.Error()}
				return
			}
			c.events <- Msg{T: "_saved", Ch: m.Ch, ID: m.ID, Text: p}
		}()
	} else {
		c.sysIn(ch, fmt.Sprintf("file not auto-downloaded — /get %d (or /watch %s)", m.ID, ch.name))
	}
}

// ---- formatting -------------------------------------------------------------

func fmtMsg(m Msg, mention bool) string {
	t := time.UnixMilli(m.TS).Format("15:04")
	body := m.Text
	if strings.Contains(body, "\n") { // multi-line paste: indent continuation lines
		body = strings.ReplaceAll(body, "\n", "\n"+strings.Repeat(" ", 8))
	}
	if mention {
		body = "\x1b[33m" + body + "\x1b[0m"
	}
	return fmt.Sprintf("%s %s %s %s", dim(t), colorNick(m.From), dim(fmt.Sprintf("#%d", m.ID)), body)
}

func fmtFile(m Msg) string {
	t := time.UnixMilli(m.TS).Format("15:04")
	return fmt.Sprintf("%s %s %s %s %s %s", dim(t), colorNick(m.From), dim(fmt.Sprintf("#%d", m.ID)),
		"\x1b[33m📎", bold(m.Name), dim("("+humanSize(m.Size)+")")+"\x1b[0m")
}

// ---- OS glue ----------------------------------------------------------------

func openFolder(p string) {
	os.MkdirAll(p, 0o755)
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", p).Start()
	case "windows":
		exec.Command("explorer", p).Start()
	default:
		exec.Command("xdg-open", p).Start()
	}
}

// asFilePath cleans a dragged/pasted path and returns it if it's a regular file.
func asFilePath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	if runtime.GOOS != "windows" {
		s = strings.ReplaceAll(s, `\ `, " ") // macOS Terminal escapes spaces on drop
	}
	if strings.HasPrefix(s, "file://") {
		s = unescape(strings.TrimPrefix(s, "file://"))
	}
	if strings.HasPrefix(s, "~") {
		home, _ := os.UserHomeDir()
		s = home + s[1:]
	}
	st, err := os.Stat(s)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return s
}

// ---- one-shot: bch send [-ch NAME] <file|text> -------------------------------

func runSend(args []string) {
	// flags may precede the payload: bch send -ch design file.png
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-tls":
			flags = append(flags, args[i])
		case strings.HasPrefix(args[i], "-") && i+1 < len(args) && len(rest) == 0:
			flags = append(flags, args[i], args[i+1])
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, dir, f := parseClientFlags("send", flags)
	c := newClient(cfg, dir)
	c.bootstrapAuth(f)
	ch := f.ch
	if ch == "" {
		ch = c.snapshot().Active
	}
	payload := strings.Join(rest, " ")
	if payload == "" || payload == "-" { // stdin
		b, _ := io.ReadAll(os.Stdin)
		payload = strings.TrimRight(string(b), "\r\n")
	}
	if p := asFilePath(payload); p != "" {
		if err := c.upload(ch, p); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("sent", filepath.Base(p))
		return
	}
	conn, err := c.dial(c.tcpAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer conn.Close()
	c.conn = conn
	cfg = c.snapshot()
	if cfg.Session != "" {
		c.write(Msg{T: "hello", V: protoVersion, Device: cfg.Name, Auth: &Auth{Session: cfg.Session}})
	} else {
		c.write(Msg{T: "hello", V: protoVersion, Name: cfg.Name, Device: cfg.Name, Token: cfg.Token, Since: 1 << 62})
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	gotOK := false
	for sc.Scan() {
		var m Msg
		json.Unmarshal(sc.Bytes(), &m)
		if m.T == "ok" {
			gotOK = true
			break
		}
		if m.T == "err" {
			fmt.Fprintln(os.Stderr, "error:", m.Text)
			os.Exit(1)
		}
	}
	if !gotOK {
		fmt.Fprintln(os.Stderr, "error: no reply from hub — wrong port, or the hub uses TLS (try -hub tls://HOST)")
		os.Exit(1)
	}
	c.write(Msg{T: "msg", Ch: ch, Text: payload, RID: "send"})
	conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	for sc.Scan() { // give the hub a moment to refuse (rate limit, read-only, no such channel)
		var m Msg
		json.Unmarshal(sc.Bytes(), &m)
		if m.T == "err" && m.RID == "send" {
			fmt.Fprintln(os.Stderr, "error:", m.Text)
			os.Exit(1)
		}
		if m.T == "msg" && m.Text == payload && !m.Hist {
			break
		}
	}
	fmt.Println("sent")
}

// sortedKeys is a small helper for deterministic output.
func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
