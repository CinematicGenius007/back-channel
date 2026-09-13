# backchannel (bch) — project context for Claude

This file rebuilds context that was lost across chat-to-Claude-Code and session
transitions. Read it before touching anything.

## What this is

`backchannel` (binary `bch`, formerly `dropchan`) is a private, self-hosted multi-channel
"IRC" for moving text, clipboard contents and files between machines — the owner's Mac ↔
Windows work PC first, a small trusted group second. One static Go binary, **stdlib only**
(no `net/http`, no third-party modules), ~6 MB, idles at a few MB RSS with zero CPU. Runs
on macOS, Windows and Linux. No cloud, no browser.

Repo layout. Git has one commit on `main` (the Phase 2 work); **no remote is configured**
— nothing has been pushed anywhere, and there was nothing to push to when last asked.

```
code/            the Go module `backchannel` (package main, flat, `go build -o bch .`, Go 1.24+)
code/service/    launchd plist + Windows startup shortcut script
code/dist/       build.sh output (gitignored)
README.md        overview, setup, everyday use, upgrade from v1        ← canonical docs live at repo root
ADMIN-GUIDE.md   running a hub for people: roles, invites, moderation, limits, self-destruct, e2e, audit
PUBLIC-SERVER.md internet-facing hub: TLS/pinning, VPS, systemd, firewall, threat model
PROTOCOL.md      wire protocol v2 + HTTP file API, for scripting
CLIPBOARD.md     clipboard sync: behaviour, guarantees, platform notes
ENCRYPTION.md    end-to-end encrypted channels: design, key exchange, threat model
WINDOWS-SETUP.md Windows client install
PHASE2-DESIGN.md the design spec; §15 = what the built code does differently and why
```

Owner: Ayush (GitHub `CinematicGenius007`). Builds/deploys themselves; wants **code +
thorough docs**, and to be taught the reasoning behind design choices. Also asked, when
reviewing documentation, for it to read as **plain and precise, not AI-generated filler**
— cut hedging, cut restated summaries, keep concrete detail over vague reassurance.

## History (what was decided and why)

**Phase 1 (dropchan v1.0.0):** star topology, one hub; JSON lines over TCP `:7777`;
hand-rolled HTTP/1.1 for files on `:7778`; UDP discovery `:7779`; append-only history with
`since` replay; shared token; cooked-mode VT100 TUI; drag-a-file / outbox / inbox;
public-hub hardening (`serve -public`: TLS 1.3 + SSH-style fingerprint pinning, per-IP
token buckets, size caps, `Expect: 100-continue`, dedup by sha256).

**Phase 2 (v2.0.0):** many channels on one port; accounts (PBKDF2) + revocable sessions;
server roles owner/admin/user/guest and channel roles owner/admin/mod/member/readonly;
unlisted channels with the identical-error rule; invites with sign-up; kick/ban/mute/
del/purge/transfer; per-account limits at three levels + quota + auto-mute + ref-counted
files; audit log; v1 clients as guests; TUI channel bar and per-channel scrollback;
clipboard sync between a user's devices; self-destruct channels (`expire`); admin password
reset. Committed as the repo's only commit so far. Deviations from the original spec are
in PHASE2-DESIGN.md §15 — read that before "fixing" something that looks off.

**Phase 3 (this session): end-to-end encrypted channels, built.** `/create NAME e2e=on`.
Each device has its own X25519 identity key pair; a channel's AES-256 key is generated
client-side and handed to other devices by ECDH-wrapping it to their public key and
relaying the wrapped bytes through the hub, which never sees plaintext or the key itself.
`keyreq`/`keyshare` are live-relay-only frames (like `clip`) — never persisted, never
replayed, delivered only between two currently-connected devices. `/e2e status|rotate|
reshare`, `/verify NAME`. Kick/ban in an e2e channel auto-rotates the key from whichever
device performed it, if it holds the key. Full design and honest limitations in
ENCRYPTION.md; deviations from the original PHASE2-DESIGN.md §11 sketch are appended to
its §15 table. This phase also did a documentation pass across every doc in the repo for
tone and precision, and deleted a stale `builds/` directory of pre-Phase-2 binaries.

## Working rules for this repo

- **Stay stdlib-only.** Adding a module dependency is a design change; ask first. E2E
  crypto stayed inside this rule: `crypto/ecdh` (X25519, Go 1.20+) and `crypto/aes` +
  `crypto/cipher` (AES-256-GCM) cover it without `golang.org/x/crypto`.
- **Never scatter role checks.** Every privileged path calls `can()`/`canTarget()` from
  `perms.go`. Add a row to `perms_test.go` when adding an action.
- **Identical errors for "no such channel" and "not a member"** (`code:"nochan"`, and HTTP
  404 for files). Tests assert the bytes are equal. Don't add a code path that leaks
  existence — this now also covers `keyshare`/`keyreq`: both silently no-op rather than
  telling a non-member anything.
- **Frames are flat and additive.** New JSON fields are fine; renaming/removing breaks old
  clients. Version `hello`, not individual frames. v1 clients must keep working as guests
  in the default channel while `guest_access` is on.
- **Never hash a password under `h.mu`.** PBKDF2 takes ~0.3 s; `authenticate` and
  `handleCmd` compute hashes before taking the lock.
- **Cooked-mode TUI.** No raw mode. The only exception is echo-off for password prompts
  (`stty -echo` / `SetConsoleMode`). Channel switching is via `/n`, `/p`, `/1`…`/9`,
  `/c NAME` — Alt/Ctrl combos are not reliably visible in cooked mode.
