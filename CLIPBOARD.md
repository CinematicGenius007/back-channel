# backchannel — Clipboard Sync

Copy on one of your machines, paste on another. This document is what it does, what it
deliberately does not do, how it works, and what it costs.

---

## 1. Using it

Three clipboard features exist; they are different things:

| Command | What it does | Who sees it |
|---|---|---|
| `/paste` | Posts your clipboard's text as a **message** in the active channel | everyone in the channel; stored in history |
| `/copy [#id]` | Puts a message's text into **your** clipboard | nobody |
| `/clip on` | **Sync**: from now on, whatever you copy on this device is placed into the clipboard of your *other* devices that also have sync on | only devices logged in as you; never stored |

Turn sync on where you want it:
```
/clip on        # on the Mac
/clip on        # on the Windows PC
```
Copy a URL on the Mac → about a second later Ctrl-V on Windows pastes it. The status bar
shows `📋 sync` while it is on, and a short note `📋 clipboard ← mac (42 B)` appears when
something arrives. The setting is remembered per device in `config.json`.

One-off variants, useful when you don't want it always on:
```
/clip push      # send my current clipboard to my other devices once
/clip pull      # apply the last clipboard my other devices sent me (shown in the status bar)
/clip off
/clip           # status
```

Only **text** is synced. Images and copied files are ignored (nothing is sent).

## 2. What it never does

- **Never goes to other people.** The hub relays a `clip` frame only to sessions whose
  account is yours. Guests are keyed by nick, so two guests with the same nick would share
  — get an account if that matters.
- **Never stored, never replayed.** `clip` frames are not written to `messages.jsonl`, are
  not part of history, and a device that was offline never receives old clipboards.
- **Never on by default.** Each device opts in with `/clip on`. A device with sync off
  keeps the last received value only in memory for `/clip pull` and shows a status-bar hint.
- **Never larger than a message.** The hub's `max_msg_kb` (default 64 KB) applies; bigger
  clipboards are skipped with a status-bar note. That keeps a 200 MB accidental copy from
  crossing the network every second.
- **Never a loop.** Each client remembers the last value it sent or applied and does not
  re-send it. Two devices with sync on settle immediately.

What it *cannot* protect against: the hub operator can read clipboard traffic like
everything else (it is TLS-protected on the wire with `-public`, plaintext inside the hub),
and anything you copy — passwords included — will land on your other device. Turn it off
before copying something you don't want to travel.

## 3. How it works

```
 Mac (sync on)                         hub                          Windows (sync on)
 ──────────────                        ───                          ─────────────────
 poll clipboard ─ changed? ─┐
                            ▼
 {"t":"clip","text":"…"} ───────────►  same account?  ───────────►  {"t":"clip","from":"mac","text":"…"}
                                       other sessions only          write to clipboard
                                       (not back to the sender)     remember value (don't echo)
```

Client side (`clipboard.go`):
1. A goroutine wakes every 0.5 s (Windows) or 1.5 s (macOS/Linux) **only while sync is on
   and the client is online**; otherwise it just sleeps.
2. On Windows it first calls `GetClipboardSequenceNumber()` — one cheap syscall that
   returns a counter the OS bumps on every clipboard write. Nothing is read unless the
   counter changed. macOS has an equivalent (`NSPasteboard.changeCount`) but it needs
   Objective-C, and `bch` has no cgo, so it reads `pbpaste` each poll.
3. A new, non-empty, not-too-large value that differs from the last one sent/applied is
   sent as one `clip` frame over the existing chat connection.

Hub side (`hub.go: handleClip`):
1. Size check against `max_msg_kb`, rate check against the message bucket (`clip/<user>`).
2. Fan-out to every other session with the same user id. Nothing is logged.

Receiving side: if sync is on, write to the clipboard and remember the value; otherwise
store it for `/clip pull` and show a status-bar hint.

## 4. Platform notes

| OS | Read | Write | Change detection | Notes |
|---|---|---|---|---|
| Windows | Win32 `GetClipboardData(CF_UNICODETEXT)` | `SetClipboardData` | `GetClipboardSequenceNumber` | Native, no process spawn, correct Unicode. Retries briefly if another app holds the clipboard. |
| macOS | `pbpaste` | `pbcopy` | none (poll + compare) | Always available. |
| Linux Wayland | `wl-paste -n` | `wl-copy` | none | Install `wl-clipboard`. |
| Linux X11 | `xclip -selection clipboard -o` | `xclip -selection clipboard` | none | Install `xclip`. Headless / SSH sessions have no clipboard; `/clip` reports it unavailable. |

Why v1's Windows path (`powershell Get-Clipboard` / `clip.exe`) was replaced: spawning
PowerShell costs ~200 ms per read, far too slow to poll, and `clip.exe` reads stdin in the
console code page, mangling non-ASCII text. The native calls fix both and also make
`/paste` and `/copy` instant.

## 5. Cost

Sync off: nothing (the goroutine sleeps). Sync on, Windows: one syscall every 0.5 s,
unmeasurable. Sync on, macOS/Linux: one `pbpaste`/`xclip` process every 1.5 s — a few
milliseconds of CPU each, roughly 0.2–0.5 % of one core. If that bothers you on a laptop,
leave sync off and use `/clip push` when you need it.

## 6. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Nothing arrives on the other device | Is sync on **there**? (`/clip` shows the state.) Are both devices logged in as the same user? Guests: same nick? |
| "clipboard busy" (Windows) | Another app held the clipboard for more than ~100 ms; the next poll will pick it up. |
| Works one way only | The receiving device probably has sync off and shows the status-bar hint; `/clip on` or `/clip pull` there. |
| "over the hub's 64 KB limit" | Copy less, or ask the owner to raise `max_msg_kb` (`/admin set max_msg_kb 256`). |
| Status shows `📋 sync` but nothing happens on Linux | No `xclip` / `wl-clipboard` installed, or no display (SSH). |
