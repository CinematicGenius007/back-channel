package main

import (
	"os"
	"runtime"
	"strings"
)

// Themes for the app TUI. The default follows the project website: near-black, hairline
// rules and one signal green. The rest are alternatives a person can pick in setup or
// under Manage → Appearance; "terminal" uses the terminal's own colours throughout.
type theme struct {
	id, name                           string
	bg, panel, ink, copy, muted, rule  colorV
	sig, sigText, sigInk, warn, danger colorV
	nicks                              [8]colorV
}

var themes = []*theme{
	mkTheme("site-dark", "Site dark", "#0b0b0b", "#1c1c1c", "#ececec", "#bdbdbd", "#8a8a8a", "#3a3a3a", "#3dff7f", "#3dff7f", "#0a0a0a", "#ffd166", "#ff6b5e",
		"#7fd4ff", "#3dff7f", "#e59cff", "#8ab4ff", "#ff8a7a", "#5ff2e6", "#b8f28a", "#ffb3e6"),
	mkTheme("site-light", "Site light", "#ffffff", "#ededed", "#0a0a0a", "#3a3a3a", "#6e6e6e", "#c9c9c9", "#27e36a", "#0a7d34", "#0a0a0a", "#8a5a00", "#b3261e",
		"#0067a3", "#0a7d34", "#8a3fbf", "#2453c7", "#b3261e", "#00796b", "#4f7d00", "#a8326e"),
	mkTheme("phosphor", "Phosphor", "#050805", "#0c1a10", "#c9ffd9", "#8fc4a3", "#5f9873", "#1f4a2e", "#5dff93", "#5dff93", "#04120a", "#eaff7a", "#ff8f6b",
		"#5dff93", "#c9ffd9", "#8fe8b0", "#3fd47a", "#b6ffcf", "#79f2a8", "#a5e8b8", "#e1ffe9"),
	mkTheme("amber", "Amber", "#0a0700", "#140f05", "#ffd98a", "#d9a441", "#8a6a2a", "#5a4318", "#ffb000", "#ffb000", "#150c00", "#ff7a3d", "#ff5a3d",
		"#ffb000", "#ffd98a", "#ffc54d", "#e69500", "#ffe2a8", "#ffcd70", "#d9a441", "#fff0c9"),
	mkTheme("telex", "Telex", "#efe9dc", "#f7f2e6", "#1b1a17", "#3d3a33", "#6d675b", "#cfc7b5", "#b8381d", "#b8381d", "#ffffff", "#9a5b00", "#b8381d",
		"#1d5fb8", "#2d7a46", "#7a3b9e", "#3d3a33", "#b8381d", "#246b6b", "#5c6b1f", "#9e2f5c"),
	mkTheme("blueprint", "Blueprint", "#0a1626", "#0f2038", "#d6e6ff", "#9bb6dc", "#6a86b0", "#2c4a78", "#6ea8ff", "#6ea8ff", "#06101e", "#ffd27a", "#ff8a8a",
		"#6ea8ff", "#9be7ff", "#c4a7ff", "#8fb8ff", "#ff9e9e", "#7fe0d0", "#b9e68a", "#ffb8e0"),
	{id: "terminal", name: "Terminal colours", sig: ansiColor(2), sigText: ansiColor(2), sigInk: ansiColor(0), muted: ansiColor(8), rule: ansiColor(8),
		warn: ansiColor(3), danger: ansiColor(1),
		nicks: [8]colorV{ansiColor(6), ansiColor(2), ansiColor(5), ansiColor(4), ansiColor(9), ansiColor(14), ansiColor(10), ansiColor(13)}},
}

func mkTheme(id, name, bg, panel, ink, copy, muted, rule, sig, sigText, sigInk, warn, danger string, nicks ...string) *theme {
	t := &theme{id: id, name: name, bg: hexColor(bg), panel: hexColor(panel), ink: hexColor(ink), copy: hexColor(copy),
		muted: hexColor(muted), rule: hexColor(rule), sig: hexColor(sig), sigText: hexColor(sigText), sigInk: hexColor(sigInk),
		warn: hexColor(warn), danger: hexColor(danger)}
	for i := range t.nicks {
		t.nicks[i] = hexColor(nicks[i])
	}
	return t
}

func themeByID(id string) *theme {
	for _, t := range themes {
		if t.id == id {
			return t
		}
	}
	if os.Getenv("NO_COLOR") != "" {
		return themeByID("terminal")
	}
	return themes[0]
}

// palette maps the 16 ANSI colours the client uses when formatting messages (nick
// colours, mention highlight, dim system lines) onto the theme, so a message printed
// as "\x1b[36m" is readable on a light theme too.
func (t *theme) palette() *[16]colorV {
	var p [16]colorV
	if t.id == "terminal" {
		return &p // leave everything to the terminal
	}
	p[0], p[8] = t.muted, t.muted
	p[7], p[15] = t.copy, t.ink
	p[1], p[9] = t.nicks[4], t.danger
	p[2], p[10] = t.nicks[1], t.nicks[6]
	p[3], p[11] = t.warn, t.warn
	p[4], p[12] = t.nicks[3], t.nicks[3]
	p[5], p[13] = t.nicks[2], t.nicks[7]
	p[6], p[14] = t.nicks[0], t.nicks[5]
	return &p
}

// colorDepth guesses whether the terminal takes 24-bit colour. Apple Terminal only
// gained it recently and many SSH sessions don't advertise it, so 256 colours (which
// every modern terminal has) is the fallback.
func colorDepth() int {
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	switch {
	case ct == "truecolor" || ct == "24bit":
		return 24
	case os.Getenv("WT_SESSION") != "" || runtime.GOOS == "windows":
		return 24
	case os.Getenv("TERM_PROGRAM") == "iTerm.app" || os.Getenv("TERM_PROGRAM") == "WezTerm" || os.Getenv("TERM_PROGRAM") == "vscode":
		return 24
	case strings.Contains(os.Getenv("TERM"), "direct") || strings.Contains(os.Getenv("TERM"), "kitty"):
		return 24
	}
	return 256
}
