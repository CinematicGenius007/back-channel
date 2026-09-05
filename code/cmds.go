package main

// Every moderation / administration operation arrives as one frame type:
//
//	→ {"t":"cmd","rid":"c9","name":"ban","ch":"design","args":{"user":"troll","days":"30","reason":"spam"}}
//	← {"t":"res","rid":"c9","ok":true,"text":"troll banned from #design for 30d"}
//	← {"t":"res","rid":"c9","code":"perm","text":"you need mod in #design"}
//
// Every handler starts with a permission check from perms.go and nothing else decides
// access. Where an answer could reveal secret state (does that channel exist? is that a
// real user?) the failure is byte-identical for both cases.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (h *hub) handleCmd(s *session, m Msg) Msg {
	name := strings.ToLower(strings.TrimSpace(m.Name))
	args := m.Args
	if args == nil {
		args = map[string]string{}
	}
	arg := func(k string) string { return strings.TrimSpace(args[k]) }
	ok := func(text string) Msg { return Msg{T: "res", RID: m.RID, OK: true, Text: text} }
	fail := func(code, text string) Msg { return Msg{T: "res", RID: m.RID, Code: code, Text: text} }

	// Password hashing is slow (~0.3 s) and must never run under the hub lock.
	var passHash string
	switch name {
	case "useradd":
		pw := arg("pass")
		if pw == "" {
			pw = randAlnum(12)
			args["pass"] = pw
		}
		if len(pw) < 8 {
			return fail(codeBadArg, "password: at least 8 characters")
		}
		passHash = hashPassword(pw)
	case "usermod":
		if pw := arg("pass"); pw != "" {
			if len(pw) < 8 {
				return fail(codeBadArg, "password: at least 8 characters")
			}
			passHash = hashPassword(pw)
		}
	case "passwd":
		if s.guest {
			return fail(codeGuest, "guests have no password")
		}
		if len(arg("new")) < 8 {
			return fail(codeBadArg, "new password: at least 8 characters")
		}
		h.mu.Lock()
		stored := s.user.Pass
		h.mu.Unlock()
		if !checkPassword(stored, arg("old")) {
			return fail(codeAuth, "current password is wrong")
		}
		passHash = hashPassword(arg("new"))
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	S := &h.st.Settings
	now := nowMs()

	if s.guest {
		switch name {
		case "channels":
		default:
			return fail(codeGuest, "guests can't do that — ask for an account")
		}
	}

	// chCtx resolves the command's channel and checks the actor's rank for an action.
	chCtx := func(a act) (*Channel, int, *Msg) {
		ch := h.chanFor(s, m.Ch)
		if ch == nil {
			e := fail(codeNoChan, textNoChan)
			return nil, 0, &e
		}
		rank := h.rankIn(s.user, ch)
		if !can(rank, a) {
			e := fail(codePerm, "you need "+rankRole[need[a]]+" in #"+ch.Name)
			return nil, 0, &e
		}
		return ch, rank, nil
	}
	// target resolves args.user as a member of ch that the actor outranks.
	target := func(ch *Channel, rank int) (*User, int, *Msg) {
		t := h.userByName(arg("user"))
		if t == nil || (ch.Members[t.ID] == nil && srvRank(t) < srvAdmin) {
			e := fail(codeNotFound, "no such member in #"+ch.Name)
			return nil, 0, &e
		}
		tr := h.rankIn(t, ch)
		if !canTarget(rank, tr) {
			e := fail(codePerm, fmt.Sprintf("you can't act on %s (%s)", t.Name, rankRole[tr]))
			return nil, 0, &e
		}
		return t, tr, nil
	}
	atoi := func(k string, def int) int {
		if v, err := strconv.Atoi(arg(k)); err == nil {
			return v
		}
		return def
	}

	switch name {

	// ---- channels ----------------------------------------------------------------

	case "channels":
		return Msg{T: "res", RID: m.RID, OK: true, Channels: h.channelsFor(s.user)}

	case "create":
		if !canCreateChannel(s.user, S) {
			return fail(codePerm, "only server admins can create channels here (setting allow_user_channels)")
		}
		cn := strings.ToLower(arg("name"))
		if !validName(cn, 1, 32, false) {
			return fail(codeBadArg, "channel name: 1-32 chars, a-z 0-9 _ -")
		}
		if cn == "sent" {
			return fail(codeBadArg, "'sent' is reserved (outbox/sent)")
		}
		if h.chanByName(cn) != nil {
			return fail(codeExists, "#"+cn+" already exists")
		}
		if len(h.st.Channels) >= S.MaxChannels {
			return fail(codeLimit, "channel limit reached")
		}
		if v := arg("expire"); v != "" {
			if _, good := parseExpire(v); !good {
				return fail(codeBadArg, "expire: 10m, 2h, 7d, 1w or off")
			}
		}
		ch := h.newChannel(cn, s.user.ID)
		ch.Topic = arg("topic")
		ch.Readonly = arg("readonly") == "on"
		ch.ExpireSec, _ = parseExpire(arg("expire"))
		h.audit(s.user, "create", "", ch, "expire="+fmtExpire(ch.ExpireSec))
		h.save()
		return Msg{T: "res", RID: m.RID, OK: true, Text: "created #" + cn + " — you are its owner. /invite to bring people in.",
			Channels: []ChanInfo{h.chanInfoFor(s.user, ch)}}

	case "delete":
		ch, _, e := chCtx(actDelete)
		if e != nil {
			return *e
		}
		if arg("confirm") != ch.Name {
			return fail(codeBadArg, "type /delete "+ch.Name+" to confirm — this removes all its messages and files")
		}
		if ch.Default {
			return fail(codeBadArg, "can't delete the default channel (change default_channel first)")
		}
		h.deleteChannel(ch, s.user)
		return ok("deleted #" + ch.Name)

	case "topic":
		ch, _, e := chCtx(actTopic)
		if e != nil {
			return *e
		}
		t := arg("text")
		if len(t) > 200 {
			t = t[:200]
		}
		ch.Topic = t
		h.toChan(ch.ID, Msg{T: "topic", Ch: ch.Name, From: s.user.Name, Text: t})
		h.audit(s.user, "topic", "", ch, t)
		h.save()
		return ok("topic set")

	case "invite":
		ch, rank, e := chCtx(actInvite)
		if e != nil {
			return *e
		}
		uses := atoi("uses", 1)
		if arg("uses") == "inf" || uses < 0 {
			uses = -1
		}
		hours := atoi("hours", 72)
		role := arg("role")
		if role == "" {
			role = "member"
		}
		rr := roleRank[role]
		if rr == 0 || (rr >= rank && rank < rankSrvAdmin) || role == "owner" {
			return fail(codeBadArg, "role must be below your own: readonly, member, mod, admin")
		}
		signup := S.Registration != "admin"
		if v := arg("signup"); v == "off" {
			signup = false
		}
		L := h.lim()
		if !h.inviteLim.allowRate(fmt.Sprintf("inv/%d", s.user.ID), L.InviteRate/3600, L.InviteRate, 1) {
			return fail(codeRateLimit, "invite rate limit — try later")
		}
		code := randAlnum(4) + "-" + randAlnum(5)
		inv := &Invite{Code: code, ChannelID: ch.ID, CreatedBy: s.user.ID, CreatedAt: now, UsesLeft: uses, Role: role, Signup: signup}
		if hours > 0 {
			inv.ExpiresAt = now + int64(hours)*3600*1000
		}
		h.st.Invites[code] = inv
		h.audit(s.user, "invite", "", ch, fmt.Sprintf("code %s role=%s uses=%d hours=%d signup=%v", code, role, uses, hours, signup))
		h.save()
		desc := fmt.Sprintf("uses: %s · expires: %s · role: %s", plural(uses, "use", "unlimited"), expiry(hours), role)
		lines := []string{"invite code:  " + code + "   (#" + ch.Name + " · " + desc + ")"}
		if signup {
			lines = append(lines, "  new person:      "+binName+" -hub <hub address> -invite "+code+" -user THEIR_NAME")
		}
		lines = append(lines, "  existing member: /join "+code)
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "invites":
		ch, _, e := chCtx(actInvite)
		if e != nil {
			return *e
		}
		var lines []string
		for _, inv := range h.st.Invites {
			if inv.ChannelID != ch.ID {
				continue
			}
			exp := "never"
			if inv.ExpiresAt > 0 {
				exp = "in " + humanDur(time.Until(time.UnixMilli(inv.ExpiresAt)))
			}
			lines = append(lines, fmt.Sprintf("%s  role %-8s uses left %-4s expires %-8s by %s", inv.Code, inv.Role, plural(inv.UsesLeft, "", "∞"), exp, h.nameOf(inv.CreatedBy)))
		}
		if len(lines) == 0 {
			lines = []string{"no open invites for #" + ch.Name}
		}
		sort.Strings(lines)
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "revoke":
		ch, rank, e := chCtx(actInvite)
		if e != nil {
			return *e
		}
		inv := h.st.Invites[arg("code")]
		if inv == nil || inv.ChannelID != ch.ID {
			return fail(codeNotFound, "no such invite in #"+ch.Name)
		}
		if inv.CreatedBy != s.user.ID && !can(rank, actRevokeAny) {
			return fail(codePerm, "you can only revoke your own invites")
		}
		delete(h.st.Invites, inv.Code)
		h.audit(s.user, "revoke", "", ch, inv.Code)
		h.save()
		return ok("revoked " + inv.Code)

	case "join":
		inv := h.validInvite(arg("code"))
		if inv == nil {
			return fail(codeAuth, "invalid or expired invite")
		}
		ch := h.st.Channels[inv.ChannelID]
		if ch == nil || ch.Bans[s.user.ID] != nil {
			return fail(codeAuth, "invalid or expired invite")
		}
		if ch.Members[s.user.ID] != nil {
			return Msg{T: "res", RID: m.RID, OK: true, Text: "you're already in #" + ch.Name, Channels: []ChanInfo{h.chanInfoFor(s.user, ch)}}
		}
		ch.Members[s.user.ID] = &Membership{Role: inv.Role, JoinedAt: now, InvitedBy: inv.CreatedBy}
		h.consumeInvite(inv)
		h.audit(s.user, "join", s.user.Name, ch, "invite from "+h.nameOf(inv.CreatedBy))
		h.save()
		info := h.chanInfoFor(s.user, ch)
		h.toUser(s.user.ID, Msg{T: "invited", Ch: ch.Name, Channels: []ChanInfo{info}}, s)
		return Msg{T: "res", RID: m.RID, OK: true, Text: "joined #" + ch.Name, Channels: []ChanInfo{info}}

	case "leave":
		ch, _, e := chCtx(actRead)
		if e != nil {
			return *e
		}
		mem := ch.Members[s.user.ID]
		if mem == nil {
			return fail(codeBadArg, "you're not a member of #"+ch.Name+" (server admins see every channel)")
		}
		if mem.Role == "owner" {
			return fail(codeBadArg, "you own #"+ch.Name+": /transfer NAME first, or /delete it")
		}
		delete(ch.Members, s.user.ID)
		h.save()
		h.unsubUser(s.user.ID, ch.ID, Msg{T: "removed", Ch: ch.Name, Text: "you left"}, false)
		return ok("left #" + ch.Name)

	case "members":
		ch, _, e := chCtx(actMembers)
		if e != nil {
			return *e
		}
		type row struct {
			name string
			rank int
			line string
		}
		var rows []row
		for uid, mem := range ch.Members {
			u := h.st.Users[uid]
			if u == nil {
				continue
			}
			dot := "○"
			if h.presence[ch.ID][uid] > 0 {
				dot = "●"
			}
			extra := ""
			if mem.MutedUntil > now {
				extra = "  muted " + humanDur(time.Until(time.UnixMilli(mem.MutedUntil)))
			}
			if srvRank(u) >= srvAdmin {
				extra += "  (server " + u.Role + ")"
			}
			rows = append(rows, row{u.Name, roleRank[mem.Role], fmt.Sprintf("%s %-16s %-8s%s", dot, u.Name, mem.Role, extra)})
		}
		for uid := range h.presence[ch.ID] { // guests are online but never members
			if h.st.Users[uid] == nil {
				for ses := range h.sessions {
					if ses.user.ID == uid {
						rows = append(rows, row{ses.user.Name, 0, fmt.Sprintf("● %-16s guest", ses.user.Name)})
						break
					}
				}
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].rank != rows[j].rank {
				return rows[i].rank > rows[j].rank
			}
			return rows[i].name < rows[j].name
		})
		lines := []string{fmt.Sprintf("#%s — %d members, %d online", ch.Name, len(ch.Members), len(h.presence[ch.ID]))}
		for _, r := range rows {
			lines = append(lines, r.line)
		}
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "add":
		ch, _, e := chCtx(actAdd)
		if e != nil {
			return *e
		}
		t := h.userByName(arg("user"))
		if t == nil || t.Status != "active" {
			return fail(codeNotFound, "can't add that user")
		}
		if ch.Bans[t.ID] != nil {
			return fail(codePerm, t.Name+" is banned from #"+ch.Name+" — /unban first")
		}
		if ch.Members[t.ID] != nil {
			return ok(t.Name + " is already in #" + ch.Name)
		}
		ch.Members[t.ID] = &Membership{Role: "member", JoinedAt: now, InvitedBy: s.user.ID}
		h.audit(s.user, "add", t.Name, ch, "")
		h.save()
		h.toUser(t.ID, Msg{T: "invited", Ch: ch.Name, By: s.user.Name, Channels: []ChanInfo{h.chanInfoFor(t, ch)}}, nil)
		return ok("added " + t.Name + " to #" + ch.Name)

	case "kick", "ban":
		a := actKick
		if name == "ban" {
			a = actBan
		}
		ch, rank, e := chCtx(a)
		if e != nil {
			return *e
		}
		t, _, e := target(ch, rank)
		if e != nil {
			return *e
		}
		reason := arg("reason")
		ev := Msg{T: name + "ed", Ch: ch.Name, From: t.Name, By: s.user.Name, Text: reason}
		if name == "ban" {
			ev.T = "banned"
			days := atoi("days", 0)
			b := &Ban{By: s.user.ID, Reason: reason, At: now}
			if days > 0 {
				b.Until = now + int64(days)*24*3600*1000
				ev.Until = b.Until
			}
			ch.Bans[t.ID] = b
		}
		delete(ch.Members, t.ID)
		h.unsubUser(t.ID, ch.ID, ev, true) // the target hears it once, directly
		h.toChan(ch.ID, ev)                // everyone still in the room hears it
		h.audit(s.user, name, t.Name, ch, reason)
		h.save()
		return ok(fmt.Sprintf("%s %s from #%s", t.Name, ev.T, ch.Name))

	case "unban":
		ch, _, e := chCtx(actBan)
		if e != nil {
			return *e
		}
		t := h.userByName(arg("user"))
		if t == nil || ch.Bans[t.ID] == nil {
			return fail(codeNotFound, "nobody by that name is banned from #"+ch.Name)
		}
		delete(ch.Bans, t.ID)
		h.audit(s.user, "unban", t.Name, ch, "")
		h.save()
		return ok(t.Name + " unbanned from #" + ch.Name + " — /add or /invite to bring them back")

	case "bans":
		ch, _, e := chCtx(actBan)
		if e != nil {
			return *e
		}
		var lines []string
		for uid, b := range ch.Bans {
			until := "permanent"
			if b.Until > 0 {
				until = "until " + time.UnixMilli(b.Until).Format("2006-01-02")
			}
			lines = append(lines, fmt.Sprintf("%-16s %-22s by %-12s %s", h.nameOf(uid), until, h.nameOf(b.By), b.Reason))
		}
		if len(lines) == 0 {
			lines = []string{"no bans in #" + ch.Name}
		}
		sort.Strings(lines)
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "mute":
		ch, rank, e := chCtx(actMute)
		if e != nil {
			return *e
		}
		t, _, e := target(ch, rank)
		if e != nil {
			return *e
		}
		mem := ch.Members[t.ID]
		if mem == nil {
			return fail(codeNotFound, "no such member in #"+ch.Name)
		}
		mins := atoi("minutes", 10)
		if mins <= 0 {
			mem.MutedUntil = 0
			h.audit(s.user, "unmute", t.Name, ch, "")
			h.save()
			h.toChan(ch.ID, Msg{T: "muted", Ch: ch.Name, From: t.Name, By: s.user.Name, Until: 0})
			return ok(t.Name + " unmuted")
		}
		mem.MutedUntil = now + int64(mins)*60*1000
		h.audit(s.user, "mute", t.Name, ch, fmt.Sprintf("%d min %s", mins, arg("reason")))
		h.save()
		h.toChan(ch.ID, Msg{T: "muted", Ch: ch.Name, From: t.Name, By: s.user.Name, Until: mem.MutedUntil, Text: arg("reason")})
		return ok(fmt.Sprintf("%s muted for %d min", t.Name, mins))

	case "role":
		ch, rank, e := chCtx(actRole)
		if e != nil {
			return *e
		}
		t, _, e := target(ch, rank)
		if e != nil {
			return *e
		}
		mem := ch.Members[t.ID]
		if mem == nil {
			return fail(codeNotFound, "no such member in #"+ch.Name)
		}
		nr := roleRank[arg("role")]
		if nr == 0 || arg("role") == "owner" {
			return fail(codeBadArg, "role: readonly, member, mod or admin (ownership: /transfer)")
		}
		if nr >= rank && rank < rankSrvAdmin {
			return fail(codePerm, "you can only grant roles below your own")
		}
		mem.Role = arg("role")
		h.toChan(ch.ID, Msg{T: "role", Ch: ch.Name, From: t.Name, By: s.user.Name, Role: mem.Role})
		h.audit(s.user, "role", t.Name, ch, mem.Role)
		h.save()
		return ok(t.Name + " is now " + mem.Role + " in #" + ch.Name)

	case "transfer":
		ch, _, e := chCtx(actTransfer)
		if e != nil {
			return *e
		}
		t := h.userByName(arg("user"))
		if t == nil || ch.Members[t.ID] == nil {
			return fail(codeNotFound, "no such member in #"+ch.Name)
		}
		if t.ID == s.user.ID {
			return fail(codeBadArg, "you already own it")
		}
		ch.Members[t.ID].Role = "owner"
		if mine := ch.Members[s.user.ID]; mine != nil && mine.Role == "owner" {
			mine.Role = "admin"
			h.toChan(ch.ID, Msg{T: "role", Ch: ch.Name, From: s.user.Name, By: s.user.Name, Role: "admin"})
		}
		h.toChan(ch.ID, Msg{T: "role", Ch: ch.Name, From: t.Name, By: s.user.Name, Role: "owner"})
		h.audit(s.user, "transfer", t.Name, ch, "")
		h.save()
		return ok(t.Name + " now owns #" + ch.Name)

	case "del":
		ch, rank, e := chCtx(actDelOwn)
		if e != nil {
			return *e
		}
		id, _ := strconv.ParseInt(strings.TrimPrefix(arg("id"), "#"), 10, 64)
		var found *Msg
		for i := range h.msgs[ch.ID] {
			if h.msgs[ch.ID][i].ID == id {
				found = &h.msgs[ch.ID][i]
				break
			}
		}
		if found == nil || found.T == "deleted" {
			return fail(codeNotFound, "no such message in #"+ch.Name)
		}
		own := found.From == s.user.Name
		if !own && !can(rank, actDelAny) {
			return fail(codePerm, "you can only delete your own messages")
		}
		h.tombstone(ch, id, s.user.Name)
		h.toChan(ch.ID, Msg{T: "deleted", Ch: ch.Name, ID: id, By: s.user.Name})
		if !own {
			h.audit(s.user, "del", found.From, ch, fmt.Sprintf("#%d", id))
		}
		return ok(fmt.Sprintf("deleted #%d", id))

	case "purge":
		ch, _, e := chCtx(actPurge)
		if e != nil {
			return *e
		}
		var ids []int64
		ms := h.msgs[ch.ID]
		switch {
		case arg("user") != "":
			for _, x := range ms {
				if x.T != "deleted" && strings.EqualFold(x.From, arg("user")) {
					ids = append(ids, x.ID)
				}
			}
		case arg("before") != "":
			b, _ := strconv.ParseInt(strings.TrimPrefix(arg("before"), "#"), 10, 64)
			for _, x := range ms {
				if x.T != "deleted" && x.ID < b {
					ids = append(ids, x.ID)
				}
			}
		case arg("last") != "":
			n := atoi("last", 0)
			for i := len(ms) - 1; i >= 0 && n > 0; i-- {
				if ms[i].T != "deleted" {
					ids = append(ids, ms[i].ID)
					n--
				}
			}
		default:
			return fail(codeBadArg, "purge what? user=NAME | last=N | before=#ID")
		}
		for _, id := range ids {
			h.tombstone(ch, id, s.user.Name)
		}
		if len(ids) > 0 {
			h.toChan(ch.ID, Msg{T: "purged", Ch: ch.Name, IDs: ids, By: s.user.Name})
		}
		h.audit(s.user, "purge", arg("user"), ch, fmt.Sprintf("%d messages", len(ids)))
		return ok(fmt.Sprintf("purged %d messages", len(ids)))

	case "settings":
		ch, _, e := chCtx(actSettings)
		if e != nil {
			return *e
		}
		if len(args) == 0 {
			return Msg{T: "res", RID: m.RID, OK: true, Lines: []string{
				fmt.Sprintf("#%s settings — expire=%s  max_file_mb=%d  msg_rate=%g  readonly=%v   (0 = server default)",
					ch.Name, fmtExpire(ch.ExpireSec), ch.MaxFileMB, ch.MsgRate, ch.Readonly),
				"  expire: messages (and their files) self-destruct after this age — 10m, 2h, 7d, 1w, off"}}
		}
		for k, v := range args {
			switch k {
			case "expire":
				sec, good := parseExpire(v)
				if !good {
					return fail(codeBadArg, "expire: 10m, 2h, 7d, 1w or off (minimum 1m)")
				}
				ch.ExpireSec = sec
			case "retention_days":
				ch.ExpireSec = int64(atoi(k, 0)) * 86400
			case "max_file_mb":
				ch.MaxFileMB = int64(atoi(k, int(ch.MaxFileMB)))
			case "msg_rate":
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					ch.MsgRate = f
				}
			case "readonly":
				ch.Readonly = v == "on" || v == "true"
			default:
				return fail(codeBadArg, "unknown setting "+k+" (expire, max_file_mb, msg_rate, readonly)")
			}
		}
		h.audit(s.user, "settings", "", ch, fmt.Sprint(args))
		h.save()
		h.expireMessages() // a shorter expiry applies immediately
		h.toChan(ch.ID, Msg{T: "settings", Ch: ch.Name, By: s.user.Name,
			Channels: []ChanInfo{{Name: ch.Name, Topic: ch.Topic, Readonly: ch.Readonly, Expire: ch.ExpireSec}}})
		return ok("#" + ch.Name + " settings updated (expire=" + fmtExpire(ch.ExpireSec) + ")")

	// ---- account -----------------------------------------------------------------

	case "passwd":
		s.user.Pass = passHash
		h.save()
		return ok("password changed")

	case "sessions":
		type row struct {
			created int64
			line    string
		}
		var rows []row
		for k, se := range h.st.Sessions {
			if se.UserID != s.user.ID {
				continue
			}
			mark := ""
			if k == s.sessHash {
				mark = "  ← this one"
			}
			live := ""
			for ses := range h.sessions {
				if ses.sessHash == k {
					live = " online"
					break
				}
			}
			rows = append(rows, row{se.CreatedAt, fmt.Sprintf("%s  %-14s created %-6s last seen %-6s %s%s%s",
				shortSess(k), se.Device, humanDur(time.Since(time.UnixMilli(se.CreatedAt))), humanDur(time.Since(time.UnixMilli(se.LastSeen))), se.IP, live, mark)})
		}
		if rv := arg("revoke"); rv != "" {
			n := 0
			for k, se := range h.st.Sessions {
				if se.UserID == s.user.ID && strings.HasPrefix(k, rv) && k != s.sessHash {
					delete(h.st.Sessions, k)
					n++
					for ses := range h.sessions {
						if ses.sessHash == k {
							ses.push(Msg{T: "err", Code: codeAuth, Text: "session revoked from another device"})
							go func(c interface{ Close() error }) { time.Sleep(300 * time.Millisecond); c.Close() }(ses.conn)
						}
					}
				}
			}
			h.save()
			return ok(fmt.Sprintf("revoked %d session(s)", n))
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].created < rows[j].created })
		lines := []string{"your sessions (revoke with /sessions revoke ID):"}
		for _, r := range rows {
			lines = append(lines, r.line)
		}
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "logout":
		all := arg("all") == "on"
		for k, se := range h.st.Sessions {
			if se.UserID == s.user.ID && (all || k == s.sessHash) {
				delete(h.st.Sessions, k)
			}
		}
		h.save()
		if all {
			for ses := range h.sessions {
				if ses.user.ID == s.user.ID && ses != s {
					ses.push(Msg{T: "err", Code: codeAuth, Text: "logged out everywhere from another device"})
					go func(c interface{ Close() error }) { time.Sleep(300 * time.Millisecond); c.Close() }(ses.conn)
				}
			}
		}
		go func(c interface{ Close() error }) { time.Sleep(300 * time.Millisecond); c.Close() }(s.conn)
		return ok("logged out")

	// ---- server administration ---------------------------------------------------

	case "users":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		var lines []string
		for _, u := range h.st.Users {
			seen := "never"
			if u.LastSeen > 0 {
				seen = humanDur(time.Since(time.UnixMilli(u.LastSeen))) + " ago"
			}
			online := ""
			if h.sessionCount(u.ID) > 0 {
				online = " ●"
			}
			q := "∞"
			if quota := h.quotaFor(u); quota > 0 {
				q = humanSize(quota)
			}
			lines = append(lines, fmt.Sprintf("%-16s %-6s %-9s storage %s/%s  seen %s%s", u.Name, u.Role, u.Status, humanSize(h.used[u.ID]), q, seen, online))
		}
		sort.Strings(lines)
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "useradd":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		un := strings.ToLower(arg("name"))
		if !validName(un, 2, 24, true) {
			return fail(codeBadArg, "username: 2-24 chars, a-z 0-9 _ . -")
		}
		if h.userByName(un) != nil {
			return fail(codeExists, "that username is taken")
		}
		if len(h.st.Users) >= S.MaxUsers {
			return fail(codeLimit, "user limit reached")
		}
		u := h.newUserHashed(un, passHash, "user", s.user.ID)
		if def := h.defaultChan(); def != nil {
			def.Members[u.ID] = &Membership{Role: "member", JoinedAt: now, InvitedBy: s.user.ID}
		}
		h.audit(s.user, "useradd", u.Name, nil, "")
		h.save()
		return Msg{T: "res", RID: m.RID, OK: true, Lines: []string{
			fmt.Sprintf("created %s — temporary password: %s", u.Name, args["pass"]),
			"  they run:  " + binName + " -hub <hub address> -user " + u.Name + "   and then /passwd"}}

	case "userban", "userunban":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		t := h.userByName(arg("user"))
		if t == nil {
			return fail(codeNotFound, "no such user")
		}
		if !canServerTarget(s.user, t) {
			return fail(codePerm, "you can't act on "+t.Name+" ("+t.Role+")")
		}
		if name == "userunban" {
			t.Status, t.BanReason, t.BanUntil = "active", "", 0
			h.audit(s.user, "userunban", t.Name, nil, "")
			h.save()
			return ok(t.Name + " may log in again")
		}
		t.Status, t.BanReason = "banned", arg("reason")
		if d := atoi("days", 0); d > 0 {
			t.BanUntil = now + int64(d)*24*3600*1000
		} else {
			t.BanUntil = 0
		}
		h.kickSessions(t.ID, Msg{T: "err", Code: codeBanned, Text: h.userBlocked(t)})
		h.audit(s.user, "userban", t.Name, nil, fmt.Sprintf("days=%d %s", atoi("days", 0), arg("reason")))
		h.save()
		return ok(t.Name + " banned from the hub")

	case "usermod":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		t := h.userByName(arg("user"))
		if t == nil {
			return fail(codeNotFound, "no such user")
		}
		changed := []string{}
		for k, v := range args {
			switch k {
			case "user":
			case "role":
				if !isServerOwner(s.user) {
					return fail(codePerm, "only the server owner changes server roles")
				}
				if t.ID == s.user.ID {
					return fail(codeBadArg, "transfer ownership with usermod user=X role=owner")
				}
				switch v {
				case "admin", "user":
					t.Role = v
				case "owner":
					t.Role, s.user.Role = "owner", "admin"
				default:
					return fail(codeBadArg, "role: admin, user or owner")
				}
				changed = append(changed, "role="+v)
			case "pass": // admin password reset: sets it and logs the user out everywhere
				if !canServerTarget(s.user, t) {
					return fail(codePerm, "you can't change "+t.Name)
				}
				t.Pass = passHash
				for k, se := range h.st.Sessions {
					if se.UserID == t.ID {
						delete(h.st.Sessions, k)
					}
				}
				h.kickSessions(t.ID, Msg{T: "err", Code: codeAuth, Text: "your password was reset by an admin — log in again"})
				changed = append(changed, "pass=(reset)")
			case "quota_mb":
				if !canServerTarget(s.user, t) && t.ID != s.user.ID {
					return fail(codePerm, "you can't change "+t.Name)
				}
				t.QuotaMB = int64(atoi(k, 0))
				changed = append(changed, "quota_mb="+v)
			case "msg_rate", "upload_rate":
				if !canServerTarget(s.user, t) {
					return fail(codePerm, "you can't change "+t.Name)
				}
				f, _ := strconv.ParseFloat(v, 64)
				if k == "msg_rate" {
					t.MsgRate = f
				} else {
					t.UploadRate = f
				}
				changed = append(changed, k+"="+v)
			case "status":
				if !canServerTarget(s.user, t) {
					return fail(codePerm, "you can't change "+t.Name)
				}
				if v != "active" && v != "disabled" {
					return fail(codeBadArg, "status: active or disabled (bans: /admin ban)")
				}
				t.Status = v
				if v == "disabled" {
					h.kickSessions(t.ID, Msg{T: "err", Code: codeBanned, Text: "this account is disabled"})
				}
				changed = append(changed, "status="+v)
			default:
				return fail(codeBadArg, "unknown field "+k+" (role, pass, quota_mb, msg_rate, upload_rate, status)")
			}
		}
		if len(changed) == 0 {
			return fail(codeBadArg, "nothing to change")
		}
		h.audit(s.user, "usermod", t.Name, nil, strings.Join(changed, " "))
		h.save()
		return ok(t.Name + ": " + strings.Join(changed, ", "))

	case "audit":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		n := atoi("n", 30)
		var lines []string
		for i := len(h.auditLog) - 1; i >= 0 && len(lines) < n; i-- {
			r := h.auditLog[i]
			if u := arg("user"); u != "" && r.User != u && r.Actor != u {
				continue
			}
			if c := arg("ch"); c != "" && r.Ch != c {
				continue
			}
			lines = append(lines, r.String())
		}
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
		if len(lines) == 0 {
			lines = []string{"audit log is empty"}
		}
		return Msg{T: "res", RID: m.RID, OK: true, Lines: lines}

	case "stats":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		nmsg, banned := 0, 0
		for _, ms := range h.msgs {
			nmsg += len(ms)
		}
		for _, u := range h.st.Users {
			if u.Status != "active" {
				banned++
			}
		}
		orphans, orphanBytes := 0, int64(0)
		for fid, f := range h.st.Files {
			if h.fileRefCount(fid) == 0 {
				orphans++
				orphanBytes += f.Size
			}
		}
		L := h.lim()
		return Msg{T: "res", RID: m.RID, OK: true, Lines: []string{
			fmt.Sprintf("uptime %s · %d users (%d banned/disabled) · %d channels · %d connections online",
				humanDur(time.Since(h.started)), len(h.st.Users), banned, len(h.st.Channels), len(h.sessions)),
			fmt.Sprintf("%d messages in memory · %d files, %s on disk · %d unreferenced files (%s) — /admin prune",
				nmsg, len(h.st.Files), humanSize(h.totalBytes), orphans, humanSize(orphanBytes)),
			fmt.Sprintf("registration=%s guest_access=%v allow_user_channels=%v default_channel=%s",
				S.Registration, S.GuestAccess, S.AllowUserChannels, S.DefaultChannel),
			fmt.Sprintf("limits: msg_rate=%g burst=%g max_msg_kb=%d upload_rate=%g/min max_file_mb=%d quota_mb=%d invite_rate=%g/h login_rate=%g/min conn_rate=%g/min sessions_max=%d conns_per_user=%d max_uploads=%d auto_mute=%d hits→%d min",
				L.MsgRate, L.MsgBurst, L.MaxMsgKB, L.UploadRate, L.MaxFileMB, L.QuotaMB, L.InviteRate, L.LoginRate, L.ConnRate, L.SessionsMax, L.ConnsPerUser, L.MaxUploads, L.AutoMuteHits, L.AutoMuteMin),
		}}

	case "prune":
		if !canServerAct(s.user) {
			return fail(codePerm, "server admins only")
		}
		n, bytes := 0, int64(0)
		for fid, f := range h.st.Files {
			if h.fileRefCount(fid) == 0 {
				bytes += f.Size
				h.deleteFile(fid)
				n++
			}
		}
		h.audit(s.user, "prune", "", nil, fmt.Sprintf("%d files %s", n, humanSize(bytes)))
		h.save()
		return ok(fmt.Sprintf("removed %d unreferenced files (%s)", n, humanSize(bytes)))

	case "set":
		if !isServerOwner(s.user) {
			return fail(codePerm, "server owner only")
		}
		if len(args) == 0 {
			return Msg{T: "res", RID: m.RID, OK: true, Lines: h.settingsLines()}
		}
		for k, v := range args {
			if msg := h.applySetting(k, v); msg != "" {
				return fail(codeBadArg, msg)
			}
		}
		h.audit(s.user, "set", "", nil, fmt.Sprint(args))
		h.save()
		return ok("settings updated")
	}
	return fail(codeBadArg, "unknown command "+name)
}

