# backchannel Phase 2 — Multi-channel Server with Accounts, Roles and Abuse Control

> Written as `dropchan`; the project was renamed to **backchannel** (`bch`) during
> implementation. Command examples below use `bch`. **§15 records where the built code
> deliberately differs from this spec and what was added.**

Design specification. Written to be built from directly: data model, wire protocol,
permission matrix, every command, every limit, the migration path from v1, and an
implementation order. Where a decision could go two ways, the choice and the reason
are stated so you don't have to re-derive it.

---

## 1. Goals and non-goals

**Goals**
1. Many channels on **one port**, one hub process, one TLS identity, one binary.
2. Accounts: people log in as themselves, not as "whoever has the token".
3. Roles: owner → admin → moderator → member → guest, per channel.
4. Channels are **private by default and unlisted**: you cannot see, join, or even
   confirm the existence of a channel you weren't invited to.
5. Moderation: kick, ban (per channel and server-wide), mute, purge, invite revocation.
6. Abuse control moved from IP to account: message rate, upload rate, storage quota,
   dedup, size caps, per-channel retention.
7. Users cycle through their channels in the TUI without restarting or retyping anything.
8. Every privileged action lands in an append-only audit log.
9. v1 clients keep working against a v2 hub (they land in a `#general`-style default channel).

**Non-goals (deliberately)**
- Federation between hubs. One hub = one community. Simpler, and secrecy is easier.
- Public discovery / channel directory. Contradicts goal 4.
- Rich media rendering, threads, reactions. Nice; not this phase.
- Perfect end-to-end encryption. §11 gives an optional design; the default is "hub can read".

## 2. Can multiple channels share one port?

Yes, trivially. A port identifies a *process*, not a conversation. Every message already
carries a JSON envelope; adding `"ch":"design"` to it tells the hub which room to route
to. The hub keeps one connection per client and one **subscription set** per connection
(the channels that client is in). Fan-out becomes "for each client subscribed to `ch`"
instead of "for each client". IRC, Slack, Matrix and Discord all work this way.

The v1 pattern of "one `serve` per channel" is what you'd do *without* this — it works
but multiplies ports, tokens, certificates, service files, and reconnects. Phase 2
replaces it.

## 3. Concepts

| Term | Meaning |
|---|---|
| **Hub** | The server. Has one owner. Holds all users, channels, messages, files. |
| **User** | An account: `username`, password hash, server role, quotas, created-by, status (active / banned / disabled). |
| **Session** | One authenticated TCP connection. A user may have several (Mac + phone + Windows). |
| **Channel** | A room: `name`, `id`, topic, members with per-channel roles, settings, retention. Unlisted unless you're a member. |
| **Membership** | (user, channel, role, joined_at, muted_until, last_read_id). |
| **Invite** | Token-like code that grants membership when redeemed. Has uses-left and expiry. Created by mods+. |
| **Server role** | `owner` (exactly one), `admin`, `user`. Applies hub-wide. |
| **Channel role** | `owner`, `admin`, `mod`, `member`, `readonly`. Applies in that channel only. |
| **Audit log** | Append-only record of every privileged action. Owners/admins can read it. |

Server admins outrank everything inside channels (they can enter any channel, act as
channel admin). Channel roles govern what ordinary users can do where.

## 4. Permission matrix

`✓` allowed · `–` denied · `own` only on their own content · `≤` only on users of lower rank

