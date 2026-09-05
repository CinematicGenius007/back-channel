package main

// Abuse control. Every limit exists at three levels; the most specific non-zero value
// wins: server default → channel override → user override. Limits are keyed by
// account (or guest nick), not IP, so an office behind one NAT doesn't share a bucket
// and one patient attacker with many IPs doesn't get many buckets.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- token buckets ----------------------------------------------------------

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   float64 // tokens per second
	burst  float64
}

func newBucket(rate, burst float64) *bucket {
	return &bucket{tokens: burst, last: time.Now(), rate: rate, burst: burst}
}

func (b *bucket) take(n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// limiter keys buckets by a string and forgets idle ones.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	seen    map[string]time.Time
	rate    float64
	burst   float64
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{buckets: map[string]*bucket{}, seen: map[string]time.Time{}, rate: rate, burst: burst}
}

// allow uses the limiter's default rate.
func (l *limiter) allow(key string, n float64) bool { return l.allowRate(key, l.rate, l.burst, n) }

// allowRate uses a per-call rate (the effective limit for this user/channel).
func (l *limiter) allowRate(key string, rate, burst, n float64) bool {
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		b = newBucket(rate, burst)
		l.buckets[key] = b
	} else if b.rate != rate || b.burst != burst {
		b.mu.Lock()
		b.rate, b.burst = rate, burst
		b.mu.Unlock()
	}
	l.seen[key] = time.Now()
	if len(l.buckets) > 4096 { // GC stale keys under memory pressure
		for k, t := range l.seen {
			if time.Since(t) > time.Hour {
				delete(l.buckets, k)
				delete(l.seen, k)
			}
		}
	}
	l.mu.Unlock()
	return b.take(n)
}

// ---- effective limits -------------------------------------------------------

func (h *hub) lim() *Limits { return &h.st.Settings.Limits }

// msgRateFor: server → channel → user override; mods and above get 3× (they're cleaning up).
func (h *hub) msgRateFor(u *User, ch *Channel, rank int) (rate, burst float64) {
	L := h.lim()
	rate, burst = L.MsgRate, L.MsgBurst
	if ch.MsgRate > 0 {
		rate = ch.MsgRate
	}
	if u.MsgRate > 0 {
		rate = u.MsgRate
	}
	if rank >= rankMod {
		rate *= 3
	}
	if burst < 1 {
		burst = 1
	}
	return rate, burst
}

func (h *hub) maxMsgBytes() int { return h.lim().MaxMsgKB << 10 }

func (h *hub) maxFileFor(ch *Channel) int64 {
	if ch != nil && ch.MaxFileMB > 0 {
		return ch.MaxFileMB << 20
	}
	return h.lim().MaxFileMB << 20
}

// quotaFor returns the user's storage quota in bytes; 0 = unlimited.
func (h *hub) quotaFor(u *User) int64 {
	if u.QuotaMB > 0 {
		return u.QuotaMB << 20
	}
	return h.lim().QuotaMB << 20
}

func (h *hub) uploadRateFor(u *User) float64 {
	if u.UploadRate > 0 {
		return u.UploadRate
	}
	return h.lim().UploadRate
}

func (h *hub) limitsFor(u *User) *LimitInfo {
	li := &LimitInfo{MaxMsg: h.maxMsgBytes(), MaxFile: h.lim().MaxFileMB << 20}
	if q := h.quotaFor(u); q > 0 {
		left := q - h.used[u.ID]
		if left < 0 {
			left = 0
		}
		li.QuotaLeft = left
	}
	return li
}

// ---- auto-mute --------------------------------------------------------------

// noteRateHit records a rate-limit hit for (user, channel). More than AutoMuteHits in
// five minutes → muted for AutoMuteMin in that channel, with an audit entry. Automation
// is deliberately conservative: it mutes for minutes; humans decide bans.
// Caller holds h.mu. Returns true if a mute was applied.
func (h *hub) noteRateHit(u *User, ch *Channel) bool {
	key := fmt.Sprintf("%d/%d", u.ID, ch.ID)
	now := nowMs()
	hits := h.rateHits[key]
	cut := now - 5*60*1000
	j := 0
	for _, t := range hits {
		if t > cut {
			hits[j] = t
			j++
		}
	}
	hits = append(hits[:j], now)
	h.rateHits[key] = hits
	L := h.lim()
	if L.AutoMuteHits > 0 && len(hits) > L.AutoMuteHits {
		delete(h.rateHits, key)
		m := ch.Members[u.ID]
		if m == nil {
			return false // guests: nothing to mute; they're already rate-limited
		}
		until := now + int64(L.AutoMuteMin)*60*1000
		m.MutedUntil = until
		h.markDirty()
		h.audit(nil, "automute", u.Name, ch, fmt.Sprintf("%d rate-limit hits in 5 min → muted %d min", len(hits), L.AutoMuteMin))
		h.toChan(ch.ID, Msg{T: "muted", Ch: ch.Name, From: u.Name, By: "hub", Until: until, Text: "flooding"})
		return true
	}
	return false
}

