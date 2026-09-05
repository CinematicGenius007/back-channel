package main

// Persistence. Everything the hub knows about people and rooms lives in one JSON file
// (state.json, rewritten atomically), messages in an append-only messages.jsonl, and
// privileged actions in an append-only audit.jsonl. No database: it keeps the binary
// stdlib-only and cross-compiles in one command. At the intended scale (tens to a few
// hundred users) the whole state is a few hundred KB and fits in memory.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type User struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`   // 2-24 chars, [a-z0-9_.-], unique (case-insensitive)
	Pass       string  `json:"pass"`   // pbkdf2 hash string, see auth.go
	Role       string  `json:"role"`   // owner | admin | user   (guests are never stored)
	Status     string  `json:"status"` // active | banned | disabled
	BanReason  string  `json:"ban_reason,omitempty"`
	BanUntil   int64   `json:"ban_until,omitempty"` // 0 = permanent (while status=banned)
	CreatedAt  int64   `json:"created_at"`
	CreatedBy  int64   `json:"created_by,omitempty"`
	LastSeen   int64   `json:"last_seen,omitempty"`
	QuotaMB    int64   `json:"quota_mb,omitempty"`    // 0 = server default
	MsgRate    float64 `json:"msg_rate,omitempty"`    // 0 = channel/server default
	UploadRate float64 `json:"upload_rate,omitempty"` // 0 = server default
}

type Session struct {
	UserID    int64  `json:"uid"`
	Device    string `json:"device,omitempty"`
	IP        string `json:"ip,omitempty"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
}

type Membership struct {
	Role       string `json:"role"` // owner | admin | mod | member | readonly
	JoinedAt   int64  `json:"joined_at"`
	InvitedBy  int64  `json:"invited_by,omitempty"`
	MutedUntil int64  `json:"muted_until,omitempty"`
	LastRead   int64  `json:"last_read,omitempty"` // message id, synced across the user's devices
}

type Ban struct {
	By     int64  `json:"by"`
	Reason string `json:"reason,omitempty"`
	At     int64  `json:"at"`
	Until  int64  `json:"until,omitempty"` // 0 = permanent
}

type Channel struct {
	ID            int64                 `json:"id"`
	Name          string                `json:"name"` // 1-32 chars, [a-z0-9_-]
	Topic         string                `json:"topic,omitempty"`
	CreatedAt     int64                 `json:"created_at"`
	CreatedBy     int64                 `json:"created_by,omitempty"`
	RetentionDays int                   `json:"retention_days,omitempty"` // legacy; converted to ExpireSec on load
	ExpireSec     int64                 `json:"expire_sec,omitempty"`     // self-destruct: messages older than this vanish; 0 = keep forever
	MaxFileMB     int64                 `json:"max_file_mb,omitempty"`    // 0 = server default
	MsgRate       float64               `json:"msg_rate,omitempty"`       // 0 = server default
	Readonly      bool                  `json:"readonly,omitempty"`       // only mod+ may post (announcements)
	Default       bool                  `json:"default,omitempty"`        // guests / new users land here
	Members       map[int64]*Membership `json:"members"`
	Bans          map[int64]*Ban        `json:"bans,omitempty"`
}

type Invite struct {
	Code      string `json:"code"`
	ChannelID int64  `json:"cid"`
	CreatedBy int64  `json:"by"`
	CreatedAt int64  `json:"at"`
	UsesLeft  int    `json:"uses"`             // -1 = unlimited
	ExpiresAt int64  `json:"exp,omitempty"`    // 0 = never
	Role      string `json:"role"`             // role granted on redeem
	Signup    bool   `json:"signup,omitempty"` // may create a new account
}

type FileMeta struct {
	FID       string `json:"fid"`
	Sha       string `json:"sha256"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Owner     int64  `json:"owner,omitempty"` // first uploader; charged for quota
	CreatedAt int64  `json:"at"`
}

