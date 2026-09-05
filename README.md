# backchannel (`bch`)

A private, self-hosted set of chat channels for your own machines and a small group of
people you trust. Type text, paste your clipboard, or drag a file into the terminal — it
shows up on every other device in that channel and lands in their `inbox` folder. Your
clipboard can follow you from your Mac to your Windows PC. Channels can self-destruct.

One static ~6 MB binary (Go, standard library only — not even `net/http`). Idles at a few
MB of RAM with zero CPU. Runs on macOS, Windows and Linux. No cloud, no browser, no
runtime to install. Formerly `dropchan`; v2 adds accounts, many channels on one port,
roles and moderation, per-account abuse limits, clipboard sync and self-destruct channels.

```
┌ mac ─────────────────────────────────────┐        ┌ windows work PC ─────────────────────────┐
│ $ bch serve                  (the hub)   │        │ > bch                                     │
│ $ bch -user ayush            (client)    │◄──────►│  10:42 ayush  #12 meeting is 3pm          │
│  10:42 ayush  #12 meeting is 3pm         │  TCP   │  10:43 ayush  #13 📎 spec.pdf (1.2 MB)     │
│  10:43 ayush  #13 📎 spec.pdf (1.2 MB)    │  7777  │  — saved ~\backchannel\inbox\design\spec… │
│  — 📋 clipboard synced (42 B)            │  HTTP  │  — 📋 clipboard ← mac (42 B)              │
│  1:#general  2:#design(1)  3:#ops        │  7778  │  1:#general  2:#design  3:#ops(3)         │
│  connected · ayush@hub · #design (owner) │        │  connected · ayush@hub · #design (owner)  │
│ > _                                      │        │ > _                                       │
└──────────────────────────────────────────┘        └───────────────────────────────────────────┘
```

## Documentation map

| Read | When you want to |
|---|---|
| this file | install, connect two machines, everyday use |
| [ADMIN-GUIDE.md](ADMIN-GUIDE.md) | run a hub for other people: roles, invites, moderation, limits, self-destruct channels, audit |
| [PUBLIC-SERVER.md](PUBLIC-SERVER.md) | put the hub on the internet safely (TLS, pinning, VPS, firewall, backups) |
| [CLIPBOARD.md](CLIPBOARD.md) | clipboard sync: how it works, what it never does, platform notes |
| [PROTOCOL.md](PROTOCOL.md) | script against the hub (JSON frames, HTTP upload/download, curl) |
| [WINDOWS-SETUP.md](WINDOWS-SETUP.md) | step-by-step Windows client install |
| [PHASE2-DESIGN.md](PHASE2-DESIGN.md) | the design spec and the reasoning behind it |

## Design in one table

| Concern | Decision | Why |
|---|---|---|
| Topology | One **hub** process (star). Clients keep a persistent TCP connection. | Persistence + history like an IRC bouncer; no NAT hole-punching; the hub is the single source of truth. |
| Channels | Many channels over **one port**; each connection has a subscription set. | A port identifies a process, not a conversation. One process, one TLS identity, one service file. |
| Chat protocol | Newline-delimited JSON over raw TCP (`:7777`), `TCP_NODELAY`. | Simplest thing that is instant. A message is one `write()`. |
| Files | Hand-rolled HTTP/1.1 (`PUT /up`, `GET /f/<id>` on `:7778`), streamed to disk, hashed on the way in. | Files move at line speed without blocking chat; `curl` works; skipping `net/http` keeps the binary small. |
| Accounts | Username + password (PBKDF2-SHA256, 600k rounds). Clients keep a **revocable session token**, never the password. | A stolen laptop leaks one session you can revoke, not the password. |
| Roles | Server: owner › admin › user › guest. Per channel: owner › admin › mod › member › readonly. One `can()` function decides everything. | Access control scattered through handlers is where chat systems leak. |
| Secrecy | Channels are unlisted. "No such channel" and "not a member" are the **same bytes**. Invite codes are the only way in. | You cannot confirm a channel exists unless you are in it. |
| Abuse control | Limits keyed by **account**, at three levels (server → channel → user): message rate, upload rate, storage quota, size caps, auto-mute for flooding. Identical files stored once. | Sending the same file 1000 times costs one copy on disk and then hits the upload rate limit. |
| History | Append-only `messages.jsonl`, replayed per channel from the last id a client saw. Deletions are tombstones. | Reconnect after Wi-Fi drop shows exactly the gap; offline clients learn about deletions. |
| Self-destruct | Per-channel `expire` (10m … weeks). Hub removes messages and their files on a 15 s sweep and tells clients, which also expire locally. | Short-lived rooms without anyone doing housekeeping. |
| Clipboard sync | `clip` frames relayed only to *your other devices*, never stored or replayed. Opt-in per device. | Clipboards hold passwords. |
| Storage | One `state.json` (atomic rename) + `messages.jsonl` + `audit.jsonl`. No database. | Stays stdlib-only, cross-compiles in one command, is `grep`-able. Fine for hundreds of users. |
| Public hub | `bch serve -public`: TLS 1.3 with a self-signed cert, clients **pin the fingerprint** (SSH-style). | No domain or CA needed; a MITM is detected. See PUBLIC-SERVER.md. |
| Discovery | UDP broadcast `:7779`. | Zero-config on the same LAN. |
| UI | VT100 scroll-region TUI in cooked mode; channel bar; `/1`…`/9` to switch. | Works in Terminal.app, iTerm, Windows Terminal, conhost, SSH with no terminal library. |

