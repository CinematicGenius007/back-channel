package main

// Slash commands. Anything the hub decides is sent as a `cmd` frame (see cmds.go); the
// TUI only parses arguments and prints the reply. Channel switching, clipboard and
// local files are handled here.

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// prompt collects one or more hidden-input answers (passwords) from the input line.
type prompt struct {
	labels  []string
	answers []string
	done    func([]string)
}

func (c *client) ask(labels []string, done func([]string)) {
	c.pending = &prompt{labels: labels, done: done}
	echoOff()
	c.ui.setPrompt(labels[0] + " ")
}

func (c *client) feedPrompt(line string) {
	p := c.pending
	p.answers = append(p.answers, strings.TrimRight(line, "\r\n"))
	if len(p.answers) < len(p.labels) {
		c.ui.setPrompt(p.labels[len(p.answers)] + " ")
		return
	}
	c.pending = nil
	echoOn()
	c.ui.setPrompt("> ")
	p.done(p.answers)
}

func fields(s string) []string { return strings.Fields(strings.TrimSpace(s)) }

// kv splits "a=b c=d" tokens into a map; plain tokens go under "" in order.
func kv(tokens []string) (map[string]string, []string) {
	m := map[string]string{}
	var plain []string
	for _, t := range tokens {
		if k, v, ok := strings.Cut(t, "="); ok && k != "" {
			m[k] = v
		} else {
			plain = append(plain, t)
		}
	}
	return m, plain
}

