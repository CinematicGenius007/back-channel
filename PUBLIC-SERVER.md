# backchannel — Running a Public Hub

How to put a hub on the internet so people outside your LAN can join, and what protects it
once it's there. Day-to-day administration (roles, invites, moderation, limits) is in
ADMIN-GUIDE.md; this document is about the machine and the network.

---

## 0. Which option?

| You want | Use |
|---|---|
| Only *your own* devices, anywhere in the world | **Tailscale** (or any WireGuard/ZeroTier overlay). Install on each device, use the hub's `100.x.y.z` IP. Zero exposure to the internet, encrypted, no port forwarding. `-tls` not needed. |
| A group of people, permanent | **VPS + `bch serve -public`** — this document. |
| One-off session with someone | `cloudflared`/`ngrok` TCP tunnels. Fiddly (two ports, hostnames change each run). |

## 1. Security model — read this first

A public hub has four layers. Each stops a different attack.

**1. TLS with fingerprint pinning (`-tls` / `-public`).** The hub generates a self-signed
certificate on first start and prints its SHA-256 fingerprint. Clients pin that fingerprint —
either you give it to them (`-fingerprint`), or they accept it on first connect and it's
saved (trust-on-first-use, the SSH model). If anyone later sits between a client and the
hub with a different certificate, the client **refuses to connect** and shows both
fingerprints. No domain name or certificate authority is required; a bare IP is fine.
Everything — passwords at login, messages, files, clipboard — is encrypted in transit (TLS 1.3 only).

**2. Accounts.** Every person has their own username and password (PBKDF2-SHA256, 600k
rounds). Clients keep a revocable session token, never the password. Failed logins are
rate-limited **per username** (5/min) as well as per IP, so an attacker with many IPs still
cannot walk through one account. Removing a person is `/admin ban NAME` — nothing else changes.

**3. Unlisted channels.** You cannot see, join, or confirm the existence of a channel you
were not invited to. Invite codes have ~60 bits of entropy, limited uses and an expiry.

**4. Rate limits and quotas, per account.** What a member (or a stolen session) can do to
the server. See §4.

What this does **not** do: end-to-end encryption. The hub sees plaintext. If the hub
machine is compromised, so is everything on it.

## 2. Deploy on a VPS (Ubuntu/Debian, ~10 minutes)

Any $4–6/month VM works (Hetzner, DigitalOcean, Vultr, Linode, Oracle free tier).

```sh
# on the VPS
sudo mkdir -p /var/lib/backchannel
# upload dist/bch-linux-amd64 (scp), then:
sudo mv bch-linux-amd64 /usr/local/bin/bch && sudo chmod +x /usr/local/bin/bch
sudo useradd -r -m -d /var/lib/backchannel -s /usr/sbin/nologin bch

# first run, interactively, to see the owner password + fingerprint
sudo -u bch BCH_DIR=/var/lib/backchannel bch serve -public
```
You'll see:
```
  tls: true
  fingerprint: DC:CB:53:A5:…:98:57
  ★ owner account created —  username: owner   password: k3J9mQvT2xPb7n
  guest access: off (accounts only)
  (public) bch -hub tls://<YOUR-PUBLIC-IP>:7777 -user NAME -fingerprint DC:CB:…
  firewall: allow TCP 7777 and 7778 inbound
```
Write the password down, Ctrl-C, then install it as a service:

```sh
sudo tee /etc/systemd/system/backchannel.service >/dev/null <<'EOF'
[Unit]
Description=backchannel hub
After=network-online.target
Wants=network-online.target

[Service]
User=bch
Environment=BCH_DIR=/var/lib/backchannel
ExecStart=/usr/local/bin/bch serve -public
Restart=always
RestartSec=2
KillSignal=SIGINT
# hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/backchannel
PrivateTmp=yes
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now backchannel
sudo systemctl status backchannel        # running?
sudo journalctl -u backchannel -f        # live log
```
`KillSignal=SIGINT` matters: the hub saves state cleanly on Ctrl-C/SIGINT.

**Firewall** — open only the two TCP ports (discovery UDP 7779 is LAN-only and should stay closed):
```sh
sudo ufw allow 7777/tcp
sudo ufw allow 7778/tcp
sudo ufw enable
```
Also open them in the cloud provider's own firewall/security group if it has one.

**Connect from anywhere:**
```sh
bch -hub tls://203.0.113.10:7777 -user owner -fingerprint DC:CB:…
```
Change the owner password immediately (`/passwd`). After the first run, `bch` alone reconnects.

### Managing the service
```sh
sudo systemctl restart backchannel
sudo systemctl stop backchannel
sudo journalctl -u backchannel --since today
sudo -u bch BCH_DIR=/var/lib/backchannel bch serve -reset-owner    # while stopped: new owner password
```

### Where things live on the server
```
/var/lib/backchannel/hub/
  state.json        accounts (hashed passwords), channels, bans, invites, sessions (hashed), settings
  messages.jsonl    messages and file announcements
  audit.jsonl       who did what
  files/            uploads (+ .sha256 sidecars used for dedup)
  cert.pem key.pem  the TLS identity — back these up; losing them changes the fingerprint
```