// Limits are the server-wide defaults. Channels and users may override some of them;
// the most specific non-zero value wins (see limits.go).
type Limits struct {
	MsgRate      float64 `json:"msg_rate"`       // messages/second per (user, channel); mods get 3x
	MsgBurst     float64 `json:"msg_burst"`      // bucket size
	MaxMsgKB     int     `json:"max_msg_kb"`     // text message / clipboard size
	UploadRate   float64 `json:"upload_rate"`    // uploads/minute per user
	MaxFileMB    int64   `json:"max_file_mb"`    // per upload
	QuotaMB      int64   `json:"quota_mb"`       // per user, unique bytes; 0 = unlimited
	InviteRate   float64 `json:"invite_rate"`    // invites/hour per user
	LoginRate    float64 `json:"login_rate"`     // failed logins/minute per username
	ConnRate     float64 `json:"conn_rate"`      // new connections/minute per IP
	SessionsMax  int     `json:"sessions_max"`   // saved sessions per user (oldest evicted)
	ConnsPerUser int     `json:"conns_per_user"` // simultaneous connections per user
	MaxUploads   int     `json:"max_uploads"`    // concurrent uploads per user
	AutoMuteHits int     `json:"auto_mute_hits"` // rate-limit hits in 5 min before auto-mute
	AutoMuteMin  int     `json:"auto_mute_min"`  // auto-mute duration
}

type Settings struct {
	Registration      string `json:"registration"`        // invite | admin | open
	AllowUserChannels bool   `json:"allow_user_channels"` // may ordinary users /create?
	DefaultChannel    string `json:"default_channel"`     // where guests and new users land
	GuestAccess       bool   `json:"guest_access"`        // accept v1 clients / -token with LegacyToken
	LegacyToken       string `json:"legacy_token,omitempty"`
	MaxUsers          int    `json:"max_users"`
	MaxChannels       int    `json:"max_channels"`
	Limits            Limits `json:"limits"`
}

type State struct {
	Version  int                  `json:"version"`
	NextUser int64                `json:"next_user"`
	NextChan int64                `json:"next_chan"`
	Users    map[int64]*User      `json:"users"`
	Channels map[int64]*Channel   `json:"channels"`
	Sessions map[string]*Session  `json:"sessions"` // key: sha256(token) hex
	Invites  map[string]*Invite   `json:"invites"`
	Files    map[string]*FileMeta `json:"files"`
	Settings Settings             `json:"settings"`
}

func defaultLimits() Limits {
	return Limits{
		MsgRate: 5, MsgBurst: 20, MaxMsgKB: 64,
		UploadRate: 30, MaxFileMB: 2048, QuotaMB: 5120,
		InviteRate: 10, LoginRate: 5, ConnRate: 20,
		SessionsMax: 10, ConnsPerUser: 5, MaxUploads: 2,
		AutoMuteHits: 20, AutoMuteMin: 10,
	}
}

func newState() *State {
	return &State{
		Version: 2, NextUser: 1, NextChan: 1,
		Users: map[int64]*User{}, Channels: map[int64]*Channel{}, Sessions: map[string]*Session{},
		Invites: map[string]*Invite{}, Files: map[string]*FileMeta{},
		Settings: Settings{Registration: "invite", DefaultChannel: "general", MaxUsers: 500, MaxChannels: 200, Limits: defaultLimits()},
	}
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

func loadState(dir string) (*State, error) {
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		return nil, err
	}
	st := newState()
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("state.json is corrupt: %w", err)
	}
	for _, ch := range st.Channels {
		if ch.Members == nil {
			ch.Members = map[int64]*Membership{}
		}
		if ch.Bans == nil {
			ch.Bans = map[int64]*Ban{}
		}
		if ch.RetentionDays > 0 && ch.ExpireSec == 0 {
			ch.ExpireSec = int64(ch.RetentionDays) * 86400
			ch.RetentionDays = 0
		}
	}
	return st, nil
}

// saveState writes state.json atomically (tmp + rename) so a crash mid-write can never
// leave a truncated file.
func saveState(dir string, st *State) error {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir))
}

// ---- hub-side wrappers ------------------------------------------------------

// save persists state now. Call after anything you would hate to lose (a ban, a new
// user, a session). Cheap edits (last_read, last_seen) use markDirty and the flusher.
func (h *hub) save() {
	if err := saveState(h.dir, h.st); err != nil {
		fmt.Fprintln(os.Stderr, "state save failed:", err)
	}
	h.dirty = false
}

func (h *hub) markDirty() { h.dirty = true }

func (h *hub) flusher() {
	for {
		time.Sleep(2 * time.Second)
		h.mu.Lock()
		if h.dirty {
			h.save()
		}
		h.mu.Unlock()
	}
}

