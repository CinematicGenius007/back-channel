# backchannel — End-to-End Encrypted Channels

What `e2e=on` actually buys you, how it works, and what it does not protect against.
Read this before you rely on it for anything sensitive.

---

## 1. The property

In an ordinary channel, the hub sees everything: message text, file contents, file
names. Transport is encrypted (`-tls`), but the hub operator — or anyone who compromises
the hub — can read it all.

In an end-to-end (e2e) channel, the hub sees only ciphertext. Messages, file contents,
and file names are sealed on the sending device and opened only on receiving devices
that hold the channel's key. The hub relays bytes it cannot read, stores bytes it cannot
read, and has no way to get the key: it is never sent to the hub, not even encrypted.

What the hub still learns, because a relay with server-side history inherently learns
it: who is in the channel, who is online, roughly how large each message or file is
(ciphertext is the plaintext size plus 28 bytes), and when everything happened. If that
metadata matters to you, it is not hidden by this feature — see §7.

## 2. Using it

```
/create incident-room topic here e2e=on
```
`e2e` can only be set at creation. There is no `/settings e2e=on` for an existing
channel, because the hub has no key to retroactively protect history that already
passed through it in the clear — starting fresh is the only honest option.

Bring people in the normal way:
```
/invite
/add NAME
```
A newly joined device does not have the key yet. It asks for it automatically; any
other device that already holds it and is online answers automatically. You'll see:
```
— 🔒 waiting for the channel key — /e2e status
— 🔓 channel key received for #incident-room
```
usually within a second, since the request-and-answer happens over the same connection
you're already on. If nobody who holds the key is online, nothing bad happens — the
channel just stays locked for you until someone is.

```
/e2e status      what epoch, do I have the key, my device's fingerprint
/e2e rotate      generate a new key and hand it to everyone online now (mod+)
/e2e reshare     re-send the *current* key to everyone online (no new epoch)
/verify NAME     show a member's device fingerprint(s), for comparing out of band
```
`/e2e rotate` runs automatically after `/kick` or `/ban` in an e2e channel, from
whichever device performed it — see §5.

One-shot `bch send` works too, but only from a device that has already opened the
channel at least once in the interactive app (so it has a saved key). Otherwise it
refuses outright rather than silently sending plaintext:
```
error: #incident-room is end-to-end encrypted and this device has no saved key for it —
open it once in the bch TUI so it can receive the key, then try again
```

## 3. Trust model: device identities, not accounts

Every **device**, not every account, has its own long-term key pair (X25519), generated
the first time `bch` runs and kept in that device's `config.json` next to its session
token — protected the same way, by file permissions on that machine, not by a
passphrase. Logging in as the same person on a second computer does not carry the
private key over; that computer generates its own identity and asks for the channel
keys it needs, the same way any new member's device would.

This means:
- **Losing a device** means whatever channel keys were on it are gone with it, along
  with everything else on that machine (session token, clipboard state). Rotate the
  e2e channels it had access to (§5) if that matters.
- **A stolen laptop** that was logged in can still read the channels it had keys for,
  same as it could read your other sessions — the account being remote-logged-out
  (`/logout all`, or an admin ban) stops it from talking to the hub again, but does not
  reach into local storage. Rotate affected channels.
- **`/verify NAME`** exists for the same reason SSH and this project's own TLS pinning
  show a fingerprint: a public key is safe to publish, but *which* public key actually
  belongs to your colleague's laptop is a question only you and them can answer, over a
  channel you already trust (in person, a call, a Signal message). The hub could in
  principle hand a requester a forged directory entry: nothing here defends against that
  by itself, only mutual verification does.

## 4. How the key gets from one device to another

```
 device B (needs the key)                hub                    device A (has the key)
 ─────────────────────────                ───                     ──────────────────────
 {"t":"keyreq","ch":…,"epoch":1} ───► relay to every other device ───► {"t":"keyreq", from_user, from_dev, from_pub}
                                       currently subscribed to #ch
                                                                        wrap key to from_pub (ECDH + AES-256-GCM)
 {"t":"keyshare", to_user, to_dev, ◄─── relay only to that ◄────── {"t":"keyshare", ch, epoch, from_pub, wrapped}
  from_pub, wrapped}                    (user, device)
 unwrap with own priv key + from_pub
```
- **ECDH** (Elliptic-Curve Diffie-Hellman over Curve25519, `crypto/ecdh` in the Go
  standard library) lets device A and device B derive the same shared secret from A's
  private key + B's public key, or B's private key + A's public key — without either
  ever transmitting a private key. That shared secret, hashed together with the channel
  name, becomes a one-time wrapping key.
- The 32-byte channel key is sealed under that wrapping key with **AES-256-GCM**
  (authenticated encryption: tampering is detected, not silently accepted).
- The hub's part is exactly two `if` statements: relay a `keyreq` to other subscribers
  of that channel, relay a `keyshare` to the one specific (user, device) it names. It
  never touches the plaintext key, the wrapped bytes, or the shared secret. It does
  enforce that both the requester and the person answering are *currently members* of
  the channel — someone kicked or never invited gets nothing, even if they still know
  the channel's name.
