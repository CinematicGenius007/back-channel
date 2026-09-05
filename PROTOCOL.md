# backchannel — Wire Protocol v2

Everything a script, bot or alternative client needs. The protocol is newline-delimited
JSON over TCP (TLS 1.3 when the hub runs `-tls`/`-public`), plus a tiny HTTP/1.1 server for
files. One frame type, `Msg`; `t` says what it is. Frames are **flat and additive**: new
fields never break old clients. Only `hello` is versioned.

Conventions: `→` client to hub, `←` hub to client. Times are unix milliseconds UTC.
Message ids are hub-assigned and monotonic **across all channels**.

---

## 1. Connect and authenticate

Open TCP to `HOST:7777` (TLS if the hub says so) and send exactly one `hello` line within 10 s.

```jsonc
→ {"t":"hello","v":2,"device":"mac","auth":{"session":"<64 hex>"}}                 // resume a saved session
→ {"t":"hello","v":2,"device":"mac","auth":{"user":"alice","pass":"…"}}           // password login → new session
→ {"t":"hello","v":2,"device":"mac","auth":{"invite":"k7Qz-9mPd2","user":"newguy","pass":"…"}}  // sign up via invite
→ {"t":"hello","v":2,"device":"mac","auth":{"user":"x","pass":"…","register":true}} // open registration (if allowed)
→ {"t":"hello","v":2,"name":"oldpc","token":"<legacy token>"}                      // guest (if guest_access is on)
→ {"t":"hello","name":"oldpc","token":"<legacy token>","since":0}                  // v1 client (no "v")
```
Replies:
```jsonc
← {"t":"ok","v":2,"user":"alice","role":"user","session":"<only after password/invite login>",
   "channels":[{"name":"general","role":"member","unread":12,"last":1301,"topic":"…","members":9},
               {"name":"design","role":"mod","expire":3600,"readonly":false}],
   "limits":{"max_msg":65536,"max_file":2147483648,"quota_left":5368709120},"text":"backchannel hub 2.0.0"}
← {"t":"err","code":"auth","text":"bad username or password"}
← {"t":"err","code":"banned","text":"you are banned from this hub: spam"}
← {"t":"err","code":"ratelimit","text":"too many login attempts — wait a minute"}
← {"t":"err","code":"limit","text":"too many simultaneous connections (max 5) …"}
```
Store `session`; never store the password. Sessions are revocable (`/sessions`, `/logout`,
admin reset) — on `code:"auth"` at hello, forget the session and ask for a login again.

v1 clients receive a v1-shaped `ok` (`{"t":"ok","name":…,"users":[…]}`) after a replay of
the default channel, and are subscribed to the default channel only.

## 2. Channels: subscribe, replay, presence

After `ok` you are subscribed to nothing. Subscribe to what you want to see:
```jsonc
→ {"t":"sub","ch":"design","since":1234}      // replay messages with id > 1234 (cap 400 per sub)
← {"t":"msg","ch":"design","id":1235,"ts":…,"from":"bob","text":"…","hist":true}
← {"t":"deleted","ch":"design","id":1240,"by":"mod1","hist":true}     // tombstone of a deleted message
← {"t":"synced","ch":"design","last":1301}
→ {"t":"unsub","ch":"design"}
```
- `since:0` replays the last 400 messages. Messages past a self-destruct channel's expiry
  are never replayed.
- Subscribing to a channel that doesn't exist **or** that you aren't a member of returns
  the identical frame `{"t":"err","code":"nochan","text":"no such channel","ch":"…"}`.
  Do not try to distinguish them; the hub is built so you can't.
- Presence: the first session of a user subscribing to a channel triggers
  `{"t":"join","ch":…,"from":"bob"}` to the channel; the last one leaving triggers `part`.
- `→ {"t":"who","ch":"design"}` `← {"t":"who","ch":"design","users":["alice","bob"]}`

## 3. Messages