func (h *hub) reindex() {
	h.byName = map[string]*User{}
	for _, u := range h.st.Users {
		h.byName[strings.ToLower(u.Name)] = u
	}
	h.chByName = map[string]*Channel{}
	for _, c := range h.st.Channels {
		h.chByName[strings.ToLower(c.Name)] = c
	}
}

func (h *hub) userByName(n string) *User    { return h.byName[strings.ToLower(strings.TrimSpace(n))] }
func (h *hub) chanByName(n string) *Channel { return h.chByName[strings.ToLower(strings.TrimSpace(n))] }
func (h *hub) defaultChan() *Channel        { return h.chanByName(h.st.Settings.DefaultChannel) }

func (h *hub) nameOf(id int64) string {
	if u := h.st.Users[id]; u != nil {
		return u.Name
	}
	if id == 0 {
		return "system"
	}
	return fmt.Sprintf("user#%d", id)
}

// ---- bootstrap / migration --------------------------------------------------

// loadOrInit loads state.json or creates a fresh hub. On a fresh hub it creates the
// owner account (password printed once) and the default channel, and imports a v1
// data directory (history.jsonl + token + files/) if one is found in the same dir.
// Returns whether this was the first start and the owner's initial password.
func (h *hub) loadOrInit() (first bool, ownerPass string) {
	st, err := loadState(h.dir)
	if err == nil {
		h.st = st
		h.reindex()
		return false, ""
	}
	if !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	h.st = newState()
	h.reindex()
	ownerPass = randAlnum(14)
	owner := h.newUser("owner", ownerPass, "owner", 0)
	def := h.newChannel(h.st.Settings.DefaultChannel, owner.ID)
	def.Default = true
	def.Topic = "everyone lands here"

	// v1 leftovers in this directory?
	if b, err := os.ReadFile(filepath.Join(h.dir, "token")); err == nil {
		h.st.Settings.LegacyToken = strings.TrimSpace(string(b))
		h.st.Settings.GuestAccess = true
		h.migrated = append(h.migrated, "guest access ON with the v1 token, so old clients keep working in #"+def.Name)
	}
	if old := filepath.Join(h.dir, "history.jsonl"); fileExists(old) && !fileExists(h.logPath()) {
		n := h.importV1History(old, def)
		os.Rename(old, old+".v1.bak")
		h.migrated = append(h.migrated, fmt.Sprintf("imported %d v1 messages into #%s (original kept as history.jsonl.v1.bak)", n, def.Name))
	}
	h.save()
	return true, ownerPass
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func (h *hub) newUser(name, pass, role string, by int64) *User {
	u := &User{ID: h.st.NextUser, Name: strings.ToLower(name), Pass: hashPassword(pass), Role: role,
		Status: "active", CreatedAt: nowMs(), CreatedBy: by}
	h.st.NextUser++
	h.st.Users[u.ID] = u
	h.byName[u.Name] = u
	return u
}

func (h *hub) newChannel(name string, by int64) *Channel {
	c := &Channel{ID: h.st.NextChan, Name: strings.ToLower(name), CreatedAt: nowMs(), CreatedBy: by,
		Members: map[int64]*Membership{}, Bans: map[int64]*Ban{}}
	h.st.NextChan++
	h.st.Channels[c.ID] = c
	h.chByName[c.Name] = c
	if by != 0 {
		c.Members[by] = &Membership{Role: "owner", JoinedAt: nowMs()}
	}
	return c
}

func (h *hub) importV1History(path string, def *Channel) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	out, err := os.OpenFile(h.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0
	}
	defer out.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	n := 0
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == 0 || (m.T != "msg" && m.T != "file") {
			continue
		}
		m.Ch, m.CID = def.Name, def.ID
		b, _ := json.Marshal(m)
		out.Write(append(b, '\n'))
		n++
	}
	return n
}

// ---- message log ------------------------------------------------------------

func (h *hub) logPath() string { return filepath.Join(h.dir, "messages.jsonl") }

