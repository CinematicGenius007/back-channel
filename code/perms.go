package main

// The permission matrix from PHASE2-DESIGN.md §4, as code. Every handler calls
// can()/canTarget() at the top and nothing else decides access. perms_test.go checks
// every cell of the matrix, so a change here without a test change is a red flag.
//
// Ranks inside a channel, low → high:
//
//	0 none      not a member (and the channel must look non-existent to you)
//	1 readonly  may read
//	2 member    may post
//	3 mod       kick/ban/mute lower ranks, topic, invites, delete any message
//	4 admin     + promote/demote below own rank, channel settings
//	5 owner     + purge, delete channel, transfer ownership
//	6 server admin   acts as above-owner in every channel; untouchable by channel staff
//	7 server owner   the one account nobody can ban; can act on server admins
//
// Server-level rank: 0 guest, 1 user, 2 admin, 3 owner.

const (
	rankNone     = 0
	rankReadonly = 1
	rankMember   = 2
	rankMod      = 3
	rankAdmin    = 4
	rankOwner    = 5
	rankSrvAdmin = 6
	rankSrvOwner = 7
)

var roleRank = map[string]int{"readonly": rankReadonly, "member": rankMember, "mod": rankMod, "admin": rankAdmin, "owner": rankOwner}
var rankRole = map[int]string{rankReadonly: "readonly", rankMember: "member", rankMod: "mod", rankAdmin: "admin", rankOwner: "owner", rankSrvAdmin: "server admin", rankSrvOwner: "server owner"}

type act int

const (
	actRead      act = iota // read history, see the channel exists
	actMembers              // list members
	actSend                 // post text / files
	actDelOwn               // delete own message
	actTopic                // set topic
	actInvite               // create invite codes
	actAdd                  // add an existing account directly
	actKick                 // (target rank must be lower)
	actBan                  // (target rank must be lower)
	actMute                 // (target rank must be lower)
	actDelAny               // delete anyone's message
	actRevokeAny            // revoke invites made by others
	actRole                 // change roles (target and new role below own rank)
	actSettings             // retention / max file / msg rate / readonly
	actPurge                // bulk delete
	actDelete               // delete the channel
	actTransfer             // hand ownership to someone else
)

// need is the minimum channel rank per action.
var need = map[act]int{
	actRead: rankReadonly, actMembers: rankReadonly,
	actSend: rankMember, actDelOwn: rankMember,
	actTopic: rankMod, actInvite: rankMod, actAdd: rankMod, actKick: rankMod, actBan: rankMod, actMute: rankMod, actDelAny: rankMod,
	actRevokeAny: rankAdmin, actRole: rankAdmin, actSettings: rankAdmin,
	actPurge: rankOwner, actDelete: rankOwner, actTransfer: rankOwner,
}

// can says whether a channel rank may perform an action.
func can(rank int, a act) bool { return rank >= need[a] }

// canTarget implements the "≤" rule: you may act on someone only if you outrank them,
// and nobody but the server owner may act on a server admin. Prevents mod-vs-mod wars
// and channel staff ejecting the people who run the hub.
func canTarget(actor, target int) bool {
	if target >= rankSrvAdmin && actor != rankSrvOwner {
		return false
	}
	return actor > target
}

// canSend folds the channel's read-only flag in: in an announcements channel only
// mods and above may post, whatever their membership role says.
func canSend(rank int, ch *Channel) bool {
	if !can(rank, actSend) {
		return false
	}
	if ch.Readonly && rank < rankMod {
		return false
	}
	return true
}

// ---- server level -----------------------------------------------------------

const (
	srvGuest = 0
	srvUser  = 1
	srvAdmin = 2
	srvOwner = 3
)

func srvRank(u *User) int {
	switch u.Role {
	case "owner":
		return srvOwner
	case "admin":
		return srvAdmin
	case "guest":
		return srvGuest
	}
	return srvUser
}

// rankIn is a user's effective rank inside a channel.
func (h *hub) rankIn(u *User, ch *Channel) int {
	switch srvRank(u) {
	case srvOwner:
		return rankSrvOwner
	case srvAdmin:
		return rankSrvAdmin
	case srvGuest:
		if ch.Default {
			return rankMember
		}
		return rankNone
	}
	m := ch.Members[u.ID]
	if m == nil {
		return rankNone
	}
	return roleRank[m.Role]
}

func canCreateChannel(u *User, s *Settings) bool {
	r := srvRank(u)
	return r >= srvAdmin || (r == srvUser && s.AllowUserChannels)
}

// canServerAct covers admin-only server commands (useradd, userban, audit, stats).
func canServerAct(u *User) bool { return srvRank(u) >= srvAdmin }

// canServerTarget: server admins act only on lower server ranks; the owner on anyone but itself.
func canServerTarget(actor, target *User) bool {
	if actor.ID == target.ID {
		return false
	}
	return srvRank(actor) > srvRank(target)
}

func isServerOwner(u *User) bool { return srvRank(u) == srvOwner }