// handleInput returns false to quit.
func (c *client) handleInput(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}
	// drop-in: a bare path to an existing file gets sent as a file
	if p := asFilePath(line); p != "" {
		go c.sendFile(p)
		return true
	}
	if !strings.HasPrefix(line, "/") {
		if c.active == nil || c.active.name == "…" {
			c.sys("not in a channel — /join CODE first")
			return true
		}
		if len(line) > int(c.maxMsg.Load()) {
			c.sys(fmt.Sprintf("too long for one message (hub limit %s) — send it as a file", humanSize(c.maxMsg.Load())))
			return true
		}
		text, epoch, ok := c.encryptForSend(c.active, line)
		if !ok {
			c.sys("no channel key yet for #" + c.active.name + " — /e2e status")
			return true
		}
		ch := ""
		if !c.legacy {
			ch = c.active.name
		}
		if err := c.write(Msg{T: "msg", Ch: ch, Text: text, Epoch: epoch}); err != nil {
			c.sys("not sent (offline): " + line)
		}
		return true
	}
	cmd, arg, _ := strings.Cut(line[1:], " ")
	arg = strings.TrimSpace(arg)
	cmd = strings.ToLower(cmd)
	f := fields(arg)

	// channel shortcuts: /1 … /9, /NAME
	if n, err := strconv.Atoi(cmd); err == nil && n >= 1 && n <= len(c.chans) {
		c.switchTo(c.chans[n-1])
		return true
	}
	if ch := c.chanByName(cmd); ch != nil && !isBuiltin(cmd) {
		c.switchTo(ch)
		return true
	}

	switch cmd {
	case "q", "quit", "exit":
		return false
	case "help", "h", "?":
		c.help(arg)

	// ---- channels ----------------------------------------------------------------
	case "c", "ch", "switch", "go":
		if ch := c.chanByName(arg); ch != nil {
			c.switchTo(ch)
		} else {
			c.sys("you're not in #" + arg + " — /channels lists yours")
		}
	case "n", "next":
		c.cycle(1)
	case "p", "prev":
		c.cycle(-1)
	case "channels", "list", "ls":
		c.cmd("channels", nil, func(m Msg) {
			if !m.OK {
				c.printRes(m)
				return
			}
			for _, ci := range m.Channels {
				ch := c.applyInfo(ci)
				line := fmt.Sprintf("#%-16s %-8s %3d members", ci.Name, ci.Role, ci.Members)
				if ci.Readonly {
					line += "  read-only"
				}
				if ci.Expire > 0 {
					line += "  🔥 " + fmtExpire(ci.Expire)
				}
				if ci.Topic != "" {
					line += "  — " + ci.Topic
				}
				c.addLine(c.active, "  "+line)
				if ch.lastRead == 0 && ch.lastID == 0 {
					c.write(Msg{T: "sub", Ch: ch.name, Since: 0})
				}
			}
			c.drawBar()
		})
	case "create":
		if len(f) == 0 {
			c.sys("usage: /create NAME [topic words…] [readonly=on] [e2e=on] [expire=10m|2h|7d]")
			return true
		}
		args, plain := kv(f[1:])
		args["name"] = f[0]
		args["topic"] = strings.Join(plain, " ")
		c.cmd("create", args, c.onJoined)
	case "join":
		if arg == "" {
			c.sys("usage: /join INVITE-CODE")
			return true
		}
		c.cmd("join", map[string]string{"code": arg}, c.onJoined)
	case "leave":
		if c.active == nil {
			return true
		}
		leaving := c.active
		c.cmd("leave", nil, func(m Msg) {
			c.printRes(m)
			if m.OK {
				c.dropChan(leaving, "left #"+leaving.name)
			}
		})
	case "delete":
		c.cmd("delete", map[string]string{"confirm": arg}, nil)
	case "topic":
		c.cmd("topic", map[string]string{"text": arg}, nil)
	case "members", "m":
		c.cmd("members", nil, nil)
	case "who", "w":
		ch := ""
		if !c.legacy && c.active != nil {
			ch = c.active.name
		}
		c.write(Msg{T: "who", Ch: ch})
	case "invite":
		args, plain := kv(f)
		if len(plain) > 0 {
			args["uses"] = plain[0]
		}
		if len(plain) > 1 {
			args["hours"] = plain[1]
		}
		if len(plain) > 2 {
			args["role"] = plain[2]
		}
		c.cmd("invite", args, nil)
	case "invites":
		c.cmd("invites", nil, nil)
	case "revoke":
		c.cmd("revoke", map[string]string{"code": arg}, nil)
	case "add":
		c.cmd("add", map[string]string{"user": arg}, nil)
	case "kick":
		if len(f) == 0 {
			c.sys("usage: /kick NAME [reason]")
			return true
		}
		kicked := c.active
		c.cmd("kick", map[string]string{"user": f[0], "reason": strings.Join(f[1:], " ")}, func(m Msg) {
			c.printRes(m)
			if m.OK && kicked != nil {
				c.afterModeration(kicked.name) // e2e channels: nudge a key rotation
			}
		})
	case "ban":
		if len(f) == 0 {
			c.sys("usage: /ban NAME [days] [reason]   (0 or no days = permanent)")
			return true
		}
		args := map[string]string{"user": f[0]}
		rest := f[1:]
		if len(rest) > 0 {
			if _, err := strconv.Atoi(rest[0]); err == nil {
				args["days"] = rest[0]
				rest = rest[1:]
			}
		}
		args["reason"] = strings.Join(rest, " ")
		banned := c.active
		c.cmd("ban", args, func(m Msg) {
			c.printRes(m)
			if m.OK && banned != nil {
				c.afterModeration(banned.name)
			}
		})
	case "unban":
		c.cmd("unban", map[string]string{"user": arg}, nil)
	case "bans":
		c.cmd("bans", nil, nil)
	case "mute":
		if len(f) == 0 {
			c.sys("usage: /mute NAME [minutes] [reason]")
			return true
		}
		args := map[string]string{"user": f[0], "minutes": "10"}
		rest := f[1:]
		if len(rest) > 0 {
			if _, err := strconv.Atoi(rest[0]); err == nil {
				args["minutes"] = rest[0]
				rest = rest[1:]
			}
		}
		args["reason"] = strings.Join(rest, " ")
		c.cmd("mute", args, nil)
	case "unmute":
		c.cmd("mute", map[string]string{"user": arg, "minutes": "0"}, nil)
	case "role":
		if len(f) != 2 {
			c.sys("usage: /role NAME readonly|member|mod|admin")
			return true
		}
		c.cmd("role", map[string]string{"user": f[0], "role": f[1]}, nil)
	case "transfer":
		c.cmd("transfer", map[string]string{"user": arg}, nil)
	case "del", "rm":
		c.cmd("del", map[string]string{"id": arg}, nil)
	case "purge":
		args := map[string]string{}
		switch {
		case len(f) == 2 && f[0] == "last":
			args["last"] = f[1]
		case len(f) == 2 && f[0] == "before":
			args["before"] = f[1]
		case len(f) == 1:
			args["user"] = f[0]
		default:
			c.sys("usage: /purge NAME  |  /purge last N  |  /purge before #ID")
			return true
		}
		c.cmd("purge", args, nil)
	case "settings":
		args, _ := kv(f)
		c.cmd("settings", args, nil)

	// ---- account -----------------------------------------------------------------
	case "login":
		if arg == "" {
			c.sys("usage: /login NAME")
			return true
		}
		user := arg
		c.ask([]string{"password for " + user + ":"}, func(a []string) {
			go c.loginAndReconnect(Auth{User: user, Pass: a[0]})
		})
	case "register":
		if len(f) != 2 {
			c.sys("usage: /register INVITE-CODE USERNAME")
			return true
		}
		code, user := f[0], f[1]
		c.ask([]string{"choose a password (8+ chars):", "repeat it:"}, func(a []string) {
			if a[0] != a[1] {
				c.sys("passwords don't match")
				return
			}
			go c.loginAndReconnect(Auth{Invite: code, User: user, Pass: a[0]})
		})
	case "passwd":
		c.ask([]string{"current password:", "new password (8+ chars):", "repeat new password:"}, func(a []string) {
			if a[1] != a[2] {
				c.sys("new passwords don't match")
				return
			}
			c.cmd("passwd", map[string]string{"old": a[0], "new": a[1]}, nil)
		})
	case "sessions":
		args := map[string]string{}
		if len(f) == 2 && f[0] == "revoke" {
			args["revoke"] = f[1]
		}
		c.cmd("sessions", args, nil)
	case "logout":
		args := map[string]string{}
		if arg == "all" {
			args["all"] = "on"
		}
		c.cmd("logout", args, func(m Msg) {
			c.printRes(m)
			if m.OK {
				c.update(func(cf *Config) { cf.Session = "" })
				c.sys("session cleared — /login NAME to sign in again, or /quit")
			}
		})

	// ---- server admin ------------------------------------------------------------
	case "admin", "a":
		c.adminCmd(f)

	// ---- files -------------------------------------------------------------------
	case "send", "s", "f":
		if p := asFilePath(arg); p != "" {
			go c.sendFile(p)
		} else {
			c.sys("no such file: " + arg)
		}
	case "get", "g":
		c.get(arg)
	case "open", "o":
		dir := filepath.Join(c.dir, "inbox")
		if c.active != nil && !c.legacy && c.active.name != "…" {
			dir = filepath.Join(dir, c.active.name)
		}
		openFolder(dir)
	case "watch":
		name := arg
		if name == "" && c.active != nil {
			name = c.active.name
		}
		on := !c.watched(name)
		c.update(func(cf *Config) {
			var w []string
			for _, x := range cf.Watch {
				if !strings.EqualFold(x, name) {
					w = append(w, x)
				}
			}
			if on {
				w = append(w, name)
			}
			cf.Watch = w
		})
		if on {
			c.sys("#" + name + ": files now auto-download even when it's not the active channel")
		} else {
			c.sys("#" + name + ": auto-download only while active")
		}

	// ---- clipboard ---------------------------------------------------------------
	case "paste", "pa":
		txt, err := clipboardRead()
		if err != nil || strings.TrimSpace(txt) == "" {
			c.sys("clipboard empty or unavailable")
			return true
		}
		text, epoch, ok := c.encryptForSend(c.active, strings.TrimRight(txt, "\r\n"))
		if !ok {
			c.sys("no channel key yet for #" + c.active.name + " — /e2e status")
			return true
		}
		ch := ""
		if !c.legacy && c.active != nil {
			ch = c.active.name
		}
		if err := c.write(Msg{T: "msg", Ch: ch, Text: text, Epoch: epoch}); err != nil {
			c.sys("not sent (offline)")
		}
	case "copy", "y":
		if c.active == nil {
			return true
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
		if id == 0 {
			for k := range c.active.texts {
				if k > id {
					id = k
				}
			}
		}
		txt, ok := c.active.texts[id]
		if !ok {
			c.sys("no text message with that id in this channel")
		} else if err := clipboardWrite(txt); err != nil {
			c.sys("clipboard unavailable: " + err.Error())
		} else {
			c.clipSet(txt) // don't sync our own copy back out
			c.sys(fmt.Sprintf("copied #%d to clipboard", id))
		}
	case "clip":
		c.clipCmd(arg)
	case "e2e":
		c.e2eCmd(f)
	case "verify":
		c.verifyCmd(arg)
	case "notify":
		c.update(func(cf *Config) {
			if arg == "off" {
				cf.Notify = "off"
			} else {
				cf.Notify = ""
			}
		})
		c.sys("notifications " + map[bool]string{true: "off", false: "on"}[arg == "off"])
	case "clear":
		if c.active != nil {
			c.active.lines = nil
		}
		c.ui.clear()
		c.drawBar()
	default:
		c.sys("unknown command /" + cmd + " — /help")
	}
	return true
}