| Action | server owner | server admin | ch owner | ch admin | ch mod | member | readonly |
|---|---|---|---|---|---|---|---|
| read channel (if member) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| send message / file | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | – |
| delete message | ✓ | ✓ | ✓ | ✓ | ✓ | own | – |
| set topic | ✓ | ✓ | ✓ | ✓ | ✓ | – | – |
| create invite | ✓ | ✓ | ✓ | ✓ | ✓ | – | – |
| revoke invite | ✓ | ✓ | ✓ | ✓ | own | – | – |
| kick from channel | ✓ | ✓ | ✓ | ≤ | ≤ | – | – |
| ban from channel | ✓ | ✓ | ✓ | ≤ | ≤ | – | – |
| mute in channel | ✓ | ✓ | ✓ | ≤ | ≤ | – | – |
| promote/demote in channel | ✓ | ✓ | ✓ | ≤ | – | – | – |
| purge channel history | ✓ | ✓ | ✓ | – | – | – | – |
| set channel limits/retention | ✓ | ✓ | ✓ | ✓ | – | – | – |
| delete channel | ✓ | ✓ | ✓ | – | – | – | – |
| create channel | ✓ | ✓ | (any user if `allow_user_channels`) | | | | |
| create user / reset password | ✓ | ✓ | – | – | – | – | – |
| ban user server-wide | ✓ | ≤ | – | – | – | – | – |
| promote to server admin | ✓ | – | – | – | – | – | – |
| read audit log | ✓ | ✓ | – | – | – | – | – |
| change server settings | ✓ | – | – | – | – | – | – |
| see member list of a channel | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| see that a channel *exists* | ✓ | ✓ | only members | | | | |

Rule for `≤`: you can act on someone only if your role in that channel is strictly
higher than theirs, and you can never act on a server admin/owner. Prevents mod-vs-mod wars.

## 5. Data model

Storage recommendation: **SQLite** via `modernc.org/sqlite` (pure Go, no cgo, keeps
cross-compilation one-command). Single file `hub.db` in WAL mode. The JSONL log stays
for messages only if you want grep-ability; otherwise messages go in SQLite too.
If you'd rather stay dependency-free, the same tables map cleanly to one JSON file per
entity type plus the existing JSONL for messages — slower to query, fine under ~50 users.

```sql
CREATE TABLE users (
  id          INTEGER PRIMARY KEY,
  name        TEXT UNIQUE NOT NULL COLLATE NOCASE,   -- 2–24 chars, [a-z0-9_.-]
  pass_hash   TEXT NOT NULL,                          -- argon2id (golang.org/x/crypto/argon2) or scrypt
  role        TEXT NOT NULL DEFAULT 'user',           -- owner | admin | user
  status      TEXT NOT NULL DEFAULT 'active',         -- active | banned | disabled
  ban_reason  TEXT, ban_until INTEGER,
  created_at  INTEGER NOT NULL, created_by INTEGER,
  quota_bytes INTEGER,                                -- NULL → server default
  msg_rate    REAL, upload_rate REAL                  -- NULL → server default
);

CREATE TABLE sessions (
  token       TEXT PRIMARY KEY,                       -- 32 random bytes, hex; stored hashed (sha256)
  user_id     INTEGER NOT NULL REFERENCES users(id),
  device      TEXT,                                   -- "mac", "win-work" (client-supplied label)
  created_at  INTEGER, last_seen INTEGER, expires_at INTEGER
);

CREATE TABLE channels (
  id          INTEGER PRIMARY KEY,
  name        TEXT UNIQUE NOT NULL COLLATE NOCASE,   -- 1–32 chars, [a-z0-9_-]
  topic       TEXT DEFAULT '',
  created_at  INTEGER, created_by INTEGER,
  retention_days INTEGER,                             -- NULL → keep forever
  max_file_mb INTEGER, msg_rate REAL,                 -- NULL → server default
  readonly    INTEGER DEFAULT 0,                      -- announcements channel
  is_default  INTEGER DEFAULT 0                       -- v1 clients / new users land here
);

CREATE TABLE memberships (
  user_id     INTEGER REFERENCES users(id),
  channel_id  INTEGER REFERENCES channels(id),
  role        TEXT NOT NULL DEFAULT 'member',         -- owner | admin | mod | member | readonly
  joined_at   INTEGER, invited_by INTEGER,
  muted_until INTEGER,
  last_read   INTEGER DEFAULT 0,                      -- message id, for unread counts
  PRIMARY KEY (user_id, channel_id)
);

CREATE TABLE channel_bans (
  channel_id INTEGER, user_id INTEGER, by INTEGER, reason TEXT, until INTEGER,
  PRIMARY KEY (channel_id, user_id)
);

CREATE TABLE invites (
  code        TEXT PRIMARY KEY,                       -- 10 random chars, e.g. "k7Qz-9mPd2"
  channel_id  INTEGER REFERENCES channels(id),
  created_by  INTEGER, created_at INTEGER,
  uses_left   INTEGER,                                -- NULL → unlimited
  expires_at  INTEGER,                                -- NULL → never
  grant_role  TEXT DEFAULT 'member',
  creates_account INTEGER DEFAULT 0                   -- 1: redeemer may register a new user with it
);

CREATE TABLE messages (
  id          INTEGER PRIMARY KEY,                    -- global monotonic (keeps v1 "since" semantics)
  channel_id  INTEGER NOT NULL,
  user_id     INTEGER NOT NULL,
  ts          INTEGER NOT NULL,
  kind        TEXT NOT NULL,                          -- msg | file | sys
  body        TEXT,                                   -- text, or JSON {name,size,fid,sha256} for files
  deleted_by  INTEGER, deleted_at INTEGER             -- soft delete: tombstone replaces body
);
CREATE INDEX messages_ch ON messages(channel_id, id);

CREATE TABLE files (
  fid         TEXT PRIMARY KEY,
  sha256      TEXT UNIQUE,
  size        INTEGER, name TEXT,
  owner_id    INTEGER,                                -- first uploader (charged for quota)
  refs        INTEGER DEFAULT 1,                      -- messages pointing at it; delete on disk when 0
  created_at  INTEGER
);

CREATE TABLE audit (
  id INTEGER PRIMARY KEY, ts INTEGER, actor INTEGER, action TEXT,
  target_user INTEGER, target_channel INTEGER, detail TEXT
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT);   -- server-wide knobs, §9
```