// ---- retention janitor ------------------------------------------------------

// janitor: every 15 s expire messages in self-destruct channels; every hour do the
// slower housekeeping (bans, mutes, invites, compaction).
func (h *hub) janitor() {
	n := 0
	for {
		time.Sleep(15 * time.Second)
		n++
		h.mu.Lock()
		h.expireMessages()
		if n%240 == 0 {
			h.sweep()
		}
		h.mu.Unlock()
	}
}

// expireMessages removes messages older than each channel's expire_sec, drops their
// file references (deleting files nobody references any more) and tells subscribers
// which ids vanished so they disappear from screens too. Caller holds h.mu.
func (h *hub) expireMessages() {
	now := nowMs()
	for _, ch := range h.st.Channels {
		if ch.ExpireSec <= 0 {
			continue
		}
		cut := now - ch.ExpireSec*1000
		ms := h.msgs[ch.ID]
		i := 0
		var ids []int64
		for i < len(ms) && ms[i].TS < cut {
			if ms[i].T == "file" && ms[i].FID != "" {
				h.unrefFile(ms[i].FID, ch.ID)
			}
			ids = append(ids, ms[i].ID)
			i++
		}
		if i == 0 {
			continue
		}
		h.msgs[ch.ID] = append([]Msg(nil), ms[i:]...)
		h.removed += i
		h.toChan(ch.ID, Msg{T: "expired", Ch: ch.Name, IDs: ids})
	}
	if h.removed > 1000 || (h.removed > 0 && time.Since(h.lastComp) > 10*time.Minute) {
		h.compactLog()
		h.removed, h.lastComp = 0, time.Now()
	}
}

// parseExpire turns "10m", "2h", "7d", "1w", "off" into seconds (0 = keep forever).
func parseExpire(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "off" || s == "0" || s == "never" {
		return 0, true
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	mult := map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 7 * 86400}[unit]
	if mult == 0 {
		return 0, false
	}
	sec := n * mult
	if sec < 60 {
		sec = 60
	}
	return sec, true
}

// fmtExpire renders seconds the way parseExpire reads them.
func fmtExpire(sec int64) string {
	switch {
	case sec <= 0:
		return "off"
	case sec%(7*86400) == 0:
		return fmt.Sprintf("%dw", sec/(7*86400))
	case sec%86400 == 0:
		return fmt.Sprintf("%dd", sec/86400)
	case sec%3600 == 0:
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dm", sec/60)
}

// sweep is the hourly housekeeping; called under h.mu (and once at startup).
func (h *hub) sweep() {
	now := nowMs()
	h.expireMessages()
	changed := h.removed > 0
	for _, ch := range h.st.Channels {
		for uid, b := range ch.Bans {
			if b.Until > 0 && b.Until < now {
				delete(ch.Bans, uid)
				h.markDirty()
			}
		}
		for _, m := range ch.Members {
			if m.MutedUntil > 0 && m.MutedUntil < now {
				m.MutedUntil = 0
				h.markDirty()
			}
		}
	}
	for code, inv := range h.st.Invites {
		if (inv.ExpiresAt > 0 && inv.ExpiresAt < now) || inv.UsesLeft == 0 {
			delete(h.st.Invites, code)
			h.markDirty()
		}
	}
	for _, u := range h.st.Users {
		if u.Status == "banned" && u.BanUntil > 0 && u.BanUntil < now {
			u.Status, u.BanReason, u.BanUntil = "active", "", 0
			h.markDirty()
		}
	}
	if changed {
		h.compactLog()
		h.removed, h.lastComp = 0, time.Now()
	}
	if h.dirty {
		h.save()
	}
}

// exhausted reports whether the bucket for key is empty without consuming anything.
// Used for logins: only *failed* attempts are charged, so a user logging in from several
// devices in a minute isn't locked out by their own successes.
func (l *limiter) exhausted(key string) bool {
	l.mu.Lock()
	b := l.buckets[key]
	l.mu.Unlock()
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tokens := b.tokens + time.Since(b.last).Seconds()*b.rate
	return tokens < 1
}
