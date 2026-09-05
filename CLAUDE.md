# backchannel (bch) — project context for Claude

This file rebuilds context that was lost when the project moved from a claude.ai chat
into Claude Code. Read it before touching anything.

## What this is

`backchannel` (binary `bch`, formerly `dropchan`) is a private, self-hosted multi-channel
"IRC" for moving text, clipboard contents and files between machines — the owner's Mac ↔
Windows work PC first, a small trusted group second. One static Go binary, **stdlib only**
(no `net/http`, no third-party modules), ~6 MB, idles at a few MB RSS with zero CPU. Runs on
macOS, Windows and Linux. No cloud, no browser.

Repo layout (git has no commits yet — everything is untracked):

```
code/            the Go module `backchannel` (package main, flat, `go build -o bch .`, Go 1.24+)
code/service/    launchd plist + Windows startup shortcut script
code/dist/       build.sh output (gitignored)
builds/          hand-copied v1 `dropchan-*` binaries from before this session (stale)
README.md        overview, setup, everyday use, upgrade from v1        ← canonical docs live at repo root
ADMIN-GUIDE.md   running a hub for people: roles, invites, moderation, limits, self-destruct, audit
PUBLIC-SERVER.md internet-facing hub: TLS/pinning, VPS, systemd, firewall, threat model
PROTOCOL.md      wire protocol v2 + HTTP file API, for scripting
CLIPBOARD.md     clipboard sync: behaviour, guarantees, platform notes
WINDOWS-SETUP.md Windows client install
PHASE2-DESIGN.md the design spec; §15 = what the built code does differently and why
```

Owner: Ayush (GitHub `CinematicGenius007`). Builds/deploys themselves; wants **code +
thorough docs**, and to be taught the reasoning behind design choices.

## History (what was decided and why)

**Phase 1 (dropchan v1.0.0, before this session):** star topology, one hub; JSON lines over
TCP `:7777`; hand-rolled HTTP/1.1 for files on `:7778`; UDP discovery `:7779`; append-only
history with `since` replay; shared token; cooked-mode VT100 TUI; drag-a-file / outbox /
inbox; public-hub hardening (`serve -public`: TLS 1.3 + SSH-style fingerprint pinning,
per-IP token buckets, size caps, `Expect: 100-continue`, dedup by sha256).

**Phase 2 (this session, v2.0.0 — implemented, tested, documented):** many channels on one
port; accounts (PBKDF2) + revocable sessions; server roles owner/admin/user/guest and
channel roles owner/admin/mod/member/readonly; unlisted channels with the identical-error
rule; invites with sign-up; kick/ban/mute/del/purge/transfer; per-account limits at three
levels + quota + auto-mute + ref-counted files; audit log; v1 clients as guests; TUI channel
bar and per-channel scrollback; **clipboard sync** between a user's devices; **self-destruct
channels** (`expire`); admin password reset. Deviations from the spec are in
PHASE2-DESIGN.md §15 — read that before "fixing" something that looks off.

## Working rules for this repo

- **Stay stdlib-only.** Adding a module dependency is a design change; ask first.
- **Never scatter role checks.** Every privileged path calls `can()`/`canTarget()` from
  `perms.go`. Add a row to `perms_test.go` when adding an action.
- **Identical errors for "no such channel" and "not a member"** (`code:"nochan"`, and HTTP
  404 for files). Tests assert the bytes are equal. Don't add a code path that leaks existence.
- **Frames are flat and additive.** New JSON fields are fine; renaming/removing breaks old
  clients. Version `hello`, not individual frames. v1 clients must keep working as guests
  in the default channel while `guest_access` is on.
- **Never hash a password under `h.mu`.** PBKDF2 takes ~0.3 s; `authenticate` and
  `handleCmd` compute hashes before taking the lock.
- **Cooked-mode TUI.** No raw mode. The only exception is echo-off for password prompts
  (`stty -echo` / `SetConsoleMode`). Channel switching is via `/n`, `/p`, `/1`…`/9`,
  `/c NAME` — Alt/Ctrl combos are not reliably visible in cooked mode.
- **`clip` frames are never persisted or replayed** and go only to sessions of the same user.
- Docs are the deliverable as much as the code. When behaviour changes, update README.md
  and ADMIN-GUIDE.md / PROTOCOL.md / PUBLIC-SERVER.md / CLIPBOARD.md as relevant.
- Docs at the repo root are canonical; `code/` carries no copies.
- Build: `cd code && go build -o bch .`. Cross-compile: `./build.sh` → `code/dist/`
  (`OUT=../builds ./build.sh` to refresh `builds/`). Tests: `go test ./...` (~2 s; the
  suite starts real hubs on loopback ports). Also run `go test -race .` after concurrency changes.
- Windows quirks met here: the Bash tool truncates very long commands (write patch scripts
  to the scratchpad and run them); keep `messages.jsonl` handles closed in tests or
  `t.TempDir` cleanup fails; `go build` can take 1–2 min on first compile after big changes.

## Quick orientation to the code

| file | what |
|---|---|
| `proto.go` | `Msg` (the single wire type), `Auth`, `ChanInfo`, error codes, version/app names |
| `main.go` | subcommand dispatch, `Config`, shared helpers |
| `store.go` | `State` (users, channels, memberships, bans, invites, files, settings), atomic save, v1 migration, message log + compaction, audit log |
| `auth.go` | PBKDF2 hashing, session tokens, guest pseudo-ids |
| `perms.go` | rank model + `can()` / `canTarget()` / `canSend()` — **the** permission matrix |
| `limits.go` | token buckets, effective per-user/per-channel limits, auto-mute, `expireMessages` (15 s), hourly `sweep`, `parseExpire` |
| `hub.go` | `runHub` (flags → settings, banner), sessions, `authenticate`, `sub`/`msg`/`clip`/`read`, fan-out, presence, file ref-counting |
| `cmds.go` | every `cmd` handler + `applySetting` |
| `files.go` | HTTP upload/download with membership checks, dedup, quota, `Expect: 100-continue` |
| `discovery.go` | UDP LAN discovery, local IPs |
| `tls.go` | self-signed cert + fingerprint pinning |
| `http_min.go` | minimal HTTP/1.1 parser/writer |
| `client.go` | flags/config, pre-TUI login, connect loop, events, per-channel state (`line` has id/ts for expiry) |
| `client_cmds.go` | slash commands, password prompts, `/admin …` |
| `clipboard.go`, `clipboard_windows.go`, `clipboard_other.go` | clipboard sync loop; native Win32 vs pbpaste/xclip |
| `tui.go`, `term_*.go` | scroll-region TUI with channel bar; size, VT enable, echo toggle |
| `*_test.go` | permission matrix, end-to-end hub tests, expiry tests |

## Ideas the owner has floated but that are not built

End-to-end encrypted channels (PHASE2-DESIGN.md §11). Nothing else is pending.