Data layout: `~/backchannel/{config.json, inbox/<channel>/, outbox/, outbox/<channel>/, outbox/sent/}` and on
the hub machine `~/backchannel/hub/{state.json, messages.jsonl, audit.jsonl, files/, cert.pem, key.pem}`.
Override with `BCH_DIR` or `-dir`.

## Build

```sh
# needs Go 1.24+ (brew install go / winget install GoLang.Go)
cd code
go build -o bch .        # for this machine
./build.sh               # → dist/bch-darwin-arm64, -darwin-amd64, -windows-amd64.exe, -linux-amd64, …
go test ./...            # 20 tests: permissions matrix, secrecy rule, moderation, files, expiry, guests
```

## Setup (5 minutes)

**1. Start the hub** on the machine that is always on (your Mac, or a VPS — see PUBLIC-SERVER.md):
```sh
cp dist/bch-darwin-arm64 /usr/local/bin/bch && chmod +x /usr/local/bin/bch
bch serve
```
```
backchannel hub 2.0.0
  chat :7777   files :7778   discovery udp :7779   tls: false
  ★ owner account created —  username: owner   password: k3J9mQvT2xPb7n
    (shown once; change it with /passwd inside the app; recover with `bch serve -reset-owner`)
  guest access: off (accounts only)
  connect with:
    bch -hub 192.168.1.20:7777 -user owner
```
Leave it running (or install it as a service, below).

**2. Connect from the same machine** in a second tab:
```sh
bch -hub 192.168.1.20 -user owner
password for owner@192.168.1.20: ********
```
You are in `#general`. Change the password now: `/passwd`.

**3. Connect from the Windows PC** (details in WINDOWS-SETUP.md):
```powershell
bch -hub 192.168.1.20 -user owner -name work
```
Same account, second device. After the first run just type `bch` — the session is
remembered in `config.json`. Both devices see the same channels and the same unread counts.

**4. Bring in another person** (from inside the app, in the channel you want them in):
```
/invite
— invite code:  k7Qz-9mPd2   (#general · uses: 1 use · expires: 3d · role: member)
—   new person:      bch -hub <hub address> -invite k7Qz-9mPd2 -user THEIR_NAME
—   existing member: /join k7Qz-9mPd2
```
Send them the hub address and the code. They pick their own username and password.
Everything about running a hub for other people — roles, bans, limits, settings — is in
**ADMIN-GUIDE.md**.

## Using it

| Action | How |
|---|---|
| Send text | type, Enter |
| Send a file | drag it into the terminal window, Enter — or `/send C:\path\file.png` — or drop it in `~/backchannel/outbox` (→ active channel) or `~/backchannel/outbox/<channel>/` |
| Fetch a file that didn't auto-download | `/get 13` (or `/get` for the latest in this channel) |
| Open this channel's inbox folder | `/open` |
| Switch channel | `/1` … `/9`, `/n` / `/p`, `/c design`, or just `/design` |
| List your channels | `/channels` |
| Join with an invite code | `/join k7Qz-9mPd2` |
| Who is online here | `/who` · all members and roles: `/members` |
| Clipboard → channel | `/paste` |
| Message → clipboard | `/copy 12` (or `/copy` for the latest) |
| Keep clipboards in sync across my devices | `/clip on` on each device (see below) |
| Mention someone | `@name` — rings their terminal bell even in a background channel |
| Delete my message | `/del 12` |
| Quieter | `/notify off` |
| Leave | `/quit` or Ctrl-C |
| Everything | `/help` |

The **channel bar** shows unread counts: `1:#general  2:#design(3)  3:#ops`. Files
auto-download (≤ 50 MB, `-auto N` to change) in the channel you are looking at; for a channel
you want to receive files from in the background, `/watch design`.

### Clipboard sync

```
/clip on          # on each of your devices
```
Copy something on the Mac; a moment later it is in the Windows clipboard, and vice versa.
It only ever goes to devices logged in as **you**, the hub never stores it, and it is off
until you turn it on. `/clip push` sends the current clipboard once without turning sync
on; `/clip pull` applies the last one received. Text only. Full details, including what
it costs and how loop-prevention works, in **CLIPBOARD.md**.

