# backchannel — Admin Guide

How to run a hub for other people: accounts, channels, roles, invites, moderation, limits,
self-destruct channels, end-to-end encryption, the audit log, and the operations you will
actually need. Every command below is typed inside the `bch` TUI unless it starts with
`bch serve`.

---

## 1. The model in two minutes

```
hub ─┬─ users        owner (exactly one) · admins · users · (guests: no account)
     ├─ channels     each with members and per-channel roles: owner › admin › mod › member › readonly
     ├─ invites      codes that let someone in (and optionally create their account)
     ├─ files        stored once by content hash, reference-counted by messages
     ├─ settings     registration mode, guest access, default channel, limits
     └─ audit log    who did what, append-only
```

- **A channel is invisible unless you are in it.** There is no channel directory. Asking
  about a channel you're not in gives exactly the same answer as asking about one that
  doesn't exist. Invite codes are the only way in (or a mod adding you by name).
- **Server admins outrank everyone inside every channel.** Channel staff cannot kick,
  ban or mute them. Only the server owner can act on a server admin.
- **The server owner cannot be banned, demoted or locked out** except by itself.
  `bch serve -reset-owner` prints a new owner password if it is lost.
- **Limits are per account**, not per IP, so an office behind one NAT doesn't share a
  bucket and an attacker with many IPs doesn't get many buckets.

## 2. First start

```sh
bch serve                    # LAN / Tailscale
bch serve -public            # internet-facing: TLS + pinning + stricter limits (PUBLIC-SERVER.md)
```
The first start creates the `owner` account and prints its password **once**, plus the
default channel `#general`. Log in from a client with `bch -hub HOST -user owner`, then:

```
/passwd                       change the owner password
/admin set                    look at the server settings
/create ops the on-call room  make a channel (you are its owner)
/invite                       get a code for #ops
```

If a `dropchan` v1 directory is found (`history.jsonl` + `token`), the first start imports
the history into `#general` and turns **guest access on** with the old token so v1 clients
keep working. Turn it off once everyone has an account: `/admin set guest_access off`.

## 3. Roles and what they may do

Server roles: **owner** (one) › **admin** › **user** › **guest** (no account; default channel only).
Channel roles: **owner** › **admin** › **mod** › **member** › **readonly**.

`✓` allowed · `–` denied · `own` only on own content · `≤` only on lower-ranked people

| Action | server owner | server admin | ch owner | ch admin | ch mod | member | readonly |
|---|---|---|---|---|---|---|---|
| read channel (if member) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| send message / file | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – |
| delete message | ✓ | ✓ | ✓ | ✓ | ✓ | own | – |
| set topic, create invite, add by name | ✓ | ✓ | ✓ | ✓ | ✓ | – | – |
| kick / ban / mute | ✓ | ✓ | ✓ | ≤ | ≤ | – | – |
| revoke others' invites, change roles | ✓ | ✓ | ✓ | ≤ | – | – | – |
| channel settings (expire, read-only, max file, rate) | ✓ | ✓ | ✓ | ✓ | – | – | – |
| rotate an e2e channel's key | ✓ | ✓ | ✓ | ✓ | – | – | – |
| purge, delete channel, transfer ownership | ✓ | ✓ | ✓ | – | – | – | – |
| create channel | ✓ | ✓ | any user if `allow_user_channels` is on | | | | |
| create user, reset password, ban user hub-wide, read audit, stats, prune | ✓ | ≤ | – | – | – | – | – |
| promote to server admin, change server settings | ✓ | – | – | – | – | – | – |

`≤` means: your rank in that channel must be strictly higher than the target's, and nobody
below server owner can act on a server admin. This is what prevents mod-vs-mod wars. It is
implemented in exactly one place (`perms.go`) and tested cell by cell.

## 4. Channels

```
/create NAME [topic words…] [readonly=on] [expire=10m] [e2e=on]
/channels                   list yours (server admins see all)
/topic New topic            mod+
/members                    who is in here, roles, who's online, who's muted
/who                        just who's online
/leave                      (owners must /transfer first)
/transfer NAME              hand ownership over; you become admin
/delete NAME                type the channel's own name to confirm — removes messages and files
```
The **default channel** is where guests and admin-created users land. Change it with
`/admin set default_channel NAME`. It cannot be deleted.