**Password hashing**: argon2id, `time=1, memory=64MB, threads=4`, 16-byte salt, or
scrypt `N=32768`. Never bcrypt-less SHA. Store as `$argon2id$v=19$m=65536,t=1,p=4$<salt>$<hash>`.

**Session tokens** replace the v1 shared token for authenticated clients. The client
logs in once with a password, receives a session token, saves it in `config.json`, and
uses it from then on. Sessions can be listed and revoked ("log out my old laptop").

## 6. Wire protocol (v2)

Still newline-delimited JSON over TCP (TLS mandatory in `-public`). Every frame has `t`.
Two new envelope fields: `ch` (channel name) and `rid` (request id, so replies can be
matched to the command that caused them).

### 6.1 Connection and auth

```
→ {"t":"hello","v":2,"device":"mac","auth":{"session":"…"}}
→ {"t":"hello","v":2,"device":"mac","auth":{"user":"alice","pass":"…"}}
→ {"t":"hello","v":2,"device":"mac","auth":{"invite":"k7Qz-9mPd2","user":"newguy","pass":"…"}}   // register via invite
→ {"t":"hello","token":"…"}                             // v1 client: no "v" → treated as guest in default channel

← {"t":"err","code":"auth","text":"bad credentials"}
← {"t":"ok","user":"alice","role":"user","session":"…",       // session only on password login
     "channels":[{"name":"general","role":"member","unread":12,"topic":"…"},
                 {"name":"design","role":"mod","unread":0}],
     "limits":{"max_msg":65536,"max_file":2147483648,"quota_left":5368709120}}
```

Failed logins count against `-conn-rate` **and** a per-username bucket (5/min) — so an
attacker can't brute-force one account from many IPs.

### 6.2 Subscribing and history

After `ok`, the client is subscribed to nothing. It subscribes to the channels it wants
to render and asks for history per channel:

```
→ {"t":"sub","ch":"general","since":1234}      // replay messages with id > 1234, cap 500
← {"t":"msg","ch":"general","id":1235,"from":"bob","ts":…,"text":"…","hist":true}
← … 
← {"t":"synced","ch":"general","last":1301}
→ {"t":"unsub","ch":"design"}
```
Subscribing to a channel you're not a member of returns
`{"t":"err","code":"nochan","text":"no such channel"}` — **identical** to the error for a
channel that doesn't exist. That's goal 4: no existence oracle.

Typical client: `sub` all channels from `ok` with each channel's saved `since`, render the
active one, count unread on the others.

### 6.3 Messages and files

```
→ {"t":"msg","ch":"design","text":"hello","rid":"a1"}
← {"t":"msg","ch":"design","id":1302,"from":"alice","ts":…,"text":"hello"}     // to all subscribers incl. sender
← {"t":"err","code":"ratelimit","rid":"a1","text":"slow down"}                   // to sender only
← {"t":"deleted","ch":"design","id":1290,"by":"mod1"}                             // tombstone broadcast
```