// tombstone soft-deletes one message: the body is replaced in memory, a `del` record is
// appended to the log, and any file it referenced loses one reference. Caller holds h.mu.
func (h *hub) tombstone(ch *Channel, id int64, by string) {
	orig, changed := h.tombstoneInMem(ch.ID, id, by)
	if !changed {
		return
	}
	h.appendLog(Msg{T: "del", CID: ch.ID, ID: id, By: by})
	if orig.T == "file" && orig.FID != "" {
		h.unrefFile(orig.FID, ch.ID)
	}
}

func (h *hub) deleteChannel(ch *Channel, by *User) {
	for _, x := range h.msgs[ch.ID] {
		if x.T == "file" && x.FID != "" {
			h.unrefFile(x.FID, ch.ID)
		}
	}
	delete(h.msgs, ch.ID)
	for code, inv := range h.st.Invites {
		if inv.ChannelID == ch.ID {
			delete(h.st.Invites, code)
		}
	}
	ev := Msg{T: "removed", Ch: ch.Name, By: by.Name, Text: "channel deleted"}
	for s := range h.sessions {
		if s.subs[ch.ID] {
			delete(s.subs, ch.ID)
			s.push(ev)
		}
	}
	delete(h.presence, ch.ID)
	delete(h.st.Channels, ch.ID)
	delete(h.chByName, ch.Name)
	h.audit(by, "delete", "", ch, fmt.Sprintf("%d members", len(ch.Members)))
	h.save()
	h.compactLog()
}