**Read-only (announcements) channel:** `/settings readonly=on` — only mods and above may
post; everyone else reads.

### 4.1 Channel settings

```
/settings                              show
/settings expire=2h                    self-destruct (see §7)
/settings readonly=on|off
/settings max_file_mb=200              override the server default for this channel
/settings msg_rate=1                   messages/second per member here (mods get 3×)
```

## 5. Getting people in

Three **registration modes** (`/admin set registration MODE`):

| mode | who can create accounts | typical use |
|---|---|---|
| `invite` (default) | anyone with an invite code that allows sign-up | a group that grows by word of mouth |
| `admin` | only server admins (`/admin useradd`) | tight control; invites then only work for existing accounts |
| `open` | anyone who knows the address (`bch -hub H -user NAME` + `register`) | not recommended |

### 5.1 Invite codes (mod+ in the channel)

```
/invite                       1 use, 3 days, role member, allows sign-up (in invite mode)
/invite 5 24 mod              5 uses, 24 hours, joiners become mods
/invite inf 0 member signup=off    unlimited uses, never expires, existing accounts only
/invites                      list open codes for this channel
/revoke CODE                  mods: own codes; admins: any
```
Hand the person the hub address and the code. They run either
`bch -hub HOST -invite CODE -user THEIRNAME` (new account; they pick the password) or, if
they already have an account, `/join CODE` inside the app. A code grants a role no higher
than one below yours. Invite creation is rate-limited (`invite_rate`, 10/hour/user) so a
compromised mod account can't spray codes.

Sign-ups through an invite join **only** that channel — not `#general`. Add them to other
rooms with `/add NAME` (mod+) from inside those rooms.

### 5.2 Admin-created accounts

```
/admin useradd NAME              generates a temporary password and shows it once
/admin useradd NAME PASSWORD     or choose one
```
The new user lands in the default channel. Tell them to `/passwd` on first login.

### 5.3 Guests (legacy shared token)

`/admin set guest_access on` (a token is generated; `/admin set legacy_token new` rotates
it). Guests connect with `-token X`, live in the default channel only, can't run commands,
and are rate-limited like everyone else. Useful for v1 clients during migration and for
throwaway devices. Off is the right default for a real community.

## 6. Moderation

```
/kick NAME [reason]                   out of this channel; can come back with a new invite
/ban NAME [days] [reason]             out and cannot redeem invites or be /add-ed; 0 days = permanent
/unban NAME  ·  /bans
/mute NAME [minutes] [reason]         still reads, can't post (default 10 min)
/unmute NAME
/role NAME readonly|member|mod|admin  (admin+; only roles below your own)
/del #ID                              delete one message (mods: anyone's)
/purge NAME | last N | before #ID     bulk delete (channel owner+)
```
Hub-wide (server admin+):
```
/admin ban NAME [days] [reason]       account can't log in anywhere; sessions are cut immediately
/admin unban NAME
/admin mod NAME status=disabled       softer than a ban (no reason shown)
/admin mod NAME pass=NEWPASSWORD      reset a password; logs them out everywhere
/admin mod NAME quota_mb=1000 msg_rate=2 upload_rate=5
/admin mod NAME role=admin|user       server owner only; role=owner transfers ownership of the hub
/admin users                          every account, storage used, last seen, online
```

Deletions are **tombstones**: the message body is gone from disk and memory, but clients
that were offline still learn the id was deleted when they replay. Deleting the last
message that references a file deletes the file from the hub's disk and refunds the
uploader's quota.

**Automatic muting:** a member who hits the message rate limit more than `auto_mute_hits`
(20) times in five minutes is muted for `auto_mute_min` (10) minutes in that channel, with
an audit entry. Automation stops floods; humans decide bans.

## 7. Self-destruct channels

```
/create burn a room that forgets expire=1h
/settings expire=10m           on an existing channel (channel admin+)
/settings expire=off
```
Values: `10m`, `2h`, `7d`, `1w`, `off` (minimum 1 minute). What happens:

- Every 15 seconds the hub removes messages older than the expiry from the channel,
  drops their files (deleted from the hub's disk once nothing references them), and sends
  every subscribed client the list of ids that vanished. Screens update immediately.
- Clients also expire locally by timestamp, so a device that is offline still drops the
  messages on time and never shows expired history after reconnecting (the hub filters
  the replay too).
- Files the client **auto-downloaded** from a self-destruct channel are removed from
  `inbox/<channel>/` when their message expires. Files people moved elsewhere, or text
  they copied, are of course beyond reach.
- The status bar shows `🔥 10m` while in such a channel; `/channels` marks them.
- Changing the expiry to a shorter value applies immediately. Setting `expire` on a
  channel with old history removes that history on the next sweep — it says so in the
  reply. `retention_days=N` is accepted as an alias for `expire=Nd`.

What it is not: end-to-end encryption or a guarantee against screenshots. It is
housekeeping that doesn't depend on anyone remembering to do it.

## 8. End-to-end encrypted channels

```
/create incident-room e2e=on          only settable at creation
/e2e status                            am I unlocked here? what epoch?
/e2e rotate                            admin+: new key, shared with everyone online now
/e2e reshare                           re-send the current key without a new epoch
/verify NAME                           compare a member's device fingerprint out of band
```
The hub stores and forwards only ciphertext for these channels — not the message text,
not file contents or names, not the key itself at any point. Each device has its own
key pair; a joining device asks for the channel's key automatically and receives it from
whichever other device is online and already holds it. If nobody who holds it is online
yet, the channel just stays locked for that device until someone is — nothing is lost or
insecurely cached in the meantime.

Kicking or banning someone from an e2e channel automatically triggers a key rotation from
whichever device performed it, if that device holds the key; otherwise `bch` tells you so
you can run `/e2e rotate` from one that does. Rotation is forward-only: it stops a former
member from reading anything new, but cannot retroactively protect what they already saw.

This is real protection against the hub operator, and against anyone who gets into the
hub's disk or database — but it is not magic. Who is in the channel, when, and roughly
how much they sent is still visible to the hub. Full design, the exact cryptography, and
an honest list of what it doesn't cover: **ENCRYPTION.md**.

## 9. Limits — what stops what

Three levels, most specific non-zero value wins: **server default → channel override →
user override**. Server defaults via `/admin set KEY VALUE`; channel via `/settings`;
user via `/admin mod NAME KEY=VALUE`.

| key | default | protects against |
|---|---|---|
| `msg_rate` / `msg_burst` | 5/s, burst 20 (mods 3×) | scroll flooding; repeated hits → auto-mute |
| `max_msg_kb` | 64 | giant pastes; also caps clipboard sync |
| `upload_rate` | 30/min per user | "send 1000 files" scripts |
| `max_file_mb` | 2048 | one upload filling the disk |
| `quota_mb` | 5120 per user (0 = unlimited) | one person filling the disk over time; charged once per unique file, refunded on delete |
| `max_uploads` | 2 concurrent per user | holding many upload slots open |
| upload speed floor | 256 KB/s (fixed) | slowloris uploads |
| dedup | always | the same file 1000 times = one copy, then `upload_rate` |
| `invite_rate` | 10/hour per user | invite spraying |
| `login_rate` | 5 failed/min per username | password guessing from many IPs |
| `conn_rate` | 20/min per IP | connection storms, brute force |
| `sessions_max` / `conns_per_user` | 10 saved / 5 simultaneous | runaway devices |
| `max_users` / `max_channels` | 500 / 200 | hard ceilings |
| `auto_mute_hits` / `auto_mute_min` | 20 hits in 5 min → 10 min | floods without a human awake |

Refusals go back to the sender with a clear reason (`rate limited`, `413 quota exceeded`,
`429 too many uploads`) and are never forwarded. Uploads are refused **before** the bytes
are sent (HTTP `Expect: 100-continue`), so a rejected 2 GB upload costs nothing.

`-public` sets `msg_rate 3` and `upload_rate 12` on a hub's first start. Flags given to
`bch serve` (`-msg-rate`, `-max-file`, `-quota`, …) override and persist the setting.

## 10. Sessions and devices

Clients store a session token, not the password. Each user can see and cut their own:
```
/sessions                     list devices (created, last seen, IP)
/sessions revoke ID           log out one device
/logout                       this device
/logout all                   every device
/passwd                       change password
```
Admins: `/admin mod NAME pass=NEW` resets a password and revokes all their sessions;
`/admin ban` cuts live connections immediately.

## 11. Audit log and stats

```
/admin audit [N] [user=NAME] [ch=NAME]     last N entries (default 30)
/admin stats                                users, channels, connections, storage, orphans, limits
/admin prune                                delete files no message references any more
```
Recorded: create/delete channel, topic, invite/revoke, join/signup/add, kick/ban/unban,
mute/unmute/automute, role changes, transfers, del (others' messages), purge, settings,
useradd/userban/usermod, server settings, prune, failed logins. Members cannot read it.
On disk: `hub/audit.jsonl`, one JSON object per line, never rewritten.

## 12. Server settings reference (`/admin set`, owner only)

| key | values | meaning |
|---|---|---|
| `registration` | `invite` / `admin` / `open` | who can create accounts (§5) |
| `allow_user_channels` | on/off | may ordinary users `/create`? |
| `default_channel` | channel name | where guests and admin-created users land |
| `guest_access` | on/off | accept `-token` / v1 clients |
| `legacy_token` | text or `new` | the guest token |
| `max_users`, `max_channels` | numbers | ceilings |
| all limit keys from §9 | | server defaults |

`/admin set` with no arguments prints everything.

## 13. Common situations

**Someone leaves the group.** `/admin ban NAME` (hub-wide) or `/kick NAME` from specific
channels. Nothing else needs to change — no shared secret to rotate. If they had the
guest token, `/admin set legacy_token new`.

**I forgot the owner password.** On the hub machine: stop the hub, `bch serve -reset-owner`,
start it again. (It rewrites `state.json`, so the hub must not be running.)

**A user forgot theirs.** `/admin mod NAME pass=TEMPORARY` and tell them to `/passwd`.

**Lock the hub down.** `/admin set registration admin`, `/admin set guest_access off`,
`/admin set allow_user_channels off`. Existing invite codes with sign-up stop creating
accounts immediately.

**A channel got too chatty.** `/settings msg_rate=1` there, or `/settings expire=1d` so
it cleans itself.

**Disk is filling up.** `/admin stats` shows totals and unreferenced files; `/admin prune`
removes the latter; `/admin users` shows who holds what; lower `quota_mb` or set
`expire` on the noisy channels.

**Move the hub to another machine.** Copy the whole `hub/` directory (`state.json`,
`messages.jsonl`, `audit.jsonl`, `files/`, and `cert.pem`/`key.pem` if TLS). Clients
reconnect to the new address; sessions stay valid because they live in `state.json`.

## 14. Data layout and backups

```
~/backchannel/hub/
  state.json        users (hashed passwords), channels, memberships, bans, invites, sessions (hashed), settings
  messages.jsonl    messages and file announcements; compacted automatically
  audit.jsonl       append-only audit trail
  files/            <id>_<name> + <id>.sha256 sidecars
  cert.pem key.pem  TLS identity (-tls/-public) — losing these changes the fingerprint every client pinned
```
Back up the directory as a whole. `state.json` is rewritten atomically (temp file +
rename), so a copy is always a consistent snapshot. The hub also saves on Ctrl-C.

## 15. Honest limits

- The hub operator can read everything in an ordinary channel. Use `e2e=on` (§8) for a
  channel that shouldn't be readable by the hub — and read ENCRYPTION.md for what even
  that does and doesn't cover.
- Server admins count as a member of every channel, e2e ones included — they can `/join`
  freely, appear in `/e2e status`'s peer list, and can send a `keyreq` that a peer
  answers the same as anyone else's. e2e channels are not carved out from server-admin
  oversight; choosing who gets that role is what actually limits this, which is also why
  only the owner can appoint one.
- Mods can probe whether a username exists via `/add NAME`. Members cannot.
- Self-destruct is housekeeping, not a security boundary (§7).
