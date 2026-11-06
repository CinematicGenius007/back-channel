package main

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ---- desktop notifications ------------------------------------------------------------
//
// With `/notify desktop`, a message in a channel you are not looking at, a mention, or
// any message while the terminal window is in the background (the app TUI asks the
// terminal for focus reports) raises a system notification. The text of a message in
// an end-to-end encrypted channel never goes to the OS notification centre — it only
// says that a message arrived — because notification centres keep history and show
// it on lock screens, which would undo what e2e is for.

func (c *client) maybeNotify(ch *chanState, m Msg, text string, mention, mine bool) {
	if m.Hist || mine || c.snapshot().Notify != "desktop" {
		return
	}
	if !mention && ch == c.active && !c.unfocused.Load() {
		return
	}
	if time.Since(c.lastNotify) < 4*time.Second { // a burst of messages → one notification
		return
	}
	c.lastNotify = time.Now()
	title := "#" + ch.name + " · " + m.From
	body := text
	switch {
	case ch.e2e:
		body = "new encrypted message"
		if m.T == "file" {
			body = "new encrypted file"
		}
	case m.T == "file":
		body = "📎 " + m.Name
	}
	if mention {
		title = m.From + " mentioned you in #" + ch.name
	}
	go desktopNotify(title, clip(strings.ReplaceAll(body, "\n", " "), 140))
}

// desktopNotify shows a system notification. Arguments travel as argv or environment
// variables, never spliced into a script, so message text cannot inject commands.
func desktopNotify(title, body string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("osascript", "-e", "on run argv", "-e",
			"display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", title, body)
	case "windows":
		cmd = windowsToast(title, body)
	default:
		cmd = exec.Command("notify-send", "-a", appName, title, body)
	}
	if cmd == nil {
		return fmt.Errorf("no notifier on this system")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, io.Discard, io.Discard
	return cmd.Run()
}

// notifyMark is the "● " / "  " prefix Manage puts on the notify mode in use.
func notifyMark(cur, mode string) string {
	if cur == "on" {
		cur = ""
	}
	if cur == mode {
		return "● "
	}
	return "  "
}

// ---- transfer progress ----------------------------------------------------------------

// progressReader reports how far an upload or download has got on the status line,
// at most four times a second.
type progressReader struct {
	r           io.Reader
	c           *client
	label       string
	total, done int64
	last        time.Time
}

func (c *client) progress(r io.Reader, total int64, label string) io.Reader {
	return &progressReader{r: r, c: c, label: label, total: total}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if time.Since(p.last) >= 250*time.Millisecond {
		p.last = time.Now()
		p.c.status(progressText(p.label, p.done, p.total))
	}
	return n, err
}

func progressText(label string, done, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("%s  %s", label, humanSize(done))
	}
	if done > total {
		done = total
	}
	pct := int(done * 100 / total)
	const w = 20
	fill := pct * w / 100
	return fmt.Sprintf("%s  [%s%s] %d%%  %s of %s", label, strings.Repeat("#", fill), strings.Repeat(".", w-fill), pct,
		humanSize(done), humanSize(total))
}

// ---- device verification marks ---------------------------------------------------------

func verifyKey(user, fp string) string { return strings.ToLower(user) + "|" + fp }

func (c *client) isVerified(user, fp string) bool {
	c.cfgMu.Lock() // snapshot copies the struct, not the map, so read it under the lock
	defer c.cfgMu.Unlock()
	return c.cfg.Verified[verifyKey(user, fp)]
}

func (c *client) setVerified(user, fp string, on bool) {
	c.update(func(cf *Config) {
		if cf.Verified == nil {
			cf.Verified = map[string]bool{}
		}
		if on {
			cf.Verified[verifyKey(user, fp)] = true
		} else {
			delete(cf.Verified, verifyKey(user, fp))
		}
	})
}
