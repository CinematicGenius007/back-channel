package main

import "testing"

// Every cell of PHASE2-DESIGN.md §4. If you add an action, add its row here.
func TestPermissionMatrix(t *testing.T) {
	ranks := []int{rankReadonly, rankMember, rankMod, rankAdmin, rankOwner, rankSrvAdmin, rankSrvOwner}
	names := map[int]string{rankReadonly: "readonly", rankMember: "member", rankMod: "mod", rankAdmin: "admin", rankOwner: "owner", rankSrvAdmin: "srvadmin", rankSrvOwner: "srvowner"}
	//                                readonly member mod admin owner srvadmin srvowner
	table := map[act][]bool{
		actRead:      {true, true, true, true, true, true, true},
		actMembers:   {true, true, true, true, true, true, true},
		actSend:      {false, true, true, true, true, true, true},
		actDelOwn:    {false, true, true, true, true, true, true},
		actTopic:     {false, false, true, true, true, true, true},
		actInvite:    {false, false, true, true, true, true, true},
		actAdd:       {false, false, true, true, true, true, true},
		actKick:      {false, false, true, true, true, true, true},
		actBan:       {false, false, true, true, true, true, true},
		actMute:      {false, false, true, true, true, true, true},
		actDelAny:    {false, false, true, true, true, true, true},
		actRevokeAny: {false, false, false, true, true, true, true},
		actRole:      {false, false, false, true, true, true, true},
		actSettings:  {false, false, false, true, true, true, true},
		actPurge:     {false, false, false, false, true, true, true},
		actDelete:    {false, false, false, false, true, true, true},
		actTransfer:  {false, false, false, false, true, true, true},
	}
	for a, row := range table {
		for i, r := range ranks {
			if got := can(r, a); got != row[i] {
				t.Errorf("can(%s, act %d) = %v, want %v", names[r], a, got, row[i])
			}
		}
	}
	if can(rankNone, actRead) {
		t.Error("non-members must not read")
	}
}

func TestCanTarget(t *testing.T) {
	cases := []struct {
		actor, target int
		want          bool
	}{
		{rankMod, rankMember, true},
		{rankMod, rankMod, false}, // no mod-vs-mod wars
		{rankAdmin, rankMod, true},
		{rankOwner, rankAdmin, true},
		{rankMod, rankOwner, false},
		{rankOwner, rankSrvAdmin, false}, // channel staff can't touch hub staff
		{rankSrvAdmin, rankSrvAdmin, false},
		{rankSrvOwner, rankSrvAdmin, true},
		{rankSrvAdmin, rankOwner, true},
		{rankSrvOwner, rankSrvOwner, false},
	}
	for _, c := range cases {
		if got := canTarget(c.actor, c.target); got != c.want {
			t.Errorf("canTarget(%d,%d)=%v want %v", c.actor, c.target, got, c.want)
		}
	}
}

func TestCanSendReadonlyChannel(t *testing.T) {
	ro := &Channel{Readonly: true}
	if canSend(rankMember, ro) {
		t.Error("members must not post in a read-only channel")
	}
	if !canSend(rankMod, ro) {
		t.Error("mods must post in a read-only channel")
	}
	if canSend(rankReadonly, &Channel{}) {
		t.Error("readonly role must not post")
	}
}

func TestServerRanks(t *testing.T) {
	owner := &User{ID: 1, Role: "owner"}
	admin := &User{ID: 2, Role: "admin"}
	user := &User{ID: 3, Role: "user"}
	guest := &User{ID: -4, Role: "guest"}
	s := &Settings{}
	if !canCreateChannel(admin, s) || canCreateChannel(user, s) || canCreateChannel(guest, s) {
		t.Error("channel creation defaults wrong")
	}
	s.AllowUserChannels = true
	if !canCreateChannel(user, s) || canCreateChannel(guest, s) {
		t.Error("allow_user_channels wrong")
	}
	if !canServerTarget(owner, admin) || canServerTarget(admin, owner) || canServerTarget(admin, admin) || !canServerTarget(admin, user) {
		t.Error("server target rule wrong")
	}
	h := &hub{}
	ch := &Channel{Members: map[int64]*Membership{}}
	if h.rankIn(owner, ch) != rankSrvOwner || h.rankIn(admin, ch) != rankSrvAdmin || h.rankIn(user, ch) != rankNone {
		t.Error("rankIn server roles wrong")
	}
	def := &Channel{Default: true, Members: map[int64]*Membership{}}
	if h.rankIn(guest, def) != rankMember || h.rankIn(guest, ch) != rankNone {
		t.Error("guest rank wrong")
	}
}