- **`clip`, `keyreq` and `keyshare` frames are never persisted or replayed.** `clip` goes
  only to the same user's other sessions; `keyshare` goes only to the one (user, device)
  it names; `keyreq` broadcasts to a channel's other current subscribers. The hub is a
  dumb relay for all three — it decides only *whether* to relay (membership), never what
  the payload means.
- **E2E identity is per device, not per account.** A private key never needs to leave the
  machine that generated it; a second device for the same person gets its own identity
  and asks for whatever channel keys it needs, the same as any other new device would.
- **`e2e` is create-time-only**, never a `/settings` toggle — there is no way to
  retroactively protect history that already passed through the hub in the clear, so
  pretending to would be dishonest.
- Docs are the deliverable as much as the code. When behaviour changes, update README.md
  and ADMIN-GUIDE.md / PROTOCOL.md / PUBLIC-SERVER.md / CLIPBOARD.md / ENCRYPTION.md as
  relevant, including cross-references and section numbers in the ones that number
  sections (PROTOCOL.md, ADMIN-GUIDE.md) — check for stale `§N` references after
  inserting or removing a section.
- Docs at the repo root are canonical; `code/` carries no copies.
- Build: `cd code && go build -o bch .`. Cross-compile: `./build.sh` → `code/dist/`
  (`OUT=../builds ./build.sh` to refresh a `builds/` directory, if the user wants one
  back — there isn't one right now). Tests: `go test ./...` (a few seconds; the suite
  starts real hubs on loopback ports). Also run `go test -race .` after concurrency
  changes, and cross-compile all six platform/arch pairs after touching anything crypto-
  or syscall-adjacent (`crypto/ecdh` and the Win32 clipboard calls are the two things
  most likely to behave differently across `GOOS`/`GOARCH`).
- Windows quirks met here: the Bash tool truncates very long inline commands and the Edit
  tool can spuriously fail to match content it just read back correctly (workaround: use
  small Python patch scripts with `str.count(old) == 1` assertions instead, written to
  the scratchpad, run, then deleted — this fully replaced heredoc-based `sed`/`python -c`
  approaches after they proved unreliable for large edits); keep `messages.jsonl` file
  handles closed in tests or `t.TempDir` cleanup fails; `go build` can take 1–2 min on
  first compile after big changes; stray `bash.exe.stackdump` files land in `code/` from
  time to time and are not repo content — delete them.

## Quick orientation to the code

| file | what |
|---|---|
| `proto.go` | `Msg` (the single wire type), `Auth`, `ChanInfo`, `Peer`, error codes, version/app names |
| `main.go` | subcommand dispatch, `Config`, shared helpers |
| `store.go` | `State` (users, channels, memberships, bans, invites, files, device keys, settings), atomic save, v1 migration, message log + compaction, audit log |
| `auth.go` | PBKDF2 hashing, session tokens, guest pseudo-ids |
| `perms.go` | rank model + `can()` / `canTarget()` / `canSend()` — **the** permission matrix |
| `limits.go` | token buckets, effective per-user/per-channel limits, auto-mute, `expireMessages` (15 s), hourly `sweep`, `parseExpire` |
| `hub.go` | `runHub` (flags → settings, banner), sessions, `authenticate` (registers a device's E2E pub key on success), `sub`/`msg`/`clip`/`read`/`keyreq`/`keyshare`, fan-out, presence, file ref-counting |
| `cmds.go` | every `cmd` handler + `applySetting`, including `e2erotate`/`e2epeers` |
| `files.go` | HTTP upload/download with membership checks, dedup, quota, `Expect: 100-continue`, the `epoch` query param for e2e uploads |
| `discovery.go` | UDP LAN discovery, local IPs |
| `tls.go` | self-signed cert + fingerprint pinning (for the transport; unrelated to e2e channel crypto) |
| `http_min.go` | minimal HTTP/1.1 parser/writer |
| `e2e.go` | the crypto primitives: X25519 device keys, AES-256-GCM seal/open, key wrap/unwrap, fingerprints — stateless, hub- and client-agnostic |
| `e2e_client.go` | client-side orchestration: device key persistence, `chanKey`/`storeChanKey`, `maybeRequestKey`, `onKeyReq`/`onKeyShare`, encrypt-for-send, rotate/reshare, `/e2e` and `/verify` command bodies |
| `client.go` | flags/config, pre-TUI login, connect loop, events, per-channel state (`chanState` carries `e2e`/`epoch`/`pendingEnc` alongside the self-destruct `expire`), `renderMsg` (decrypts when needed, used both live and to flush held-back e2e messages) |
| `client_cmds.go` | slash commands, password prompts, `/admin …`, `/e2e`/`/verify` dispatch, kick/ban wired to `afterModeration` |
| `clipboard.go`, `clipboard_windows.go`, `clipboard_other.go` | clipboard sync loop; native Win32 vs pbpaste/xclip |
| `tui.go`, `term_*.go` | scroll-region TUI with channel bar; size, VT enable, echo toggle |
| `*_test.go` | permission matrix, end-to-end hub tests, expiry tests, e2e crypto + full hub-relayed key exchange |

## Ideas the owner has floated but that are not built

Nothing is currently pending. If a new idea comes up mid-session, add it here with
enough context to resume cold, the way this file's Phase 3 entry does.