Files: HTTP as in v1 but authenticated by session and scoped to a channel:
```
PUT  /up?ch=design&name=spec.pdf      headers: Authorization: Bearer <session>, Expect: 100-continue
GET  /f/<fid>                          headers: Authorization: Bearer <session>
```
The hub checks membership of `ch` on upload, and on download checks that the requester is
a member of **some** channel containing a message that references `fid` — so a file id
leaked from a private channel is useless to outsiders.

### 6.4 Commands

All moderation/admin operations are one frame type:
```
→ {"t":"cmd","rid":"c9","name":"ban","ch":"design","args":{"user":"troll","reason":"spam","days":30}}
← {"t":"res","rid":"c9","ok":true,"text":"troll banned from #design for 30d"}
← {"t":"res","rid":"c9","ok":false,"code":"perm","text":"you need mod in #design"}
```
Commands return the same `code":"perm"` whether the target exists or not, where that
could leak information (e.g. `invite` to a channel you can't see).

Command list (the TUI maps `/slash` forms onto these):

| cmd | scope | args | who |
|---|---|---|---|
| `channels` | server | – | any → only channels you're in |
| `create` | server | name, topic?, private(default true) | admin, or user if setting allows |
| `delete` | channel | – (name confirm in TUI) | ch owner+ |
| `topic` | channel | text | mod+ |
| `invite` | channel | uses?, hours?, role?, allow_signup? | mod+ → returns code |
| `revoke` | channel | code | mod+ (own) / admin+ |
| `join` | server | invite code | anyone with the code |
| `leave` | channel | – | any (owner must transfer first) |
| `members` | channel | – | member+ |
| `kick` | channel | user, reason? | mod+ (`≤`) |
| `ban` | channel | user, reason?, days? | mod+ (`≤`) |
| `unban` | channel | user | mod+ |
| `mute` | channel | user, minutes | mod+ (`≤`) |
| `role` | channel | user, role | admin+ (`≤`) |
| `purge` | channel | user? / before_id? / last N | ch owner+ |
| `del` | channel | msg id | own / mod+ |
| `settings` | channel | retention_days?, max_file_mb?, msg_rate?, readonly? | ch admin+ |
| `useradd` | server | name, temp password or `invite_only` | server admin+ |
| `userban` | server | user, reason?, days? | server admin+ (`≤`) |
| `userunban` | server | user | server admin+ |
| `usermod` | server | user, role / quota_mb / status | server owner (role) / admin (quota, status) |
| `passwd` | server | old, new | self |
| `sessions` | server | – / revoke id | self |
| `audit` | server | limit?, user?, channel? | server admin+ |
| `stats` | server | – | server admin+ → users, channels, storage, rates |
| `transfer` | channel | user | ch owner → makes them owner |

### 6.5 Events pushed to clients

`join`, `part`, `kicked`, `banned`, `muted`, `role`, `topic`, `deleted`, `purged`,
`invited` (you were added to a channel), `removed` (channel deleted / you were removed →
client drops it from its list), `quota` (you crossed 80% / 100%). Each carries `ch`
where relevant. A client being banned server-wide receives `{"t":"err","code":"banned"}`
and the connection closes; reconnects are refused at `hello`.

## 7. Abuse control — per account, per channel

Every limit exists at three levels; the most specific non-null wins:
**server default → channel override → user override**.

| Limit | Default | Notes |
|---|---|---|
| `msg_rate` | 3/s, burst 15 | per (user, channel). Mods+ get 3× (they're cleaning up). |
| `msg_max` | 64 KB | |
| `upload_rate` | 10/min | per user across channels |
| `max_file` | 2 GB | |
| `quota_bytes` | 5 GB per user | charged to the **first** uploader of unique bytes; dedup means re-sends are free and don't count |
| `channel_storage` | unlimited | optional cap per channel |
| `retention_days` | ∞ | per channel; nightly job soft-deletes older messages and decrements file refs |
| `mentions_per_msg` | 10 | if you add @mentions |
| `invite_rate` | 10/hour per user | stops invite-code enumeration abuse |
| `login_rate` | 5/min per username, 20/min per IP | |
| `sessions_max` | 10 per user | |
| `conns_per_user` | 5 simultaneous | |

**Dedup by content hash** (already in v1): the hub hashes while writing; identical bytes
reuse the existing `fid`. Quota is charged once. "Send the same file 1000 times" costs
one copy on disk and is then stopped by `upload_rate` on the *announcements*.

**Delete accounting**: files are reference-counted by messages. Deleting/purging/expiring
the last message that references a file deletes it from disk and refunds the quota.

**Slow uploads**: the v1 deadline (≥256 KB/s) stays. Add `max_concurrent_uploads=2` per user.

**Behaviour on limit hit**: refuse with an error to the sender; never forward; log a
counter. If a user hits `msg_rate` more than 20 times in 5 minutes → auto-mute 10 min in
that channel + audit entry. Automation should be conservative; humans do the bans.

## 8. Secrecy properties

1. Channel names are never enumerable. `channels` lists only your memberships. Errors for
   "doesn't exist" and "not a member" are byte-identical.
2. Invite codes are the only way in. They are unguessable (60+ bits), rate-limited on
   redemption attempts (5/min/IP), single- or few-use, expiring.
3. File ids are random and downloads are membership-checked (§6.3).
4. Members see only the channel's *own* member list. There is no global user directory
   for non-admins; `@name` autocompletion in the client uses members of the current channel.
5. Audit log is admin-only and never leaves the server.
6. The hub operator can read everything. That is inherent to a relay with server-side
   history. If that's unacceptable for some channels, see §11.
7. Metadata the hub inevitably knows: who is online, who is in which channel, message
   sizes and timing. Document this to users honestly.

## 9. Server settings (`settings` table, owner-only)

| key | default | meaning |
|---|---|---|
| `registration` | `invite` | `invite` (only via invite codes) / `admin` (admins create users) / `open` (anyone — not recommended) |
| `allow_user_channels` | `false` | can ordinary users create channels? |
| `default_channel` | `general` | where v1/guest clients land |
| `guest_access` | `false` | accept v1 `hello` with the legacy token as a read/write guest in the default channel |
| `legacy_token` | (v1 token) | for `guest_access` |
| `max_users`, `max_channels` | 500 / 200 | hard ceilings |
| all limits from §7 | | server defaults |

## 10. Client / TUI changes

Layout gains a **channel bar** (row above the status bar, or a left gutter on wide
terminals):

```
 #general  #design(3)  #ops  #random(12)               ← unread counts, active one highlighted
 ──────────────────────────────────────────────────────
 connected · alice@hub.example · #design · 4 online
 > _
```

| Key / command | Effect |
|---|---|
| `Alt-←`/`Alt-→`, or `Ctrl-N`/`Ctrl-P` | cycle channels (cooked mode can't see Alt/Ctrl combos on every terminal, so **also** provide `/n`, `/p`, `/1`…`/9`) |
| `/c design` or `/design` | switch (matches your channels only) |
| `/join k7Qz-9mPd2` | redeem invite |
| `/leave` | leave current channel |
| `/create name [topic]` | admin / permitted user |
| `/invite [uses] [hours]` | mod+, prints a code to hand out |
| `/members`, `/topic …`, `/kick u`, `/ban u [days] [reason]`, `/mute u [min]`, `/role u mod`, `/del 1290`, `/purge u`, `/settings …` | map 1:1 to §6.4 |
| `/admin users` `/admin useradd name` `/admin ban name` `/admin audit [n]` `/admin stats` | server admin commands under one prefix so the help stays readable |
| `/login user` | prompts for password (TUI turns off echo for that line via `stty -echo` / `SetConsoleMode` — the one place raw-ish mode is needed) |
| `/logout [all]` | revoke this / all sessions |
| `/sessions` | list devices |

Files: `inbox/<channel>/` per channel. Auto-download only in the **active** channel plus
channels you mark `/watch`; others download on `/get`. Outbox: `outbox/<channel>/`, top
level goes to the active channel.

Unread and `since`: the client stores `last_read` per channel locally and sends it back
via `{"t":"read","ch":"design","id":1301}` when you view a channel, so other devices
of the same user agree on unread counts.

Notifications: a `/notify` toggle; on new message in a non-active channel, ring the
terminal bell (`\a`) and bump the count. Mentions of your name always ring.

`bch send -ch design file.png` for scripts.

## 11. Optional: end-to-end encrypted channels

If some channels must be unreadable by the hub operator:

- Channel has a symmetric **channel key** (32 bytes). Generated by the creator, never sent
  to the hub in the clear.
- Each user has a long-term public key (X25519) registered at signup; the hub stores public
  keys and distributes them (hub can substitute keys — mitigate with fingerprint display and
  `/verify user`, same TOFU pattern as the TLS pin).
- Inviting someone = encrypting the channel key to their public key and posting it as a
  `keyshare` message; the hub relays but can't read it.
- Messages/files in an E2E channel are encrypted client-side (XChaCha20-Poly1305 via
  `golang.org/x/crypto`), the hub stores ciphertext, dedup happens on ciphertext hash (so
  identical plaintext from two senders is *not* deduped — accepted).
- Rotate the key on kick/ban (creator or admin generates a new key, reshares to remaining
  members; old messages stay readable to whoever had the old key — that's inherent).
- Search, retention, and moderation of *content* stop working for the hub in E2E channels;
  moderation becomes membership-only. Make that explicit in the `/create --e2e` help text.

Build this last, and only if needed. It roughly doubles client complexity.

## 12. Migration from v1

1. v2 hub on first start with an existing v1 `hub/` dir: imports `history.jsonl` into
   `messages` as channel `general`, imports `files/`, sets `legacy_token` from `token`,
   prints a one-time **owner** username/password.
2. `guest_access=true` by default after migration so v1 clients keep working in `#general`.
   Owner turns it off once everyone has an account.
3. v1 clients see v2 only as "the channel got a topic". v2 clients talking to a v1 hub fall
   back to single-channel mode (no `v` in `ok`).

## 13. Implementation order

Each step is shippable on its own.

1. **Channels + subscriptions, still shared token.** Add `ch` to frames, `sub`/`unsub`,
   per-channel history, `create`/`channels`/`topic`. TUI channel bar + switching. *(most of
   the user-visible value; ~400 lines)*
2. **Accounts + sessions.** `users`, `sessions`, argon2id, `hello` variants, `/login`,
   `passwd`, per-user limits. Guest mode for legacy token.
3. **Memberships, invites, roles, permission matrix.** Unlisted channels, identical errors,
   file download membership check. `kick/ban/mute/role/leave/transfer`.
4. **Moderation + audit.** `del/purge`, tombstones, audit table, `/admin audit`, auto-mute.
5. **Quotas + retention + ref-counted files.** Nightly job, `quota` events, `stats`.
6. **Polish:** unread sync across devices, notifications, `/sessions`, per-channel inbox.
7. **(Optional) E2E channels.**

Suggested file layout for the hub side:
```
hub/        server.go (accept, hello, dispatch)  auth.go  store.go (sqlite)  channels.go
            perms.go (the matrix as code)  limits.go  moderation.go  files.go  audit.go  migrate.go
client/     conn.go  channels.go  commands.go  tui.go  files.go
proto/      msg.go (frame types + codes)  — shared, versioned
```

Testing that matters most: a table-driven test of `perms.go` against §4 (every cell),
and an "oracle" test asserting the not-a-member and no-such-channel error bytes are
identical for every command.

## 14. Things worth knowing while building this

- **Never build access control as `if role == "admin"` scattered through handlers.** One
  function `can(actor, action, target) bool` implementing §4, called at the top of every
  handler, tested exhaustively. Security bugs in chat systems are almost always a missed check.
- **Identical errors are a feature.** Anywhere a response differs based on secret state
  (does the channel exist? is that a real username?) you've built an oracle. Log the
  distinction server-side, return the same bytes.
- **Rate limit on identity, not just IP**, and on *failed* auth per username, or one
  patient attacker with many IPs walks in.
- **Soft-delete with tombstones**, not hard delete, or clients that were offline will
  never learn the message is gone (they replay by `id`, and the id would simply be missing).
- **Global monotonic message ids** across channels keep `since` semantics trivial and let
  you order events hub-wide for the audit log.
- **Reference-count files** rather than deleting on message delete — dedup means one file
  may back many messages in many channels.
- **Sessions, not passwords, in config files.** A stolen laptop leaks one revocable
  session, not the password.
- **Make the owner account unbannable and undeletable** except by itself; make ownership
  transfer explicit. Every "admin locked themselves out" story comes from missing this.
- **Keep the JSON frames flat and additive.** New fields never break old clients; renamed
  fields do. Version the `hello`, never the individual frames.
- **Back-pressure**: keep the v1 "drop the slow client" behaviour per subscription, and add
  a per-client outbound byte budget so a member on a slow link can't make the hub buffer
  gigabytes.
- **Time**: store UTC millis everywhere; render local time in the client only.
- **What you now control that you didn't before**: who is in the room, for how long, how
  loud they can be, how much disk they can use, how long things are kept, and a complete,
  tamper-evident record of who changed any of that. That's the whole difference between a
  shared secret and a community.

## 15. Implementation notes (what was built, and where it differs)

Built in `code/` as v2.0.0. Deviations from the sections above, with the reason:

| Spec said | Built | Why |
|---|---|---|
| SQLite via `modernc.org/sqlite` (§5) | `state.json` (atomic rename) + `messages.jsonl` + `audit.jsonl`, all in memory | keeps the binary stdlib-only and one-command cross-compiled; at ≤ hundreds of users the state is a few hundred KB |
| argon2id (§5) | PBKDF2-HMAC-SHA256, 600k rounds, from `crypto/pbkdf2` (Go 1.24) | argon2 needs `golang.org/x/crypto`; PBKDF2 at this cost is the stdlib-only OWASP option. Hashing runs outside the hub lock |
| `hub/ client/ proto/` packages (§13) | flat `package main`, one file per concern | `go build .` stays trivial; the important invariant — one `can()` in `perms.go` with a cell-by-cell test — is kept |
| `retention_days`, nightly job (§7) | `expire` per channel in seconds (`10m`…`1w`), 15 s sweep, `expired` events pushed to clients, clients also expire locally and delete auto-downloaded files | the user asked for a self-destruct mode; days were too coarse and silent server-side deletion left stale screens |
| invite sign-ups land in the invited channel *and* presumably `#general` | invited channel **only**; `/add NAME` (new `add` cmd, mod+) puts existing accounts into further rooms | secrecy: joining one room should not reveal the lobby |
| `kick`/`ban` events (§6.5) | one event to the target (directly) and one to the remaining members; presence `part` suppressed | avoids the target seeing it twice and the room seeing "left" + "kicked" |
| server admins "can enter any channel" (§3) | they are listed in every channel's `ok`/`channels` and may act everywhere; channel staff cannot touch them; only the owner appoints them | makes the trust boundary explicit |
| flags vs settings unspecified | `state.json` settings are authoritative; `bch serve -flag` overrides **and persists**; `-public` applies stricter defaults only on a hub's first start | so `/admin set` changes survive restarts, and a restart without `-public` doesn't silently loosen a tuned hub |
| `useradd`/`passwd` only | `usermod pass=` (admin reset, revokes all sessions), `sessions revoke`, `logout all` | the lockout stories from §14 |
| — | **clipboard sync** (`clip` frames, user-scoped, never stored; native Win32 clipboard) | new requirement; see CLIPBOARD.md |
| — | guests get a stable negative pseudo-id from their nick | presence, rate limits and clipboard routing work for guests without storing accounts |

Not built: §11 end-to-end channels (design stands), Alt/Ctrl key bindings (cooked-mode TUI
cannot see them reliably; `/1`…`/9`, `/n`, `/p`, `/c NAME` instead), @mention autocompletion.

Tests (`go test ./...`): the full §4 matrix cell by cell; `canTarget` rules; the
existence-oracle rule (byte-identical `nochan` for `sub` and `cmd`); invite sign-up and
single use; kick/ban/unban flow; read-only, mute, rate limit and auto-mute; v1/guest
compatibility; file membership checks, dedup accounting, quota, per-channel max file,
reclaim on purge; clipboard routing (never to another user, never stored); read sync;
server admin/owner rules; password reset; expiry sweep incl. file deletion and replay
filtering; store round-trip on restart.