- Key delivery only ever happens between two **currently connected** devices. There is
  no offline mailbox for it on the hub — that's a deliberate choice: it means a
  compromised hub disk can never contain even a wrapped copy of a channel key. The
  practical effect is that a brand-new device has to wait for someone who already holds
  the key to be online at the same time, which for a small, mostly-overlapping group of
  people is rarely more than a few minutes.

## 5. Rotation

A channel key never expires on its own, but anyone mod+ can force a new one:
```
/e2e rotate
```
This bumps the channel's epoch (a plain integer the hub does track — it's not secret,
just a counter) and the rotating device generates a fresh 32-byte key and shares it with
everyone currently online, the same way as a normal keyshare. Devices that are offline
pick it up automatically via `keyreq` the next time they open the channel.

Old messages stay under the old key — rotation is forward-only. That is inherent to any
system without re-encrypting history (which would mean trusting the new epoch with
plaintext of everything that came before, defeating the point): if someone had a key,
they can still read what they could already read. What rotation buys you is that they
stop being able to read *anything new*.

**Kick or ban in an e2e channel rotates automatically**, from whichever device performed
the action, if that device holds the current key:
```
— 🔒 #incident-room rotated to epoch 4 — sharing the new key with everyone online
```
If the device that kicked someone didn't hold the key itself (e.g. a server admin
moderating a channel they don't normally read), you'll see a nudge instead:
```
— ⚠ #incident-room is end-to-end encrypted — its key should be rotated (/e2e rotate) by
  a device that holds it
```
Rotating manually from any device that does hold it closes the gap.

## 6. Messages and files, precisely

- **Text**: `AES-256-GCM(key, nonce, plaintext)`, nonce random per message, associated
  data = channel name + epoch (so ciphertext from one channel or epoch cannot be
  replayed into another and still verify). Stored on the wire as base64; the ~33%
  base64 overhead plus the 28-byte nonce+tag counts against the hub's per-message size
  limit, so very long messages in an e2e channel hit that ceiling slightly sooner than
  in a plain one.
- **Files**: the same AES-256-GCM construction over the whole file in one seal, plus the
  file name sealed the same way as a short text message. **The whole file is held in
  memory on both ends** while this happens — simpler and still fully correct (AES-GCM's
  safe limit for one seal is far beyond anything this tool's `max_file_mb` allows), at
  the cost of not streaming a huge encrypted upload byte-by-byte the way a plain-channel
  upload does. If multi-gigabyte files in encrypted channels become common, a chunked
  streaming cipher is the natural next step; it wasn't worth the extra complexity for a
  first version.
- **Deduplication does not cross plaintexts** in an e2e channel: two different uploads
  of the same file get different random nonces and therefore different ciphertext, so
  the hub's content-hash dedup (which hashes whatever bytes it receives, same as
  always) does not recognize them as the same file. Each copy uses its own disk space
  and quota. This is the accepted trade-off from the original design notes
  (`PHASE2-DESIGN.md` §11) for not adopting convergent encryption, which would let the
  hub confirm two files are identical — a small information leak in exchange for dedup
  that this version does not take.

## 7. What this does not protect against

- **Metadata.** Who is in the channel, who is online, message/file sizes and timing are
  all visible to the hub regardless. If the existence of the conversation matters more
  than its contents, e2e does not hide that.
- **A compromised device.** If a device is compromised while it holds the key —
  malware, physical access, a person you gave access who turns out not to deserve it —
  everything sent while they had it is exposed to them, going forward until you rotate,
  and everything from before whenever they choose to look. This is the same limitation
  every end-to-end system has; there is no cryptography that protects data from a device
  that is allowed to decrypt it.
- **Server admins.** They outrank every channel's own membership by design (ADMIN-GUIDE.md
  §3) — that applies to e2e channels too. An admin can join, appear as a valid peer, and
  send a `keyreq` that another member's device answers exactly as it would for anyone
  else's. There is no carve-out that keeps an e2e channel private from the hub's own
  admins; who you make an admin is the actual control here.
- **A forged directory entry.** Nothing stops a compromised hub from handing a joining
  device a public key it controls instead of the real member's, unless someone actually
  runs `/verify` and compares fingerprints out of band. The feature makes this attack
  detectable; it does not make it impossible without that step, exactly like TLS
  certificate pinning in `PUBLIC-SERVER.md`.
- **Deniability, forward secrecy of the wrap itself, multi-device key sync without
  someone online.** All real limitations of this design, called out honestly rather
  than glossed over. A system like Signal solves harder versions of some of these with
  substantially more protocol machinery; this is deliberately the simpler thing that
  covers the actual threat this project's owner cares about — the hub operator (or
  whoever compromises the hub) not being able to read a specific room's contents.

## 8. Reference: frames and fields

See `PROTOCOL.md` §3a for the wire-level detail. In short: `hello` carries a device's
public key (`pub`); a channel's `e2e` and `epoch` fields ride along on every `ChanInfo`;
`keyreq`/`keyshare` are the two new frame types, both un-persisted and relayed live only;
`msg`/`file` frames in an e2e channel carry an `epoch` alongside their (now opaque)
`text`/`name`, so a recipient knows which of its saved keys to try.