func (h *hub) settingsLines() []string {
	S := h.st.Settings
	L := S.Limits
	return []string{
		"server settings (/admin set KEY VALUE):",
		fmt.Sprintf("  registration=%s   allow_user_channels=%v   default_channel=%s   guest_access=%v   legacy_token=%s", S.Registration, S.AllowUserChannels, S.DefaultChannel, S.GuestAccess, S.LegacyToken),
		fmt.Sprintf("  max_users=%d   max_channels=%d", S.MaxUsers, S.MaxChannels),
		fmt.Sprintf("  msg_rate=%g   msg_burst=%g   max_msg_kb=%d   upload_rate=%g   max_file_mb=%d   quota_mb=%d", L.MsgRate, L.MsgBurst, L.MaxMsgKB, L.UploadRate, L.MaxFileMB, L.QuotaMB),
		fmt.Sprintf("  invite_rate=%g   login_rate=%g   conn_rate=%g   sessions_max=%d   conns_per_user=%d   max_uploads=%d", L.InviteRate, L.LoginRate, L.ConnRate, L.SessionsMax, L.ConnsPerUser, L.MaxUploads),
		fmt.Sprintf("  auto_mute_hits=%d   auto_mute_min=%d", L.AutoMuteHits, L.AutoMuteMin),
	}
}

