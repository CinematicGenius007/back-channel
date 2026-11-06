package main

import (
	"fmt"
	"strings"
)

// Manage: one screen for channels, people, invites, devices, files, appearance and
// (for server admins) the hub. Every action is an existing slash command — the form
// shows the exact command it will run, so the screen also teaches the commands.

type action struct {
	label, desc string
	cmd         string       // run as typed
	form        func() *form // or ask for arguments first
	run         func()       // or do something local
	stay        bool         // stay on Manage afterwards (local toggles)
}

type section struct {
	id, name, title string
	info            [][2]string
	actions         []action
}

var sectionIDs = []string{"channels", "people", "invites", "security", "files", "appearance", "server"}

func sectionIndex(id string) int {
	for i, s := range sectionIDs {
		if s == id {
			return i
		}
	}
	return 0
}

func (a *app) openManage(sec int) {
	a.mode = scrManage
	a.ov = ovNone
	secs := a.sections()
	if sec >= len(secs) {
		sec = 0
	}
	a.msec, a.msel = sec, 0
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (a *app) sections() []section {
	c := a.c
	cfg := c.snapshot()
	act := c.active
	actName := ""
	if act != nil && act.name != "…" {
		actName = act.name
	}
	var out []section

	// channels
	chs := section{id: "channels", name: "Channels", title: "Your channels."}
	for _, ch := range c.chans {
		if ch.name == "…" {
			continue
		}
		var flags []string
		if ch.role != "" {
			flags = append(flags, ch.role)
		}
		if ch.members > 0 {
			flags = append(flags, fmt.Sprintf("%d members", ch.members))
		}
		if ch.e2e {
			flags = append(flags, "e2e")
		}
		if ch.expire > 0 {
			flags = append(flags, "expires "+fmtExpire(ch.expire))
		}
		if ch.readonly {
			flags = append(flags, "read-only")
		}
		chs.info = append(chs.info, [2]string{"#" + ch.name, strings.Join(flags, " · ")})
	}
	if len(chs.info) == 0 {
		chs.info = append(chs.info, [2]string{"none yet", "join one with an invite code, or create one"})
	}
	chs.actions = []action{
		{label: "New channel…", desc: "/create — name, topic, e2e, self-destruct", form: newChannelForm},
		{label: "Join with an invite code…", desc: "/join CODE", form: joinForm},
	}
	if actName != "" {
		chs.actions = append(chs.actions,
			action{label: "Set the topic of #" + actName + "…", desc: "/topic TEXT", form: topicForm},
			action{label: "Settings for #" + actName + "…", desc: "/settings — expiry, read-only, file size", form: settingsForm},
			action{label: watchLabel(c, actName), desc: "/watch — auto-download files while it is not open", cmd: "/watch " + actName},
			action{label: "Leave #" + actName + "…", desc: "/leave — you need a new invite to come back", form: leaveForm(actName)},
		)
	}
	out = append(out, chs)

	// people
	ppl := section{id: "people", name: "People", title: "People in #" + actName + "."}
	if actName == "" {
		ppl.title = "People."
	} else if act.online != nil {
		ppl.info = append(ppl.info, [2]string{"online", strings.Join(act.online, ", ")})
	}
	ppl.actions = []action{
		{label: "Who is online", desc: "/who", cmd: "/who"},
		{label: "Everyone in this channel", desc: "/members", cmd: "/members"},
		{label: "Add someone with an account…", desc: "/add NAME", form: addForm},
		{label: "Mute…", desc: "/mute NAME [minutes] [reason]", form: muteForm},
		{label: "Unmute…", desc: "/unmute NAME", form: nameForm("Unmute", "/unmute")},
		{label: "Kick…", desc: "/kick NAME [reason]", form: kickForm},
		{label: "Ban…", desc: "/ban NAME [days] [reason]", form: banForm},
		{label: "Lift a ban…", desc: "/unban NAME", form: nameForm("Lift a ban", "/unban")},
		{label: "List bans", desc: "/bans", cmd: "/bans"},
		{label: "Change someone's role…", desc: "/role NAME ROLE", form: roleForm},
	}
	out = append(out, ppl)

	// invites
	inv := section{id: "invites", name: "Invites", title: "Bring people in."}
	inv.actions = []action{
		{label: "New invite for #" + actName + "…", desc: "/invite [uses] [hours] [role]", form: inviteForm},
		{label: "List open invites", desc: "/invites", cmd: "/invites"},
		{label: "Revoke an invite…", desc: "/revoke CODE", form: nameForm("Revoke an invite", "/revoke")},
		{label: "Join with an invite code…", desc: "/join CODE", form: joinForm},
	}
	out = append(out, inv)

	// security
	sec := section{id: "security", name: "Devices & security", title: "This device and your account."}
	sec.info = [][2]string{{"account", firstNonEmpty(cfg.User, "guest")}, {"device", cfg.Name}}
	if cfg.E2EPub != "" {
		sec.info = append(sec.info, [2]string{"device key", keyFingerprint(cfg.E2EPub)})
	}
	transport := "plain TCP (fine on a trusted LAN)"
	if cfg.TLS {
		transport = "TLS"
		if cfg.FP != "" {
			transport += " · pinned " + clip(cfg.FP, 23)
		}
	}
	sec.info = append(sec.info, [2]string{"hub", cfg.Hub}, [2]string{"transport", transport})
	sec.actions = []action{
		{label: "Encryption status of #" + actName, desc: "/e2e status", cmd: "/e2e status"},
		{label: "Rotate this channel's key", desc: "/e2e rotate — after someone leaves", cmd: "/e2e rotate"},
		{label: "Share the key with devices that lack it", desc: "/e2e reshare", cmd: "/e2e reshare"},
		{label: "Verify someone's devices…", desc: "/verify NAME — compare fingerprints out of band", form: nameForm("Verify devices", "/verify")},
		{label: "Signed-in sessions", desc: "/sessions", cmd: "/sessions"},
		{label: "Change password", desc: "/passwd — asks on the input line", cmd: "/passwd"},
		{label: "Sign out this device", desc: "/logout", cmd: "/logout"},
		{label: "Sign out everywhere", desc: "/logout all", cmd: "/logout all"},
		{label: "Run setup again", desc: "bch setup — hub, account, device, look", run: func() {
			a.c.sys("run  " + binName + " setup  to go through setup again")
			a.mode = scrMain
		}},
	}
	out = append(out, sec)

	// files
	fl := section{id: "files", name: "Files & clipboard", title: "Files and clipboard."}
	fl.info = [][2]string{
		{"clipboard", "sync " + onOff(c.clipOn.Load()) + " — only between your own devices, never stored"},
		{"auto-download", fmt.Sprintf("up to %d MB", cfg.AutoMB)},
		{"inbox", c.dir + "/inbox/<channel>"},
	}
	if len(cfg.Watch) > 0 {
		fl.info = append(fl.info, [2]string{"always download", "#" + strings.Join(cfg.Watch, ", #")})
	}
	fl.actions = []action{
		{label: "Clipboard sync: turn " + onOff(!c.clipOn.Load()), desc: "/clip " + onOff(!c.clipOn.Load()), cmd: "/clip " + onOff(!c.clipOn.Load()), stay: true},
		{label: "Send my clipboard to my other devices now", desc: "/clip push", cmd: "/clip push"},
		{label: "Use the last clipboard I received", desc: "/clip pull", cmd: "/clip pull"},
		{label: "Send a file…", desc: "/send PATH — or drag it into the window", form: sendForm},
		{label: "Download the latest file here", desc: "/get", cmd: "/get"},
		{label: "Open the inbox folder", desc: "/open", cmd: "/open", stay: true},
	}
	out = append(out, fl)

	// appearance
	ap := section{id: "appearance", name: "Appearance", title: "Theme and input."}
	ap.info = [][2]string{{"colour", fmt.Sprintf("%d-bit", a.depth)}, {"theme", a.th.name}}
	for _, t := range themes {
		t := t
		mark := "  "
		if t == a.th {
			mark = "● "
		}
		ap.actions = append(ap.actions, action{label: mark + t.name, desc: "/theme " + t.id, run: func() { a.applyTheme(t.id) }, stay: true})
	}
	ap.actions = append(ap.actions,
		action{label: "Mouse and touch: turn " + onOff(!a.mouse), desc: "/mouse " + onOff(!a.mouse) + " — off allows normal text selection",
			run: func() { a.applyMouse(!a.mouse) }, stay: true},
		action{label: "Bell for new messages: turn " + onOff(cfg.Notify == "off"), desc: "/notify " + onOff(cfg.Notify == "off"),
			cmd: "/notify " + onOff(cfg.Notify == "off"), stay: true},
	)
	out = append(out, ap)

	if c.role == "owner" || c.role == "admin" {
		sv := section{id: "server", name: "Server", title: "This hub."}
		sv.actions = []action{
			{label: "All accounts", desc: "/admin users", cmd: "/admin users"},
			{label: "Create an account…", desc: "/admin useradd NAME", form: nameForm("Create an account", "/admin useradd")},
			{label: "Hub statistics", desc: "/admin stats", cmd: "/admin stats"},
			{label: "Audit log (last 50)", desc: "/admin audit 50", cmd: "/admin audit 50"},
			{label: "Hub settings", desc: "/admin set", cmd: "/admin set"},
			{label: "Remove unreferenced files", desc: "/admin prune", cmd: "/admin prune"},
		}
		out = append(out, sv)
	}
	return out
}

func watchLabel(c *client, name string) string {
	if c.watched(name) {
		return "Stop always downloading files from #" + name
	}
	return "Always download files from #" + name
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *app) runAction(ac action) {
	switch {
	case ac.form != nil:
		f := ac.form()
		f.stay = ac.stay
		a.openForm(f)
	case ac.run != nil:
		ac.run()
	case ac.cmd != "":
		if !ac.stay {
			a.mode = scrMain
		}
		a.submit(ac.cmd)
	}
	a.dirty = true
}

func (a *app) drawManage(cv *canvas) {
	a.drawTitle(cv, "manage")
	secs := a.sections()
	if a.msec >= len(secs) {
		a.msec = 0
	}
	s := secs[a.msec]
	if a.msel >= len(s.actions) {
		a.msel = max(0, len(s.actions)-1)
	}
	top := a.bodyTop()
	bot := a.h - 2
	navW := 0
	if a.w >= 72 {
		navW = 24
		cv.text(2, top, navW-2, "MANAGE", a.stMuted())
		for i, sc := range secs {
			i := i
			y := top + 1 + i
			st := a.stCopy()
			if i == a.msec {
				st = a.stSel()
				cv.fill(0, y, navW, 1, st)
				cv.text(0, y, 1, "▌", style{fg: a.th.sig, bg: st.bg})
			}
			cv.text(2, y, navW-3, sc.name, st)
			a.onHit(0, y, navW, 1, func() { a.msec, a.msel = i, 0 })
		}
		cv.vline(navW, top, bot-top, '│', a.stRule())
		help := []string{"every action is the", "slash command shown", "beside it, checked", "by the hub like any", "other command"}
		for i, l := range help {
			if y := bot - len(help) + i; y > top+len(secs)+1 {
				cv.text(2, y, navW-3, l, a.stMuted())
			}
		}
	} else {
		// narrow: ‹ Section › header instead of a side list
		y := top
		a.button(cv, 1, y, "‹", a.stSig(), func() { a.msec, a.msel = (a.msec+len(secs)-1)%len(secs), 0 })
		cv.text(3, y, a.w-8, clip(s.name, a.w-8), style{fg: a.th.ink, bg: a.th.bg, bold: true})
		a.button(cv, a.w-2, y, "›", a.stSig(), func() { a.msec, a.msel = (a.msec+1)%len(secs), 0 })
		top++
	}

	x := navW + 3
	w := a.w - x - 2
	y := top
	if navW > 0 {
		cv.text(x, y, w, fmt.Sprintf("[%02d] %s", a.msec+1, strings.ToUpper(s.name)), a.stMuted())
		y++
	}
	cv.text(x, y, w, s.title, style{fg: a.th.ink, bg: a.th.bg, bold: true})
	y += 2
	keyW := 0
	for _, kv := range s.info {
		keyW = max(keyW, strWidth(kv[0]))
	}
	keyW = min(keyW+2, w/3)
	maxInfo := (bot - y) / 3
	for i, kv := range s.info {
		if i >= maxInfo {
			cv.text(x, y, w, fmt.Sprintf("+%d more", len(s.info)-i), a.stMuted())
			y++
			break
		}
		cv.text(x, y, keyW, clip(kv[0], keyW-1), a.stMuted())
		cv.text(x+keyW, y, w-keyW, clip(kv[1], w-keyW), a.stCopy())
		y++
	}
	if len(s.info) > 0 {
		y++
	}
	cv.hline(x, y, w, '┄', a.stRule())
	y++
	visible := bot - y
	first := 0
	if a.msel >= visible {
		first = a.msel - visible + 1
	}
	labelW := 0
	for _, ac := range s.actions {
		labelW = max(labelW, strWidth(ac.label)+4)
	}
	labelW = min(labelW, w*3/5)
	for i := first; i < len(s.actions) && y < bot; i++ {
		ac := s.actions[i]
		st := a.stBase()
		if i == a.msel {
			st = a.stSel()
			cv.fill(x-1, y, w+2, 1, st)
			cv.text(x, y, 2, "▸", style{fg: a.th.sigText, bg: st.bg, rev: st.rev})
		}
		cv.text(x+2, y, labelW-2, clip(ac.label, labelW-3), st)
		if w-labelW > 12 {
			cv.text(x+labelW, y, w-labelW, clip(ac.desc, w-labelW), style{fg: a.th.muted, bg: st.bg, rev: st.rev})
		}
		i := i
		a.onHit(x-1, y, w+2, 1, func() { a.msel = i; a.runAction(ac) })
		y++
	}
	a.drawKeybar(cv, a.h-1, []kbItem{
		{"‹ Back", "‹", "Esc", func() { a.mode = scrMain }},
		{"⌘ Commands", "⌘", "^K", a.openPalette},
		{"? Help", "?", "F1", func() { a.ov, a.help = ovHelp, 0 }},
	})
}

func (a *app) manageKey(ev event) {
	secs := a.sections()
	s := secs[min(a.msec, len(secs)-1)]
	switch {
	case ev.key == keyEsc:
		a.mode = scrMain
	case ev.key == keyUp:
		if a.msel > 0 {
			a.msel--
		}
	case ev.key == keyDown:
		if a.msel < len(s.actions)-1 {
			a.msel++
		}
	case ev.key == keyTab || ev.key == keyRight:
		a.msec, a.msel = (a.msec+1)%len(secs), 0
	case ev.key == keyBacktab || ev.key == keyLeft:
		a.msec, a.msel = (a.msec+len(secs)-1)%len(secs), 0
	case ev.key == keyEnter:
		if a.msel < len(s.actions) {
			a.runAction(s.actions[a.msel])
		}
	case ev.key == keyRune && !ev.ctrl && !ev.alt && ev.r >= '1' && ev.r <= '9':
		if n := int(ev.r - '1'); n < len(secs) {
			a.msec, a.msel = n, 0
		}
	case ev.key == keyRune && ev.r == 'q' && !ev.ctrl:
		a.mode = scrMain
	}
}

// ---- forms ---------------------------------------------------------------------------------

type field struct {
	label, hint string
	ed          editor
	choices     []string // a fixed set: ←/→ or tap to change
	choice      int
	required    bool
}

func (f *field) value() string {
	if f.choices != nil {
		return f.choices[f.choice]
	}
	return strings.TrimSpace(f.ed.String())
}

type form struct {
	title, note string
	fields      []*field
	focus       int
	build       func(v []string) string
	err         string
	stay        bool
}

func (f *form) values() []string {
	v := make([]string, len(f.fields))
	for i, fl := range f.fields {
		v[i] = fl.value()
	}
	return v
}

func (a *app) openForm(f *form) {
	a.form = f
	a.ov = ovForm
}

func text(label, hint string, required bool) *field {
	return &field{label: label, hint: hint, required: required}
}

func choice(label string, opts ...string) *field { return &field{label: label, choices: opts} }

// cmdJoin joins non-empty parts with spaces.
func cmdJoin(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(out, " ")
}

func newChannelForm() *form {
	return &form{title: "NEW CHANNEL", note: "Encryption can only be chosen now; it can't be turned on later.",
		fields: []*field{
			text("name", "lowercase letters, digits, - and _", true),
			text("topic", "optional", false),
			choice("encryption", "off", "on"),
			choice("self-destruct", "off", "10m", "2h", "24h", "7d"),
			choice("read-only", "off", "on"),
		},
		build: func(v []string) string {
			cmd := cmdJoin("/create", strings.ToLower(v[0]), v[1])
			if v[2] == "on" {
				cmd += " e2e=on"
			}
			if v[3] != "off" {
				cmd += " expire=" + v[3]
			}
			if v[4] == "on" {
				cmd += " readonly=on"
			}
			return cmd
		}}
}

func joinForm() *form {
	return &form{title: "JOIN A CHANNEL", fields: []*field{text("invite code", "e.g. bch-4F7K-92QD", true)},
		build: func(v []string) string { return cmdJoin("/join", v[0]) }}
}

func topicForm() *form {
	return &form{title: "SET TOPIC", fields: []*field{text("topic", "empty clears it", false)},
		build: func(v []string) string { return cmdJoin("/topic", v[0]) }}
}

func settingsForm() *form {
	return &form{title: "CHANNEL SETTINGS", note: "Needs admin or owner in this channel.",
		fields: []*field{
			choice("self-destruct", "keep", "off", "10m", "2h", "24h", "7d"),
			choice("read-only", "keep", "off", "on"),
			text("max file MB", "empty keeps the current limit", false),
		},
		build: func(v []string) string {
			cmd := "/settings"
			if v[0] != "keep" {
				cmd += " expire=" + v[0]
			}
			if v[1] != "keep" {
				cmd += " readonly=" + v[1]
			}
			if v[2] != "" {
				cmd += " max_file_mb=" + v[2]
			}
			return cmd
		}}
}

func leaveForm(name string) func() *form {
	return func() *form {
		return &form{title: "LEAVE #" + strings.ToUpper(name), note: "You will need a new invite to come back.",
			build: func([]string) string { return "/leave" }}
	}
}

func inviteForm() *form {
	return &form{title: "NEW INVITE", note: "The code is printed in the channel; send it to the person yourself.",
		fields: []*field{
			text("uses", "how many people can use it (default 1)", false),
			text("hours", "valid for (default 168 = 7 days)", false),
			choice("role", "member", "readonly", "mod"),
		},
		build: func(v []string) string {
			uses, hours := firstNonEmpty(v[0], "1"), firstNonEmpty(v[1], "168")
			return cmdJoin("/invite", uses, hours, v[2])
		}}
}

func sendForm() *form {
	return &form{title: "SEND A FILE", note: "Tip: dragging a file into the window types its path for you.",
		fields: []*field{text("path", "full path to the file", true)},
		build:  func(v []string) string { return cmdJoin("/send", v[0]) }}
}

func addForm() *form {
	return &form{title: "ADD SOMEONE", note: "They need an account on this hub already. Otherwise send an invite.",
		fields: []*field{text("name", "account name", true)},
		build:  func(v []string) string { return cmdJoin("/add", v[0]) }}
}

func muteForm() *form {
	return &form{title: "MUTE", fields: []*field{text("name", "", true), text("minutes", "default 10", false), text("reason", "optional, shown to them", false)},
		build: func(v []string) string { return cmdJoin("/mute", v[0], firstNonEmpty(v[1], "10"), v[2]) }}
}

func kickForm() *form {
	return &form{title: "KICK", note: "In an encrypted channel your device then rotates the key.",
		fields: []*field{text("name", "", true), text("reason", "optional", false)},
		build:  func(v []string) string { return cmdJoin("/kick", v[0], v[1]) }}
}

func banForm() *form {
	return &form{title: "BAN", note: "In an encrypted channel your device then rotates the key.",
		fields: []*field{text("name", "", true), text("days", "empty = permanent", false), text("reason", "optional", false)},
		build:  func(v []string) string { return cmdJoin("/ban", v[0], v[1], v[2]) }}
}

func roleForm() *form {
	return &form{title: "CHANGE ROLE", fields: []*field{text("name", "", true), choice("role", "member", "readonly", "mod", "admin")},
		build: func(v []string) string { return cmdJoin("/role", v[0], v[1]) }}
}

func nameForm(title, cmd string) func() *form {
	return func() *form {
		return &form{title: strings.ToUpper(title), fields: []*field{text("name", "", true)},
			build: func(v []string) string { return cmdJoin(cmd, v[0]) }}
	}
}

func (a *app) drawForm(cv *canvas) [2]int {
	f := a.form
	if f == nil {
		a.ov = ovNone
		return [2]int{-1, -1}
	}
	w := min(a.w-4, 76)
	h := 6 + len(f.fields)*2
	if f.note != "" {
		h += 2
	}
	h = min(h, a.h-2)
	x := (a.w - w) / 2
	y := max(1, (a.h-h)/2)
	a.box(cv, x, y, w, h, f.title, "esc")
	cur := [2]int{-1, -1}
	row := y + 2
	if f.note != "" {
		cv.text(x+3, row, w-6, clip(f.note, w-6), a.stMuted())
		row += 2
	}
	labelW := 16
	for i, fl := range f.fields {
		i := i
		focused := i == f.focus
		lst := a.stMuted()
		if focused {
			lst = a.stSig()
		}
		cv.text(x+3, row, labelW, fl.label, lst)
		vx := x + 3 + labelW
		vw := w - labelW - 6
		fieldSt := style{fg: a.th.ink, bg: a.th.panel}
		if a.th.panel == colDefault {
			fieldSt = style{under: true}
		}
		if fl.choices != nil {
			cx := vx
			for k, opt := range fl.choices {
				k := k
				st := a.stCopy()
				if k == fl.choice {
					st = a.stBadge()
				}
				cw := a.button(cv, cx, row, " "+opt+" ", st, func() { f.focus = i; fl.choice = k })
				cx += cw + 1
			}
		} else {
			cv.fill(vx, row, vw, 1, fieldSt)
			val := fl.ed.String()
			if val == "" && !focused {
				cv.text(vx+1, row, vw-2, clip(fl.hint, vw-2), style{fg: a.th.muted, bg: fieldSt.bg, under: fieldSt.under})
			} else {
				start := 0
				for strWidth(string(fl.ed.buf[start:fl.ed.pos])) > vw-3 && start < fl.ed.pos {
					start++
				}
				cv.text(vx+1, row, vw-2, string(fl.ed.buf[start:]), fieldSt)
				if focused {
					cur = [2]int{vx + 1 + strWidth(string(fl.ed.buf[start:fl.ed.pos])), row}
				}
				if val == "" && fl.hint != "" {
					cv.text(vx+2, row, vw-3, clip(fl.hint, vw-3), style{fg: a.th.muted, bg: fieldSt.bg, under: fieldSt.under})
				}
			}
			a.onHit(vx, row, vw, 1, func() { f.focus = i })
		}
		row += 2
	}
	cmd := f.build(f.values())
	cv.text(x+3, row, 2, ">", a.stSig())
	cv.text(x+5, row, w-8, clip(cmd, w-8), a.stCopy())
	by := y + h - 2
	if f.err != "" {
		cv.text(x+3, by, w-30, clip(f.err, w-30), style{fg: a.th.danger, bg: a.th.bg})
	}
	runLabel := " Run ↵ "
	bx := x + w - 3 - strWidth(runLabel)
	a.button(cv, bx, by, runLabel, a.stBadge(), a.submitForm)
	cw := strWidth(" Cancel ")
	a.button(cv, bx-cw-2, by, " Cancel ", a.stPanel(), a.closeOverlay)
	return cur
}

func (a *app) submitForm() {
	f := a.form
	if f == nil {
		return
	}
	for _, fl := range f.fields {
		if fl.required && fl.value() == "" {
			f.err = fl.label + " is needed"
			return
		}
	}
	cmd := f.build(f.values())
	a.closeOverlay()
	if !f.stay {
		a.mode = scrMain
	}
	a.submit(cmd)
}

func (a *app) formKey(ev event) {
	f := a.form
	if f == nil {
		a.ov = ovNone
		return
	}
	f.err = ""
	var fl *field
	if f.focus < len(f.fields) {
		fl = f.fields[f.focus]
	}
	switch {
	case ev.kind == evPaste:
		if fl != nil && fl.choices == nil {
			fl.ed.insert(strings.TrimSpace(strings.ReplaceAll(ev.text, "\n", " ")))
		}
	case ev.key == keyEsc:
		a.closeOverlay()
	case ev.key == keyTab || ev.key == keyDown:
		if f.focus < len(f.fields)-1 {
			f.focus++
		}
	case ev.key == keyBacktab || ev.key == keyUp:
		if f.focus > 0 {
			f.focus--
		}
	case ev.key == keyEnter:
		if f.focus < len(f.fields)-1 {
			f.focus++
		} else {
			a.submitForm()
		}
	case fl != nil && fl.choices != nil && (ev.key == keyRight || (ev.key == keyRune && ev.r == ' ')):
		fl.choice = (fl.choice + 1) % len(fl.choices)
	case fl != nil && fl.choices != nil && ev.key == keyLeft:
		fl.choice = (fl.choice + len(fl.choices) - 1) % len(fl.choices)
	case fl != nil && fl.choices == nil:
		fl.ed.key(ev)
	}
}
