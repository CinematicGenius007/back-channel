# Roadmap

What is built, what is next, and what has been considered and left out. Nothing here has
a date. Items move up when someone needs them, and an item in **Considered** is not a
promise.

The same list is on the project site. Keep the two in step: the data is the `roadmap`
array at the top of `site/src/pages/index.astro`.

## Shipped

| Area | What |
|---|---|
| Hub | Many channels on one port, accounts and revocable sessions, roles, invites, moderation, per-account limits, audit log, self-destruct channels |
| Files and clipboard | Drag-a-file transfer with dedup and quotas, opt-in clipboard sync between your own devices |
| Encryption | End-to-end encrypted channels: per-device X25519 identities, AES-256-GCM, hub-relayed key exchange, `/verify` with a verify screen |
| Public hub | TLS 1.3 with fingerprint pinning for internet-facing hubs |
| Interface | Full-screen terminal app with mouse and touch input, setup flow, Manage screens, themes. Tab completion, per-channel drafts, tappable file lines, find in channel, transfer progress, desktop notifications |
| Accounts on many devices | A channel created or joined on one device appears on your other connected devices without reconnecting |

## Next

| Item | Why it matters |
|---|---|
| Tagged release binaries | The README says "one binary", but today the only way to get it is `go build`. A workflow that builds the six targets (macOS, Windows, Linux; arm64 and amd64) on a version tag and attaches them to a GitHub release |
| Check macOS and Linux by hand | The test suite and cross-compilation cover them, but raw mode, focus reports and the `osascript` and `notify-send` notifications have only been run on Windows. The Windows toast was sent without error but has not been seen on screen |

## Considered

Ideas that came up and were set aside for now. Each would be a real piece of work, and
none is started.

| Idea | Note |
|---|---|
| Edit and reply to a message | Needs a way to reference a message id in a frame and a rule for what happens to history, including in e2e channels |
| Typing indicators | Cheap to send, but it adds idle traffic and the client is built to stay silent when idle |
| Image previews in the terminal | Depends on what the terminal supports (Kitty, iTerm2 and Sixel graphics differ) and needs a plain fallback |

## Not planned

| Item | Reason |
|---|---|
| Third-party Go modules | The project stays standard-library only. Changing that is a design decision, discussed in an issue first |
| A browser or cloud client | It is a terminal tool you host yourself |
| Turning on e2e for an existing channel | History that already passed through the hub in the clear cannot be protected afterwards, so `e2e` is chosen when a channel is created |

## Suggesting something

Open an issue with the problem you are trying to solve, not only the feature. See
[CONTRIBUTING.md](CONTRIBUTING.md) for how changes are made and checked.
