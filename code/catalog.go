package main

import "strings"

// The command catalog drives the "/" suggestions under the input line and the
// Ctrl-K palette. Each entry is an existing slash command; nothing here adds
// behaviour, it only makes the commands findable. args is shown as a hint and,
// when non-empty, choosing the entry fills the input instead of running it.
type cmdInfo struct {
	name, args, desc, group string
}

var catalog = []cmdInfo{
	{"channels", "", "list your channels", "channels"},
	{"c", "NAME", "switch to a channel", "channels"},
	{"join", "INVITE-CODE", "join a channel with an invite code", "channels"},
	{"create", "NAME [topic] [e2e=on] [expire=2h]", "create a channel", "channels"},
	{"leave", "", "leave the current channel", "channels"},
	{"who", "", "who is online here", "channels"},
	{"members", "", "everyone in this channel", "channels"},
	{"topic", "TEXT", "set the channel topic", "channels"},

	{"send", "PATH", "send a file (or drag one in and press Enter)", "files"},
	{"get", "[#id]", "download a file (latest if no id)", "files"},
	{"open", "[#id]", "open this channel's inbox folder (or the folder a saved file is in)", "files"},
	{"watch", "[NAME]", "auto-download files from a channel even when it is not open", "files"},

	{"paste", "", "send your clipboard as a message", "clipboard"},
	{"copy", "[#id]", "copy a message to your clipboard", "clipboard"},
	{"clip", "on|off|push|pull", "clipboard sync between your own devices", "clipboard"},

	{"e2e", "status|rotate|reshare", "end-to-end encryption for this channel", "security"},
	{"verify", "NAME [FINGERPRINT]", "compare device fingerprints; with one, mark it checked", "security"},
	{"unverify", "NAME FINGERPRINT", "remove a verified mark", "security"},
	{"sessions", "[revoke ID]", "your signed-in sessions", "security"},
	{"passwd", "", "change your password", "security"},
	{"logout", "[all]", "sign out this device (or every device)", "security"},
	{"login", "NAME", "sign in as another account", "security"},

	{"invite", "[uses] [hours] [role]", "make an invite code for this channel", "moderation"},
	{"invites", "", "list open invites", "moderation"},
	{"revoke", "CODE", "cancel an invite", "moderation"},
	{"add", "NAME", "add an existing account to this channel", "moderation"},
	{"mute", "NAME [minutes] [reason]", "mute someone here", "moderation"},
	{"unmute", "NAME", "lift a mute", "moderation"},
	{"kick", "NAME [reason]", "remove someone from this channel", "moderation"},
	{"ban", "NAME [days] [reason]", "ban someone from this channel", "moderation"},
	{"unban", "NAME", "lift a ban", "moderation"},
	{"bans", "", "list bans", "moderation"},
	{"role", "NAME readonly|member|mod|admin", "change someone's role here", "moderation"},
	{"del", "#id", "delete a message", "moderation"},
	{"purge", "NAME | last N | before #id", "delete many messages (owner)", "moderation"},
	{"settings", "expire=… readonly=on|off max_file_mb=N", "channel settings", "moderation"},
	{"transfer", "NAME", "hand the channel to someone else (owner)", "moderation"},

	{"admin", "users|useradd|ban|unban|mod|audit|stats|set|prune", "server administration", "server"},

	{"manage", "", "open Manage: channels, people, invites, devices, appearance", "app"},
	{"theme", "[NAME]", "change the colour theme", "app"},
	{"mouse", "on|off", "mouse and touch input (off allows normal text selection)", "app"},
	{"notify", "on|desktop|off|test", "bell, desktop notifications, or neither", "app"},
	{"find", "TEXT", "find in this channel (Ctrl-F)", "app"},
	{"clear", "", "clear this channel's scrollback on screen", "app"},
	{"help", "", "keys and commands", "app"},
	{"quit", "", "leave bch", "app"},
}

// matchCommands returns catalog entries whose name starts with prefix (without "/").
func matchCommands(prefix string) []cmdInfo {
	prefix = strings.ToLower(prefix)
	var out []cmdInfo
	for _, c := range catalog {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// fuzzy reports whether every rune of q appears in s in order (case-insensitive),
// and a score where lower is better (earlier, tighter matches).
func fuzzy(q, s string) (bool, int) {
	q, s = strings.ToLower(q), strings.ToLower(s)
	if q == "" {
		return true, 0
	}
	if i := strings.Index(s, q); i >= 0 {
		return true, i
	}
	score, last := 100, -1
	qi := []rune(q)
	k := 0
	for i, r := range []rune(s) {
		if k < len(qi) && r == qi[k] {
			if last >= 0 {
				score += i - last
			}
			last = i
			k++
		}
	}
	return k == len(qi), score
}