func (c *client) onJoined(m Msg) {
	c.printRes(m)
	if !m.OK {
		return
	}
	for _, ci := range m.Channels {
		ch := c.applyInfo(ci)
		c.write(Msg{T: "sub", Ch: ch.name, Since: ch.lastID})
		c.switchTo(ch)
	}
}

func (c *client) cycle(d int) {
	if len(c.chans) < 2 || c.active == nil {
		return
	}
	for i, ch := range c.chans {
		if ch == c.active {
			c.switchTo(c.chans[(i+d+len(c.chans))%len(c.chans)])
			return
		}
	}
}

func (c *client) adminCmd(f []string) {
	if len(f) == 0 {
		c.sys("/admin users | useradd NAME [PASS] | ban NAME [days] [reason] | unban NAME | mod NAME key=value… | audit [N] [user=X] [ch=Y] | stats | set [KEY VALUE] | prune")
		return
	}
	sub, rest := f[0], f[1:]
	switch sub {
	case "users":
		c.cmd("users", nil, nil)
	case "useradd", "adduser":
		if len(rest) == 0 {
			c.sys("usage: /admin useradd NAME [PASSWORD]  (a password is generated if omitted)")
			return
		}
		args := map[string]string{"name": rest[0]}
		if len(rest) > 1 {
			args["pass"] = rest[1]
		}
		c.cmd("useradd", args, nil)
	case "ban":
		if len(rest) == 0 {
			c.sys("usage: /admin ban NAME [days] [reason]")
			return
		}
		args := map[string]string{"user": rest[0]}
		rest = rest[1:]
		if len(rest) > 0 {
			if _, err := strconv.Atoi(rest[0]); err == nil {
				args["days"] = rest[0]
				rest = rest[1:]
			}
		}
		args["reason"] = strings.Join(rest, " ")
		c.cmd("userban", args, nil)
	case "unban":
		c.cmd("userunban", map[string]string{"user": strings.Join(rest, "")}, nil)
	case "mod", "usermod":
		if len(rest) < 2 {
			c.sys("usage: /admin mod NAME role=admin|user|owner  pass=NEWPASSWORD  quota_mb=N  status=active|disabled  msg_rate=N  upload_rate=N")
			return
		}
		args, _ := kv(rest[1:])
		args["user"] = rest[0]
		c.cmd("usermod", args, nil)
	case "audit":
		args, plain := kv(rest)
		if len(plain) > 0 {
			args["n"] = plain[0]
		}
		c.cmd("audit", args, nil)
	case "stats":
		c.cmd("stats", nil, nil)
	case "set":
		args := map[string]string{}
		if len(rest) >= 2 {
			args[rest[0]] = strings.Join(rest[1:], " ")
		} else if len(rest) == 1 {
			c.sys("usage: /admin set KEY VALUE   (/admin set alone lists everything)")
			return
		}
		c.cmd("set", args, nil)
	case "prune":
		c.cmd("prune", nil, nil)
	default:
		c.sys("unknown admin command — /admin")
	}
}

