package main

// Client-side orchestration for end-to-end encrypted channels. The crypto primitives
// live in e2e.go; this file is what the client does with them: keep a device identity,
// notice when a channel needs a key it doesn't have, ask for one, answer others' asks,
// seal outgoing messages and files, open incoming ones, and rotate a channel's key.
// See ENCRYPTION.md for the full picture.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ensureDeviceKey makes sure this device has a persisted X25519 identity, generating
// one on first use. Safe to call every run; it's a no-op once a key exists. Runs before
// the TUI is up, so problems go to stderr rather than the chat window.
func (c *client) ensureDeviceKey() {
	cfg := c.snapshot()
	if cfg.E2EPriv != "" {
		if priv, err := parsePriv(cfg.E2EPriv); err == nil {
			c.e2ePriv = priv
			return
		}
		// stored key is unparseable (corrupt config?): fall through and regenerate
	}
	priv, err := genDeviceKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not generate an E2E device key:", err)
		return
	}
	c.e2ePriv = priv
	c.update(func(cf *Config) {
		cf.E2EPriv = encodeKey(priv.Bytes())
		cf.E2EPub = pubFromPriv(priv)
	})
}

// chanKey looks up the key this device holds for one channel epoch, if any.
func (c *client) chanKey(chName string, epoch int) ([]byte, bool) {
	cfg := c.snapshot()
	if cfg.E2EKeys == nil {
		return nil, false
	}
	byEpoch, ok := cfg.E2EKeys[chName]
	if !ok {
		return nil, false
	}
	b64, ok := byEpoch[strconv.Itoa(epoch)]
	if !ok {
		return nil, false
	}
	key, err := decodeKey(b64)
	if err != nil {
		return nil, false
	}
	return key, true
}

// storeChanKey persists a key this device now holds for one channel epoch.
func (c *client) storeChanKey(chName string, epoch int, key []byte) {
	c.update(func(cf *Config) {
		if cf.E2EKeys == nil {
			cf.E2EKeys = map[string]map[string]string{}
		}
		if cf.E2EKeys[chName] == nil {
			cf.E2EKeys[chName] = map[string]string{}
		}
		cf.E2EKeys[chName][strconv.Itoa(epoch)] = encodeKey(key)
	})
}

func (c *client) initCreatedE2EKeys(channels []ChanInfo) {
	for _, ci := range channels {
		if !ci.E2E || ci.Epoch == 0 {
			continue
		}
		key, err := newChannelKey()
		if err != nil {
			c.sys("e2e: could not generate the initial channel key: " + err.Error())
			continue
		}
		c.storeChanKey(ci.Name, ci.Epoch, key)
	}
}

// maybeRequestKey sends one keyreq for ch's current epoch if this channel is e2e, we
// don't already hold that epoch's key, and we haven't already asked. Safe to call
// repeatedly (e.g. every time ChanInfo arrives, or someone new joins the channel).
func (c *client) maybeRequestKey(ch *chanState) {
	if !ch.e2e || ch.epoch == 0 {
		return
	}
	if _, ok := c.chanKey(ch.name, ch.epoch); ok {
		return
	}
	if ch.keyReqSent {
		return
	}
	ch.keyReqSent = true
	c.write(Msg{T: "keyreq", Ch: ch.name, Epoch: ch.epoch})
}

// onKeyReq answers someone else's request for a channel key we hold, by wrapping it to
// their device's public key. Multiple devices may all answer the same request; that's
// harmless — the requester keeps whichever arrives first, and they're all the same key.
func (c *client) onKeyReq(m Msg) {
	ch := c.chanByName(m.Ch)
	if ch == nil || !ch.e2e || m.FromPub == "" {
		return
	}
	key, ok := c.chanKey(ch.name, m.Epoch)
	if !ok || c.e2ePriv == nil {
		return
	}
	wrapped, err := wrapChannelKey(c.e2ePriv, m.FromPub, ch.name, m.Epoch, key)
	if err != nil {
		return
	}
	c.write(Msg{T: "keyshare", Ch: ch.name, Epoch: m.Epoch, ToUser: m.FromUser, ToDevice: m.FromDev,
		FromPub: c.snapshot().E2EPub, Wrapped: wrapped})
}

