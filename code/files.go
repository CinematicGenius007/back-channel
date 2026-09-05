package main

// Files travel over a minimal HTTP/1.1 server on port+1 (see http_min.go) so they
// stream at line speed without blocking the chat socket and stay curl-compatible.
//
//	PUT /up?ch=design&name=spec.pdf     Authorization: Bearer <session>   (or X-Token for guests)
//	GET /f/<fid>                        same auth
//	GET /                               public one-line status
//
// Uploads are checked (membership, size, quota, rate) *before* the body is sent thanks
// to `Expect: 100-continue`; a refused 2 GB upload costs nothing. Bytes are hashed while
// they stream; identical content is stored once and quota is charged once, to the first
// uploader. Downloads require membership of a channel that contains a message referring
// to the file, so a leaked id from a private channel is useless to outsiders.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (h *hub) serveHTTP(port int) {
	ln, err := h.listen(port)
	if err != nil {
		log.Fatal(err)
	}
	h.acceptHTTP(ln)
}

func (h *hub) acceptHTTP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handleHTTP(c)
	}
}

// httpAuth resolves the requester: an account (session token) or a guest (legacy token).
func (h *hub) httpAuth(req *httpReq) (u *User, guest bool) {
	tok := ""
	if a := req.header["authorization"]; strings.HasPrefix(strings.ToLower(a), "bearer ") {
		tok = strings.TrimSpace(a[7:])
	}
	if tok == "" {
		tok = req.header["x-session"]
	}
	if tok == "" {
		tok = req.query["session"]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if tok != "" {
		se := h.st.Sessions[sessHash(tok)]
		if se == nil {
			return nil, false
		}
		u := h.st.Users[se.UserID]
		if u == nil || h.userBlocked(u) != "" {
			return nil, false
		}
		se.LastSeen = nowMs()
		h.markDirty()
		return u, false
	}
	lt := req.header["x-token"]
	if lt == "" {
		lt = req.query["token"]
	}
	S := h.st.Settings
	if lt != "" && S.GuestAccess && S.LegacyToken != "" && lt == S.LegacyToken {
		nick := strings.ToLower(sanitizeName(req.query["from"]))
		if nick == "" || nick == "file" {
			nick = "guest"
		}
		if h.userByName(nick) != nil {
			nick += "~"
		}
		return &User{ID: guestID(nick), Name: nick, Role: "guest", Status: "active"}, true
	}
	return nil, false
}

func (h *hub) handleHTTP(conn net.Conn) {
	defer conn.Close()
	ip := remoteIP(conn)
	if !h.connAllow(ip, 0.5) { // HTTP hits count half a connection
		return
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	req, err := readRequest(bufio.NewReaderSize(conn, 32<<10))
	if err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	if req.path == "/" {
		h.mu.Lock()
		n := 0
		for _, ms := range h.msgs {
			n += len(ms)
		}
		s := fmt.Sprintf("%s hub %s — %d connections, %d channels, %d messages\n", appName, version, len(h.sessions), len(h.st.Channels), n)
		h.mu.Unlock()
		writeText(conn, 200, s)
		return
	}

	u, guest := h.httpAuth(req)
	if u == nil {
		writeText(conn, 401, "unauthorized\n")
		return
	}
	s := &session{user: u, guest: guest} // just enough for chanFor()

	switch {
	case req.path == "/up":
		h.upload(conn, req, s)

	case strings.HasPrefix(req.path, "/f/"):
		fid := sanitizeName(strings.TrimPrefix(req.path, "/f/"))
		h.mu.Lock()
		allowed := srvRank(u) >= srvAdmin
		for cid := range h.fileRefs[fid] {
			if ch := h.st.Channels[cid]; ch != nil && h.rankIn(u, ch) >= rankReadonly {
				allowed = true
				break
			}
		}
		h.mu.Unlock()
		matches, _ := filepath.Glob(filepath.Join(h.dir, "files", fid+"_*"))
		if !allowed || len(matches) == 0 { // identical answer for "no such file" and "not yours to see"
			writeText(conn, 404, "no such file\n")
			return
		}
		f, err := os.Open(matches[0])
		if err != nil {
			writeText(conn, 404, "no such file\n")
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		name := strings.TrimPrefix(filepath.Base(matches[0]), fid+"_")
		writeResponse(conn, 200, "application/octet-stream", st.Size(),
			fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n", name), f)

	default:
		writeText(conn, 404, "not found\n")
	}
}

func (h *hub) upload(conn net.Conn, req *httpReq, s *session) {
	if req.method != "PUT" && req.method != "POST" {
		writeText(conn, 405, "use PUT\n")
		return
	}
	if req.header["content-length"] == "" {
		writeText(conn, 411, "Content-Length required\n")
		return
	}
	u := s.user
	name := sanitizeName(req.query["name"])
	device := req.query["device"]

	// ---- admission: everything that can be decided before the bytes arrive ----
	h.mu.Lock()
	ch := h.chanFor(s, req.query["ch"])
	if ch == nil {
		h.mu.Unlock()
		writeText(conn, 404, textNoChan+"\n")
		return
	}
	rank := h.rankIn(u, ch)
	if !canSend(rank, ch) {
		h.mu.Unlock()
		writeText(conn, 403, "you can't post in #"+ch.Name+"\n")
		return
	}
	if mem := ch.Members[u.ID]; mem != nil && mem.MutedUntil > nowMs() {
		h.mu.Unlock()
		writeText(conn, 403, "you are muted in #"+ch.Name+"\n")
		return
	}
	if max := h.maxFileFor(ch); req.length > max {
		h.mu.Unlock()
		writeText(conn, 413, fmt.Sprintf("file too large (max %s in #%s)\n", humanSize(max), ch.Name))
		return
	}
	quota := h.quotaFor(u)
	if !s.guest && quota > 0 && h.used[u.ID]+req.length > quota {
		left := quota - h.used[u.ID]
		if left < 0 {
			left = 0
		}
		h.mu.Unlock()
		writeText(conn, 413, fmt.Sprintf("storage quota exceeded (%s left of %s) — delete old files or ask an admin\n", humanSize(left), humanSize(quota)))
		return
	}
	L := h.lim()
	if !h.upLim.allowRate(fmt.Sprintf("up/%d", u.ID), h.uploadRateFor(u)/60, 5, 1) {
		h.mu.Unlock()
		writeText(conn, 429, "too many uploads — slow down\n")
		return
	}
	if L.MaxUploads > 0 && h.uploading[u.ID] >= L.MaxUploads {
		h.mu.Unlock()
		writeText(conn, 429, fmt.Sprintf("too many concurrent uploads (max %d)\n", L.MaxUploads))
		return
	}
	h.uploading[u.ID]++
	cid := ch.ID
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.uploading[u.ID]--
		h.mu.Unlock()
	}()

	// ---- receive ----
	if strings.EqualFold(req.header["expect"], "100-continue") {
		io.WriteString(conn, "HTTP/1.1 100 Continue\r\n\r\n")
	}
	fid := randHex(8)
	path := filepath.Join(h.dir, "files", fid+"_"+name)
	f, err := os.Create(path)
	if err != nil {
		writeText(conn, 500, err.Error())
		return
	}
	hasher := sha256.New()
	conn.SetReadDeadline(time.Now().Add(time.Duration(10+req.length/(256<<10)) * time.Second)) // ≥256 KB/s or bust
	n, err := io.Copy(io.MultiWriter(f, hasher), req.body)
	f.Close()
	if err != nil || n != req.length {
		os.Remove(path)
		writeText(conn, 500, "upload incomplete\n")
		return
	}
	sum := hex.EncodeToString(hasher.Sum(nil))

	// ---- commit ----
	h.mu.Lock()
	ch = h.st.Channels[cid]
	if ch == nil { // deleted while uploading
		h.mu.Unlock()
		os.Remove(path)
		writeText(conn, 404, textNoChan+"\n")
		return
	}
	before := h.used[u.ID]
	if existing, dup := h.hashes[sum]; dup && h.st.Files[existing] != nil {
		os.Remove(path) // identical bytes already stored: reuse, charge nobody
		fid = existing
	} else {
		h.hashes[sum] = fid
		h.st.Files[fid] = &FileMeta{FID: fid, Sha: sum, Name: name, Size: n, Owner: u.ID, CreatedAt: nowMs()}
		if !s.guest {
			h.used[u.ID] += n
		}
		h.totalBytes += n
		os.WriteFile(filepath.Join(h.dir, "files", fid+".sha256"), []byte(sum), 0o644)
		h.markDirty()
	}
	h.post(ch, Msg{T: "file", From: u.Name, Device: device, Name: name, Size: n, FID: fid, Sha: sum})
	h.refFile(fid, ch.ID)
	if quota > 0 && !s.guest {
		after := h.used[u.ID]
		for _, pct := range []int64{80, 100} {
			line := quota * pct / 100
			if before < line && after >= line {
				h.toUser(u.ID, Msg{T: "quota", Text: fmt.Sprintf("storage %d%% used (%s of %s)", pct, humanSize(after), humanSize(quota))}, nil)
			}
		}
	}
	h.mu.Unlock()
	writeText(conn, 200, fmt.Sprintf(`{"fid":"%s","size":%d,"name":"%s","sha256":"%s"}`+"\n", fid, n, name, sum))
}