func (c *client) loginAndReconnect(a Auth) {
	ok, err := c.loginOnce(a)
	if err != nil {
		c.events <- Msg{T: "_sys", Text: "login failed: " + err.Error()}
		return
	}
	c.update(func(cf *Config) { cf.User, cf.Session, cf.Token = ok.User, ok.Session, "" })
	c.events <- Msg{T: "_sys", Text: "logged in as " + ok.User + " — reconnecting"}
	c.reconnect()
}

func (c *client) sendFile(p string) {
	ch := c.activeName()
	if err := c.upload(ch, p); err != nil {
		c.status("")
		c.events <- Msg{T: "_sys", Text: "send failed: " + err.Error()}
	}
}

func (c *client) get(arg string) {
	if c.active == nil {
		return
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	var m Msg
	ok := false
	if id == 0 { // latest file in the active channel
		for k, v := range c.active.files {
			if k > m.ID {
				m, ok = v, true
			}
		}
	} else {
		for _, ch := range c.chans { // any channel: ids are global
			if v, found := ch.files[id]; found {
				m, ok = v, true
				break
			}
		}
	}
	if !ok {
		c.sys("no file with that id (ids are shown as #n)")
		return
	}
	go func() {
		c.status("downloading " + m.Name + "…")
		p, err := c.download(m)
		c.status("")
		if err != nil {
			c.events <- Msg{T: "_sys", Text: "download failed: " + err.Error()}
		} else {
			c.events <- Msg{T: "_sys", Text: "saved " + p}
		}
	}()
}

func (c *client) clipCmd(arg string) {
	switch arg {
	case "on":
		c.clipOn.Store(true)
		c.update(func(cf *Config) { cf.ClipSync = true })
		c.sys("clipboard sync ON — text you copy here appears in the clipboard of your other devices that also have it on (only devices logged in as you)")
		c.drawStatus()
	case "off":
		c.clipOn.Store(false)
		c.update(func(cf *Config) { cf.ClipSync = false })
		c.sys("clipboard sync off")
		c.drawStatus()
	case "push":
		txt, err := clipboardRead()
		if err != nil || txt == "" {
			c.sys("clipboard empty or unavailable")
			return
		}
		if int64(len(txt)) > c.maxMsg.Load() {
			c.sys(fmt.Sprintf("clipboard is %s — over the hub's %s limit", humanSize(int64(len(txt))), humanSize(c.maxMsg.Load())))
			return
		}
		c.clipSet(txt)
		if err := c.write(Msg{T: "clip", Text: txt}); err == nil {
			c.sys(fmt.Sprintf("pushed clipboard (%s) to your other devices", humanSize(int64(len(txt)))))
		}
	case "pull":
		c.clipMu.Lock()
		txt := c.clipIn
		c.clipMu.Unlock()
		if txt == "" {
			c.sys("nothing received yet")
			return
		}
		if err := clipboardWrite(txt); err != nil {
			c.sys("clipboard unavailable: " + err.Error())
			return
		}
		c.clipSet(txt)
		c.sys(fmt.Sprintf("clipboard set (%s)", humanSize(int64(len(txt)))))
	default:
		state := "off"
		if c.clipOn.Load() {
			state = "on"
		}
		c.sys("clipboard sync is " + state + " — /clip on|off  /clip push (send once)  /clip pull (apply last received)")
	}
}

var builtins = map[string]bool{}

func isBuiltin(cmd string) bool { return builtins[cmd] }

func init() {
	for _, k := range strings.Fields("q quit exit help h c ch switch go n next p prev channels list ls create join leave delete topic members m who w invite invites revoke add kick ban unban bans mute unmute role transfer del rm purge settings login register passwd sessions logout admin a send s f get g open o watch paste pa copy y clip e2e verify notify clear") {
		builtins[k] = true
	}
}

func (c *client) help(topic string) {
	lines := []string{
		bold("channels") + "   /1 … /9  /n  /p  /c NAME  (or just /NAME)   /channels   /join CODE   /leave   /who   /members",
		bold("files") + "      drag a file in + Enter   /send PATH   /get [#id]   /open   /watch [NAME]   (outbox/<channel>/ also works)",
		bold("clipboard") + "  /paste (clipboard → channel)   /copy [#id] (message → clipboard)   /clip on|off|push|pull (sync between my devices)",
		bold("messages") + "   /del #id   @NAME to mention   /notify on|off   /clear",
		bold("encryption") + " /create NAME e2e=on   /e2e status|rotate|reshare   /verify NAME (compare device fingerprints) — see ENCRYPTION.md",
		bold("mod+") + "       /topic TEXT   /invite [uses] [hours] [role]   /invites   /revoke CODE   /add NAME   /kick NAME [why]   /ban NAME [days] [why]   /unban NAME   /bans   /mute NAME [min]   /unmute NAME",
		bold("admin+") + "     /role NAME readonly|member|mod|admin   /settings expire=10m|2h|7d|off readonly=on|off max_file_mb=N   /purge NAME|last N|before #id (owner)   /transfer NAME (owner)   /delete NAME (owner)",
		bold("account") + "    /login NAME   /register CODE NAME   /passwd   /sessions [revoke ID]   /logout [all]",
		bold("server") + "     /admin users|useradd|ban|unban|mod|audit|stats|set|prune   /create NAME [topic]",
		dim("           /quit or Ctrl-C to leave · full docs: README.md, ADMIN-GUIDE.md"),
	}
	for _, l := range lines {
		c.addLine(c.active, l)
	}
}