// onKeyShare accepts a channel key handed to us by another device — one of our own, or
// (after a rotation) any member's device that already has the new one — and replays
// anything that arrived encrypted before we could read it.
func (c *client) onKeyShare(m Msg) {
	if c.e2ePriv == nil || m.FromPub == "" {
		return
	}
	key, err := unwrapChannelKey(c.e2ePriv, m.FromPub, m.Ch, m.Epoch, m.Wrapped)
	if err != nil {
		c.sysIn(c.chanByName(m.Ch), "received a channel key that failed to decrypt — ignoring it")
		return
	}
	c.storeChanKey(m.Ch, m.Epoch, key)
	ch := c.chanByName(m.Ch)
	if ch == nil {
		return
	}
	if m.Epoch > ch.epoch {
		ch.epoch = m.Epoch
	}
	ch.keyReqSent = false
	pending := ch.pendingEnc
	ch.pendingEnc = nil
	for _, pm := range pending {
		c.renderMsg(ch, pm)
	}
	if len(pending) > 0 {
		c.sysIn(ch, fmt.Sprintf("🔓 channel key received — showing %d held-back message(s)", len(pending)))
	} else {
		c.sysIn(ch, "🔓 channel key received for #"+ch.name)
	}
	if ch == c.active {
		c.drawStatus()
	}
}

// encryptForSend seals outgoing text (a chat message or clipboard paste) for an e2e
// channel. ok is false if the channel needs a key we don't have yet.
func (c *client) encryptForSend(ch *chanState, plaintext string) (ciphertext string, epoch int, ok bool) {
	if ch == nil || !ch.e2e {
		return plaintext, 0, true
	}
	key, have := c.chanKey(ch.name, ch.epoch)
	if !have {
		return "", 0, false
	}
	ct, err := encryptText(key, ch.name, ch.epoch, plaintext)
	if err != nil {
		return "", 0, false
	}
	return ct, ch.epoch, true
}

// ---- rotate / reshare --------------------------------------------------------------

// rotateAndReshare bumps a channel's key epoch (the hub enforces that only mod+ may;
// see actE2ERotate) and, on success, generates the new key locally and hands it to
// every peer currently online. Called both by /e2e rotate and automatically after a
// kick/ban in an e2e channel (see afterModeration).
func (c *client) rotateAndReshare(chName string) {
	c.cmdOn(chName, "e2erotate", nil, func(res Msg) {
		if !res.OK {
			c.sys("e2e rotate: " + res.Text)
			return
		}
		if len(res.Channels) == 0 {
			return
		}
		epoch := res.Channels[0].Epoch
		ch := c.chanByName(chName)
		if ch == nil {
			return
		}
		ch.epoch = epoch
		key, err := newChannelKey()
		if err != nil {
			c.sys("e2e rotate: could not generate a channel key: " + err.Error())
			return
		}
		c.storeChanKey(chName, epoch, key)
		c.sys(fmt.Sprintf("🔒 #%s rotated to epoch %d — sharing the new key with everyone online", chName, epoch))
		c.reshareToOnlinePeers(chName, epoch, key)
	})
}

