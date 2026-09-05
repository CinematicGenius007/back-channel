package main

// Clipboard sync between *your own* devices (all sessions of the same account).
//
// When it's on, the client polls the local clipboard; a new text value is sent to the
// hub as a `clip` frame, which relays it to your other connected devices and to nobody
// else. The hub never stores or replays it. Receiving devices with sync on write it
// straight into their clipboard; devices with sync off keep it for `/clip pull`.
//
// Loop prevention: we remember the last value we sent or applied and never re-send it.
// Windows detects changes with GetClipboardSequenceNumber (one cheap syscall every
// 500 ms, no read unless it changed); macOS/Linux have to run pbpaste/xclip, so they
// poll every 1.5 s and only while sync is on.

import (
	"fmt"
	"time"
)

func (c *client) clipSet(s string) {
	c.clipMu.Lock()
	c.clipLast = s
	c.clipMu.Unlock()
}

func (c *client) clipGet() string {
	c.clipMu.Lock()
	defer c.clipMu.Unlock()
	return c.clipLast
}

func (c *client) watchClipboard() {
	var lastSeq uint32
	warned := ""
	for {
		time.Sleep(clipPollInterval())
		if !c.clipOn.Load() || !c.online.Load() {
			continue
		}
		if seq := clipSeq(); seq != 0 {
			if seq == lastSeq {
				continue
			}
			lastSeq = seq
		}
		txt, err := clipboardRead()
		if err != nil || txt == "" || txt == c.clipGet() {
			continue
		}
		if int64(len(txt)) > c.maxMsg.Load() {
			if warned != txt {
				warned = txt
				c.status(fmt.Sprintf("clipboard not synced: %s is over the hub's %s limit", humanSize(int64(len(txt))), humanSize(c.maxMsg.Load())))
			}
			continue
		}
		c.clipSet(txt)
		if err := c.write(Msg{T: "clip", Text: txt}); err == nil {
			c.status(fmt.Sprintf("📋 clipboard synced (%s)", humanSize(int64(len(txt)))))
		}
	}
}

// onClip handles a clipboard value from one of my other devices (main loop).
func (c *client) onClip(m Msg) {
	if m.Text == "" {
		return
	}
	if !c.clipOn.Load() {
		c.clipMu.Lock()
		c.clipIn = m.Text
		c.clipMu.Unlock()
		c.ui.setStatus(fmt.Sprintf("📋 %s pushed its clipboard (%s) — /clip pull to apply, /clip on to auto-sync", m.From, humanSize(int64(len(m.Text)))))
		return
	}
	c.clipSet(m.Text) // so the watcher doesn't echo it back
	if err := clipboardWrite(m.Text); err != nil {
		c.sys("clipboard from " + m.From + " received but the local clipboard is unavailable: " + err.Error())
		return
	}
	c.sys(fmt.Sprintf("📋 clipboard ← %s (%s)", m.From, humanSize(int64(len(m.Text)))))
}
