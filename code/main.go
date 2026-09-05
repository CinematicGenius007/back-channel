// backchannel (bch) — a tiny self-hosted multi-channel "IRC" for sharing text,
// clipboard and files between your machines and a small group of people you trust.
// Single static binary, stdlib only.
//
//	bch serve                      run the hub (one machine: your Mac, or a VPS)
//	bch                            open the TUI (auto-discovers a hub on the LAN)
//	bch -hub HOST -user alice      connect to a hub with an account
//	bch -hub HOST -token X         connect as a guest (legacy shared token)
//	bch send [-ch NAME] <file|text> one-shot drop into a channel
//	bch discover                   print hubs visible on the LAN
package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is remembered in <dir>/config.json so the second run needs no flags.
type Config struct {
	Hub     string `json:"hub"`               // host, host:port, or tls://host:port
	Name    string `json:"name"`              // device label (v2) / guest nick (v1 hubs)
	User    string `json:"user,omitempty"`    // account name
	Session string `json:"session,omitempty"` // session token (revocable; never the password)
	Token   string `json:"token,omitempty"`   // legacy shared token for guest access / v1 hubs
	AutoMB  int    `json:"auto_mb"`           // auto-download files up to this many MB
	TLS     bool   `json:"tls,omitempty"`
	FP      string `json:"fingerprint,omitempty"` // pinned hub certificate (SHA-256)

	ClipSync bool     `json:"clip_sync,omitempty"` // sync clipboard with my other devices
	Notify   string   `json:"notify,omitempty"`    // "off" to silence the bell (default on)
	Active   string   `json:"active,omitempty"`    // last active channel
	Watch    []string `json:"watch,omitempty"`     // channels that auto-download even when not active
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "serve", "hub":
			runHub(args[1:])
			return
		case "send":
			runSend(args[1:])
			return
		case "discover":
			for _, h := range discover(1500) {
				fmt.Println(h)
			}
			return
		case "help", "-h", "--help":
			usage()
			return
		case "version", "-v", "--version":
			fmt.Println(appName, version)
			return
		}
	}
	runClient(args)
}

func usage() {
	fmt.Print(appName + ` ` + version + ` — private channels for text, clipboard and files

  bch serve [-port 7777] [-dir D]            run the hub (accounts, channels, history, files)
  bch serve -public                          hub for the internet: TLS + fingerprint pin + strict limits
  bch serve -reset-owner                     print a new password for the owner account
  bch [-hub [tls://]HOST[:PORT]] -user NAME  join with an account (asks for the password once)
  bch -hub HOST -invite CODE -user NAME      create an account from an invite code
  bch -hub HOST -token X                     join as a guest (shared token, if the hub allows it)
  bch send [-ch NAME] <file | text...>       one-shot drop into a channel (stdin if no arg)
  bch discover                               find hubs on this LAN

Inside the TUI:
  type to chat · drag a file into the window and press Enter to send it · /help for everything
  channels:   /1 … /9  /n  /p  /c NAME  /channels  /join CODE  /leave  /create NAME [topic]
  files:      /send PATH  /get [#id]  /open  /watch [NAME]
  clipboard:  /paste  /copy [#id]  /clip on|off|push|pull
  moderate:   /invite  /members  /topic  /kick  /ban  /mute  /role  /del  /purge  /settings
  account:    /login NAME  /passwd  /sessions  /logout  /admin ...
Anything put in <dir>/outbox is sent to the active channel; <dir>/outbox/<channel>/ to that channel.
Received files land in <dir>/inbox/<channel>/.  <dir> is ~/backchannel (or $BCH_DIR).
`)
}

func defaultDir() string {
	if d := os.Getenv("BCH_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, appName)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const alnum = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/l/I

// randAlnum returns n characters from an unambiguous alphabet (~5.8 bits each).
func randAlnum(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

func loadConfig(dir string) Config {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(dir string, c Config) {
	os.MkdirAll(dir, 0o755)
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

// splitHub returns tcp addr, http addr and whether TLS was requested, for
// "host", "host:port", "[v6]:port", "tls://host:port".
func splitHub(hub string, defPort int) (tcpAddr, httpAddr string, useTLS bool) {
	if strings.HasPrefix(hub, "tls://") {
		useTLS = true
		hub = strings.TrimPrefix(hub, "tls://")
	}
	host, port := hub, defPort
	if i := strings.LastIndex(hub, ":"); i > 0 && !strings.Contains(hub[i:], "]") {
		host = hub[:i]
		fmt.Sscanf(hub[i+1:], "%d", &port)
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port), fmt.Sprintf("%s:%d", host, port+1), useTLS
}

// tuneTCP sets low-latency + keepalive options on the underlying TCP socket.
func tuneTCP(c net.Conn) {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(20 * time.Second)
	}
}

func humanSize(n int64) string {
	const k = 1024.0
	f := float64(n)
	switch {
	case f >= k*k*k:
		return fmt.Sprintf("%.1f GB", f/k/k/k)
	case f >= k*k:
		return fmt.Sprintf("%.1f MB", f/k/k)
	case f >= k:
		return fmt.Sprintf("%.0f KB", f/k)
	}
	return fmt.Sprintf("%d B", n)
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func nowMs() int64 { return time.Now().UnixMilli() }

// sanitizeName strips path separators and control chars from a file name.
func sanitizeName(s string) string {
	s = filepath.Base(strings.ReplaceAll(s, "\\", "/"))
	var b strings.Builder
	for _, r := range s {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), ". ")
	if out == "" {
		out = "file"
	}
	return out
}

// validName checks a channel or user name: lowercase letters, digits and _-. (users may
// also use '.'); length between min and max. Names are compared case-insensitively.
func validName(s string, min, max int, allowDot bool) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || (allowDot && r == '.')
		if !ok {
			return false
		}
	}
	return true
}

func writeJSON(c net.Conn, m Msg) {
	b, _ := json.Marshal(m)
	c.Write(append(b, '\n'))
}