// loadMessages reads messages.jsonl into per-channel slices (last `keep` per channel),
// applies tombstones, and compacts the file if it has grown well past what is kept.
func (h *hub) loadMessages() {
	h.msgs = map[int64][]Msg{}
	f, err := os.Open(h.logPath())
	lines := 0
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			lines++
			var m Msg
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == 0 {
				continue
			}
			if m.ID > h.nextID {
				h.nextID = m.ID
			}
			if h.st.Channels[m.CID] == nil {
				continue // channel was deleted; dropped on compaction
			}
			switch m.T {
			case "msg", "file", "deleted":
				h.msgs[m.CID] = append(h.msgs[m.CID], m)
			case "del":
				h.tombstoneInMem(m.CID, m.ID, m.By)
			}
		}
		f.Close()
	}
	kept := 0
	for cid, ms := range h.msgs {
		if len(ms) > h.keep {
			ms = ms[len(ms)-h.keep:]
			h.msgs[cid] = ms
		}
		kept += len(ms)
	}
	if lines > kept+kept/2+200 {
		h.compactLog()
	}
	lf, err := os.OpenFile(h.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open message log:", err)
		os.Exit(1)
	}
	h.logf = lf
}

func (h *hub) appendLog(m Msg) {
	if h.logf == nil {
		return
	}
	b, _ := json.Marshal(m)
	h.logf.Write(append(b, '\n'))
}

// compactLog rewrites messages.jsonl from memory (only what is still served).
func (h *hub) compactLog() {
	var all []Msg
	for _, ms := range h.msgs {
		all = append(all, ms...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	tmp := h.logPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, m := range all {
		b, _ := json.Marshal(m)
		w.Write(append(b, '\n'))
	}
	w.Flush()
	f.Close()
	if h.logf != nil {
		h.logf.Close()
		h.logf = nil
	}
	os.Rename(tmp, h.logPath())
	if lf, err := os.OpenFile(h.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		h.logf = lf
	}
}

// tombstoneInMem turns a message into a tombstone in memory; returns the original and
// whether anything changed.
func (h *hub) tombstoneInMem(cid, id int64, by string) (Msg, bool) {
	ms := h.msgs[cid]
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].ID == id {
			orig := ms[i]
			if orig.T == "deleted" {
				return orig, false
			}
			ms[i] = Msg{T: "deleted", Ch: orig.Ch, CID: cid, ID: id, TS: orig.TS, From: orig.From, By: by}
			return orig, true
		}
	}
	return Msg{}, false
}

// lastID returns the newest message id in a channel (0 if none).
func (h *hub) lastID(cid int64) int64 {
	ms := h.msgs[cid]
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].ID
}

// unreadCount counts live messages in memory newer than `since`.
func (h *hub) unreadCount(cid, since int64) int {
	ms := h.msgs[cid]
	i := sort.Search(len(ms), func(i int) bool { return ms[i].ID > since })
	n := 0
	for ; i < len(ms); i++ {
		if ms[i].T != "deleted" {
			n++
		}
	}
	return n
}

// ---- audit log --------------------------------------------------------------

type auditRec struct {
	TS     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	User   string `json:"user,omitempty"`
	Ch     string `json:"ch,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (h *hub) audit(actor *User, action, targetUser string, ch *Channel, detail string) {
	r := auditRec{TS: nowMs(), Action: action, User: targetUser, Detail: detail, Actor: "system"}
	if actor != nil {
		r.Actor = actor.Name
	}
	if ch != nil {
		r.Ch = ch.Name
	}
	h.auditLog = append(h.auditLog, r)
	if len(h.auditLog) > 2000 {
		h.auditLog = h.auditLog[len(h.auditLog)-2000:]
	}
	f, err := os.OpenFile(filepath.Join(h.dir, "audit.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	b, _ := json.Marshal(r)
	f.Write(append(b, '\n'))
	f.Close()
}

func (h *hub) loadAudit() {
	f, err := os.Open(filepath.Join(h.dir, "audit.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r auditRec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			h.auditLog = append(h.auditLog, r)
		}
	}
	if len(h.auditLog) > 2000 {
		h.auditLog = h.auditLog[len(h.auditLog)-2000:]
	}
}

func (r auditRec) String() string {
	t := time.UnixMilli(r.TS).Format("2006-01-02 15:04")
	s := fmt.Sprintf("%s  %-12s %-10s", t, r.Actor, r.Action)
	if r.Ch != "" {
		s += " #" + r.Ch
	}
	if r.User != "" {
		s += " " + r.User
	}
	if r.Detail != "" {
		s += "  " + r.Detail
	}
	return s
}