// applySetting sets one server setting from a string; returns an error message or "".
func (h *hub) applySetting(k, v string) string {
	S := &h.st.Settings
	L := &S.Limits
	onoff := func() (bool, bool) {
		switch v {
		case "on", "true", "yes":
			return true, true
		case "off", "false", "no":
			return false, true
		}
		return false, false
	}
	num := func(dst *int) string {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return k + " must be a non-negative integer"
		}
		*dst = n
		return ""
	}
	num64 := func(dst *int64) string {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return k + " must be a non-negative integer"
		}
		*dst = n
		return ""
	}
	flt := func(dst *float64) string {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return k + " must be a non-negative number"
		}
		*dst = f
		return ""
	}
	switch k {
	case "registration":
		if v != "invite" && v != "admin" && v != "open" {
			return "registration: invite, admin or open"
		}
		S.Registration = v
	case "allow_user_channels", "guest_access":
		b, ok := onoff()
		if !ok {
			return k + ": on or off"
		}
		if k == "allow_user_channels" {
			S.AllowUserChannels = b
		} else {
			S.GuestAccess = b
			if b && S.LegacyToken == "" {
				S.LegacyToken = randHex(6)
			}
		}
	case "legacy_token":
		if v == "new" {
			v = randHex(6)
		}
		S.LegacyToken = v
	case "default_channel":
		ch := h.chanByName(v)
		if ch == nil {
			return "no such channel"
		}
		for _, c := range h.st.Channels {
			c.Default = false
		}
		ch.Default = true
		S.DefaultChannel = ch.Name
	case "max_users":
		return num(&S.MaxUsers)
	case "max_channels":
		return num(&S.MaxChannels)
	case "msg_rate":
		return flt(&L.MsgRate)
	case "msg_burst":
		return flt(&L.MsgBurst)
	case "max_msg_kb":
		return num(&L.MaxMsgKB)
	case "upload_rate":
		return flt(&L.UploadRate)
	case "max_file_mb":
		return num64(&L.MaxFileMB)
	case "quota_mb":
		return num64(&L.QuotaMB)
	case "invite_rate":
		return flt(&L.InviteRate)
	case "login_rate":
		return flt(&L.LoginRate)
	case "conn_rate":
		return flt(&L.ConnRate)
	case "sessions_max":
		return num(&L.SessionsMax)
	case "conns_per_user":
		return num(&L.ConnsPerUser)
	case "max_uploads":
		return num(&L.MaxUploads)
	case "auto_mute_hits":
		return num(&L.AutoMuteHits)
	case "auto_mute_min":
		return num(&L.AutoMuteMin)
	default:
		return "unknown setting " + k
	}
	return ""
}

func plural(n int, unit, inf string) string {
	if n < 0 {
		return inf
	}
	if unit == "" {
		return strconv.Itoa(n)
	}
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func expiry(hours int) string {
	if hours <= 0 {
		return "never"
	}
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dh", hours)
}