```jsonc
→ {"t":"msg","ch":"design","text":"hello","rid":"a1"}
← {"t":"msg","ch":"design","id":1302,"ts":…,"from":"alice","text":"hello"}     // to every subscriber, sender included
← {"t":"err","code":"ratelimit","rid":"a1","ch":"design","text":"slow down — rate limited"}
← {"t":"err","code":"toolong","rid":"a1","text":"message too long (max 64 KB)"}
← {"t":"err","code":"muted","rid":"a1","until":1730000000000,"text":"you are muted in #design for another 4m"}
← {"t":"err","code":"readonly","rid":"a1","text":"#design is read-only for you"}
← {"t":"err","code":"perm"|"nochan",…}
```
`rid` is optional and echoed on errors so you can match them. A missing `ch` means the
default channel (that is how v1 clients work).

Marking read (syncs unread counts across your devices):
```jsonc
→ {"t":"read","ch":"design","id":1302}
← {"t":"read","ch":"design","id":1302}        // to your *other* sessions
```

## 4. Clipboard sync

```jsonc
→ {"t":"clip","text":"…"}                     // ≤ max_msg bytes, rate-limited like messages
← {"t":"clip","from":"mac","text":"…"}        // to the sender's other sessions only; never stored
```

## 5. Files (HTTP on port+1)

Auth header: `Authorization: Bearer <session>` (or `X-Session: …`, or `?session=…`).
Guests: `X-Token: <legacy token>` (or `?token=…`) plus `&from=NICK`.

```sh
# upload into #design — refused before the body is sent if the hub says no
curl -T spec.pdf -H "Authorization: Bearer $S" -H "Expect: 100-continue" \
     "http://hub:7778/up?ch=design&name=spec.pdf&device=mac"
# → {"fid":"a3b1acb38b0c3398","size":1234,"name":"spec.pdf","sha256":"…"}

# download (you must be a member of a channel containing a message that references the file)
curl -H "Authorization: Bearer $S" http://hub:7778/f/a3b1acb38b0c3398 -o spec.pdf

# public one-line status (no auth)
curl http://hub:7778/
```
With `-public`, use `https://` and `curl -k` (the certificate is self-signed; `bch` pins
its fingerprint instead of trusting a CA).

| status | meaning |
|---|---|
| 200 | ok |
| 401 | no / bad credentials |
| 403 | you can't post here (read-only, muted) |
| 404 | `no such channel` (or not a member) · `no such file` (or not yours to see) — identical on purpose |
| 411 | `Content-Length` required |
| 413 | file too large for this channel, or storage quota exceeded |
| 429 | upload rate or concurrent-upload limit |

After a successful upload the hub broadcasts to the channel:
```jsonc
← {"t":"file","ch":"design","id":1303,"ts":…,"from":"alice","device":"mac","name":"spec.pdf","size":1234,"fid":"a3b1…","sha256":"…"}
```
Identical bytes uploaded again (by anyone, anywhere) reuse the same `fid`; only the first
uploader's quota is charged.

## 6. Commands

Every moderation/administration action is one frame type:
```jsonc
→ {"t":"cmd","rid":"c9","name":"ban","ch":"design","args":{"user":"troll","days":"30","reason":"spam"}}
← {"t":"res","rid":"c9","ok":true,"text":"troll banned from #design"}
← {"t":"res","rid":"c9","code":"perm","text":"you need mod in #design"}
← {"t":"res","rid":"c9","ok":true,"lines":["…","…"]}          // tabular output
← {"t":"res","rid":"c9","ok":true,"channels":[{…}]}            // channels / create / join
```
`args` values are always strings. `ch` scopes channel commands. Guests may only run `channels`.