// reshareToOnlinePeers wraps `key` for every other device currently online in the
// channel and sends it directly. Devices offline right now pick the key up later via
// their own keyreq when they reconnect (see maybeRequestKey / onKeyReq).
func (c *client) reshareToOnlinePeers(chName string, epoch int, key []byte) {
	c.cmdOn(chName, "e2epeers", nil, func(res Msg) {
		if !res.OK || c.e2ePriv == nil {
			return
		}
		me, myDevice := c.me, c.snapshot().Name
		sent := 0
		for _, p := range res.Peers {
			if p.User == me && p.Device == myDevice {
				continue // that's us
			}
			wrapped, err := wrapChannelKey(c.e2ePriv, p.Pub, chName, epoch, key)
			if err != nil {
				continue
			}
			c.write(Msg{T: "keyshare", Ch: chName, Epoch: epoch, ToUser: p.User, ToDevice: p.Device,
				FromPub: c.snapshot().E2EPub, Wrapped: wrapped})
			sent++
		}
		if sent > 0 {
			c.sys(fmt.Sprintf("🔒 shared the epoch %d key for #%s with %d online device(s)", epoch, chName, sent))
		}
	})
}

// ---- slash commands -----------------------------------------------------------------

// e2eCmd implements /e2e [status|rotate|reshare], all scoped to the active channel.
func (c *client) e2eCmd(f []string) {
	if c.active == nil {
		return
	}
	ch := c.active
	sub := "status"
	if len(f) > 0 {
		sub = f[0]
	}
	switch sub {
	case "status":
		if !ch.e2e {
			c.sys("#" + ch.name + " is not end-to-end encrypted")
			return
		}
		_, have := c.chanKey(ch.name, ch.epoch)
		state := "🔒 locked — waiting for the key (/e2e reshare on a device that has it, or it'll arrive automatically)"
		if have {
			state = "🔓 unlocked — this device holds the current key"
		}
		c.sys(fmt.Sprintf("#%s is end-to-end encrypted, epoch %d: %s", ch.name, ch.epoch, state))
		c.sys("your device fingerprint: " + keyFingerprint(c.snapshot().E2EPub) + "  (/verify NAME to compare a member's)")
	case "rotate":
		if !ch.e2e {
			c.sys("#" + ch.name + " is not end-to-end encrypted")
			return
		}
		c.rotateAndReshare(ch.name)
	case "reshare":
		if !ch.e2e {
			c.sys("#" + ch.name + " is not end-to-end encrypted")
			return
		}
		key, have := c.chanKey(ch.name, ch.epoch)
		if !have {
			c.sys("this device doesn't hold the current key either — /e2e rotate instead, from a device that does")
			return
		}
		c.reshareToOnlinePeers(ch.name, ch.epoch, key)
	default:
		c.sys("usage: /e2e status | rotate | reshare")
	}
}

// verifyCmd shows a member's device fingerprints for out-of-band comparison — the same
// trust-on-first-use idea as the TLS certificate pin, applied to who holds a channel key.
func (c *client) verifyCmd(name string) {
	if name == "" {
		c.sys("your device fingerprint: " + keyFingerprint(c.snapshot().E2EPub))
		return
	}
	if c.active == nil || !c.active.e2e {
		c.sys("/verify only applies inside an end-to-end encrypted channel")
		return
	}
	c.cmd("e2epeers", nil, func(m Msg) {
		if !m.OK {
			c.printRes(m)
			return
		}
		found := false
		for _, p := range m.Peers {
			if strings.EqualFold(p.User, name) {
				c.sys(fmt.Sprintf("%s @ %s: %s", p.User, p.Device, keyFingerprint(p.Pub)))
				found = true
			}
		}
		if !found {
			c.sys(name + " has no device online here right now — ask them to open the channel, then try again")
		}
	})
}

// afterModeration triggers the recommended key rotation once a kick or ban in an e2e
// channel succeeds, if this device currently holds that channel's key. If it doesn't,
// whichever device does still needs to rotate — this is a best-effort nudge, not the
// only path to a rotation happening.
func (c *client) afterModeration(chName string) {
	ch := c.chanByName(chName)
	if ch == nil || !ch.e2e {
		return
	}
	if _, have := c.chanKey(ch.name, ch.epoch); !have {
		c.sys("⚠ #" + chName + " is end-to-end encrypted — its key should be rotated (/e2e rotate) by a device that holds it")
		return
	}
	c.rotateAndReshare(chName)
}