### Self-destruct channels

```
/create burn a room that forgets expire=1h
/settings expire=10m        # in an existing channel (channel admin+)
```
Messages older than the expiry disappear from the hub (and their files from the hub's
disk) on a 15-second sweep, and from every open client — even offline ones, which expire
locally by timestamp. Files the app auto-downloaded from such a channel are removed from
your inbox when their message expires; anything you moved elsewhere is yours. The status
bar shows `🔥 10m` while you are in one. Values: `10m`, `2h`, `7d`, `1w`, `off`.

### From scripts / other terminals (no TUI)

```sh
bch send "deploy is done"                # text → your last active channel
bch send -ch ops ~/Desktop/shot.png      # file → #ops
pbpaste | bch send -ch notes             # stdin → channel
bch discover                             # list hubs on this LAN
```
The first `bch send` on a machine asks for your password once; the session is saved.

## Public hub (people outside your network)

```sh
bch serve -public          # TLS + fingerprint pinning + stricter rate limits
#   fingerprint: DC:CB:53:…
#   (public) bch -hub tls://<YOUR-PUBLIC-IP>:7777 -user NAME -fingerprint DC:CB:53:…
```
Clients connect with `tls://` and ideally `-fingerprint`; without it the first certificate
seen is pinned and any later change is refused. VPS deployment, systemd unit, firewall,
backups and the threat model are in **PUBLIC-SERVER.md**.

## Different networks (home ↔ office)

The hub needs to be reachable from both machines. Pick one:

1. **Tailscale (recommended for your own devices).** Install on both machines, log in. Use
   the hub's Tailscale IP: `bch -hub 100.101.102.103 -user you`. WireGuard-encrypted,
   direct peer-to-peer, works through corporate NAT with no port forwarding. Any similar
   overlay (ZeroTier, NetBird, WireGuard) works the same way.
2. **Tiny VPS with `-public`.** Run `bch serve -public` on a $4 VM, open TCP 7777–7778.
   TLS + pinning + accounts make this safe for a group. See PUBLIC-SERVER.md.
3. **SSH tunnel** if the office allows outbound SSH to a home box:
   `ssh -N -L 7777:localhost:7777 -L 7778:localhost:7778 user@home` then `-hub 127.0.0.1`.

The client reconnects automatically with backoff and replays anything it missed, so
lid-close / VPN flap / office Wi-Fi are all fine.

## Run it as a service

**macOS hub (launchd):** edit the path in `code/service/com.backchannel.hub.plist`, then
```sh
cp code/service/com.backchannel.hub.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.backchannel.hub.plist
```
**Linux / VPS:** systemd unit in PUBLIC-SERVER.md.

**Windows client always open at logon:** `code\service\install-windows-startup.cmd` creates a
Start-menu Startup shortcut that opens `bch` in Windows Terminal.

## Upgrading from dropchan v1

1. Rename the data directory: `mv ~/dropchan ~/backchannel` (or pass `-dir ~/dropchan/hub`).
2. Start `bch serve` in it. It finds `history.jsonl` and `token`, imports the history into
   `#general`, turns **guest access on** with the old token so v1 clients keep working, and
   prints the new owner password.
3. Give people accounts (`/admin useradd` or `/invite`) and then `/admin set guest_access off`.

## Footprint

| | Size / usage |
|---|---|
| binary | ~6 MB, static, stripped, stdlib only (TLS included) |
| hub idle | a few MB RSS, 0 CPU; one goroutine per connection; state is a few hundred KB of JSON |
| client idle | a few MB RSS, 0 CPU; outbox polled with a cheap `stat()` once a second; clipboard polled only while `/clip on` (Windows: one syscall / 0.5 s; macOS/Linux: `pbpaste` / 1.5 s) |

Message latency on a LAN is one TCP round-trip; files move at whatever the link does.

## Ports / firewall

| Port | Proto | Purpose |
|---|---|---|
| 7777 | TCP | chat (JSON lines) |
| 7778 | TCP | files (HTTP) |
| 7779 | UDP | LAN discovery (keep closed on a public hub) |

`bch serve -port 9000` shifts all three (9000/9001/9002).

## Limits and honest caveats

- Transport encryption yes (`-tls`); end-to-end no — **the hub operator can read
  everything**. End-to-end channels are sketched in PHASE2-DESIGN.md §11 and not built.
- The hub knows who is online, who is in which channel, and when. That is inherent to a
  relay with server-side history.
- Self-destruct removes messages from the hub and from `bch` clients. It cannot remove
  what someone copied elsewhere or a file they moved out of the inbox.
- History replays the last 2000 messages per channel (`-history N`). Files without any
  referencing message are removed by `/admin prune`.