| name | scope | args | minimum role |
|---|---|---|---|
| `channels` | server | – | any |
| `create` | server | `name`, `topic`?, `readonly`=on?, `expire`? | server admin, or user if `allow_user_channels` |
| `delete` | channel | `confirm`=channel name | ch owner |
| `topic` | channel | `text` | mod |
| `invite` | channel | `uses` (n / inf), `hours` (0 = never), `role`, `signup`=off? | mod |
| `invites` / `revoke` | channel | – / `code` | mod (revoke others': admin) |
| `join` | server | `code` | any account |
| `leave` | channel | – | member (owner must transfer) |
| `members` | channel | – | readonly |
| `add` | channel | `user` | mod |
| `kick` / `ban` | channel | `user`, `reason`?, `days`? (ban) | mod, target lower-ranked |
| `unban` / `bans` | channel | `user` / – | mod |
| `mute` | channel | `user`, `minutes` (0 = unmute), `reason`? | mod, target lower-ranked |
| `role` | channel | `user`, `role` (readonly/member/mod/admin) | admin; role below own |
| `transfer` | channel | `user` | ch owner |
| `del` | channel | `id` | own message; mod for others |
| `purge` | channel | `user` \| `last` \| `before` | ch owner |
| `settings` | channel | `expire`, `readonly`, `max_file_mb`, `msg_rate` (none → show) | ch admin |
| `passwd` | self | `old`, `new` | – |
| `sessions` | self | `revoke`=id prefix? | – |
| `logout` | self | `all`=on? | – |
| `users` | server | – | server admin |
| `useradd` | server | `name`, `pass`? | server admin |
| `userban` / `userunban` | server | `user`, `days`?, `reason`? | server admin, target lower |
| `usermod` | server | `user` + `role` / `pass` / `quota_mb` / `msg_rate` / `upload_rate` / `status` | server admin (`role`: owner) |
| `audit` | server | `n`?, `user`?, `ch`? | server admin |
| `stats` / `prune` | server | – | server admin |
| `set` | server | `KEY`=`VALUE` … (none → show) | server owner |

## 7. Events pushed to clients

| `t` | fields | meaning |
|---|---|---|
| `msg`, `file` | see §3, §5 | new content (`hist:true` when replayed) |
| `deleted` | `ch`, `id`, `by` | one message tombstoned |
| `purged` | `ch`, `ids`, `by` | bulk delete |
| `expired` | `ch`, `ids` | self-destruct sweep removed these |
| `join`, `part` | `ch`, `from` | presence |
| `kicked`, `banned` | `ch`, `from` (target), `by`, `text` (reason), `until` | sent to the room and to the target |
| `muted` | `ch`, `from`, `by`, `until` (0 = unmuted), `text` | |
| `role` | `ch`, `from`, `role`, `by` | role change |
| `topic` | `ch`, `from`, `text` | |
| `settings` | `ch`, `by`, `channels[0]` (`topic`, `readonly`, `expire`) | channel settings changed |
| `invited` | `ch`, `by`?, `channels[0]` | you were added to a channel — `sub` to it |
| `removed` | `ch`, `text`, `by`? | channel deleted, or you left from another device |
| `read` | `ch`, `id` | your other device read up to here |
| `clip` | `from`, `text` | clipboard from your other device |
| `quota` | `text` | you crossed 80 % / 100 % of storage |
| `err` with `code:"auth"`/`"banned"` (no `rid`) | | session revoked / banned mid-connection; the hub closes the socket |
| `pong` | | reply to `{"t":"ping"}` |

## 8. Error codes

`auth` `banned` `nochan` `perm` `ratelimit` `toolong` `muted` `readonly` `badarg` `limit`
`exists` `notfound` `guest`. Rule for implementers: anywhere the answer would differ based
on secret state (does the channel exist? is that a username?), return the same code **and
text**. `nochan` and file `404` follow this rule.

## 9. Compatibility

- v1 (`dropchan`) clients: `hello` without `v`, legacy `token`. Accepted only while
  `guest_access` is on; they live in the default channel and ignore frames they don't know.
- A v2 client talking to a v1 hub gets an `ok` without `v` and falls back to a single
  channel called `main`.
- Unknown frame types and fields must be ignored, not treated as errors.