## 3. Distributing access

Give each person three things: **address, fingerprint, and either an invite code or an
account**. Send them together through something already secure (Signal, in person, a
password-manager share). Without the fingerprint they can still connect and will pin
whatever they see first — fine if nobody is attacking that exact moment, but sending it
removes the guess.

```sh
# what you send (invite mode — they choose their own password):
bch -hub tls://203.0.113.10:7777 -invite k7Qz-9mPd2 -user THEIRNAME -fingerprint DC:CB:53:…
```
Invite codes come from `/invite` inside a channel (ADMIN-GUIDE.md §5). Windows users: same
command, `bch.exe` on PATH (WINDOWS-SETUP.md).

## 4. Limits — what `-public` sets and how to tune

Limits are keyed by **account** (guests by nick, connections by IP) and enforced by the
hub. `-public` on a hub's first start sets `msg_rate 3` and `upload_rate 12`; everything
else keeps the defaults. Change any of them live with `/admin set KEY VALUE` (owner) or
at startup with the matching flag.

| setting | default (`-public`) | stops |
|---|---|---|
| `max_msg_kb` | 64 | giant pastes filling everyone's scrollback |
| `max_file_mb` | 2048 | disk exhaustion by one upload |
| `quota_mb` | 5120 per user | disk exhaustion by one person over time |
| `msg_rate` (burst 20) | 3/s | message spam / scroll flooding; repeat offenders auto-muted |
| `upload_rate` | 12/min | "send 1000 files" scripts |
| `max_uploads` | 2 concurrent | holding slots open |
| `conn_rate` | 20/min per IP | brute force, reconnect storms |
| `login_rate` | 5 failed/min per username | password guessing across IPs |
| `invite_rate` | 10/hour per user | invite spraying |
| dedup | always | "the same file 1000 times" — stored once, then rate-limited |

Refused actions return a clear error to the sender and are never forwarded. Uploads are
refused **before** the bytes are sent (`Expect: 100-continue`), so a rejected 2 GB upload
costs nothing. Uploads slower than ~256 KB/s are dropped so a client can't hold a slot open forever.

Example for a chattier group with small files:
```
ExecStart=/usr/local/bin/bch serve -public -msg-rate 10 -max-file 200 -upload-rate 30
```

## 5. Operations

**Someone left / misbehaves:** `/admin ban NAME [days] [reason]`. Their connections drop
immediately and logins are refused. No secrets to rotate.

**A device was stolen:** the user runs `/sessions` and `/sessions revoke ID` from another
device, or `/logout all`; an admin can `/admin mod NAME pass=NEW` (revokes all their sessions).

**Prune old files:** `/admin prune` removes files no message references any more. Set
`/settings expire=30d` on busy channels so they clean themselves.

**Back up:** the whole `/var/lib/backchannel/hub` directory. `state.json` is rewritten
atomically, so a live copy is consistent. `cert.pem`/`key.pem` matter most — restoring
without them means every client sees a fingerprint change and must re-pin.

**Reinstall / new certificate:** tell users the new fingerprint; they reconnect with
`-fingerprint NEW` (the error they see tells them exactly this).

**Upgrade:** replace the binary, `systemctl restart backchannel`. Clients reconnect within
seconds and replay anything they missed. Sessions survive restarts.

**Monitor:** `curl -sk https://IP:7778/` (no auth) prints connection, channel and message
counts; `/admin stats` inside the app has the rest.

## 6. Home hosting instead of a VPS

Possible: `bch serve -public` on the Mac, forward TCP 7777–7778 on your router, use a
dynamic-DNS name. Same security properties. The trade-offs: your home IP takes inbound
traffic from whoever has the address, your upload bandwidth becomes the hub's download
bandwidth for everyone, and it's down when the Mac sleeps. Fine for a few trusted people;
a VPS is better for anything larger.

## 7. Threat checklist

| Threat | Covered by |
|---|---|
| Eavesdropping on the internet | TLS 1.3 |
| Certificate swap / MITM | fingerprint pin — client refuses |
| Password guessing | per-username failed-login limit + per-IP connection limit + 600k-round PBKDF2 |
| Stolen laptop | session tokens are revocable; passwords are never stored client-side |
| Message flood | `msg_rate`, `max_msg_kb`, auto-mute |
| File flood / disk fill | `upload_rate`, `max_file_mb`, `quota_mb`, dedup, upload speed floor |
| Slowloris / half-open connections | 10 s hello deadline, 15 s header deadline, keepalive, per-user connection cap |
| Malicious file names (`../../etc/passwd`) | names sanitized to a basename; files stored under a random id |
| Enumerating channels or users | identical errors for "doesn't exist" and "not yours"; no directory; unguessable invite codes |
| One member misbehaving | kick / ban / mute per channel; hub-wide ban; audit log |
| Hub operator reading messages | **not covered** — inherent to a relay with server-side history |
