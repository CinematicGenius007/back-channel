package main

// A deliberately tiny HTTP/1.1 subset — just enough for `PUT /up`, `GET /f/<id>`
// and `GET /` with Content-Length bodies. Replacing net/http drops TLS, HTTP/2,
// x509 and friends from the binary (~40% smaller) and a few MB of RSS.
// Still works with curl / browsers / PowerShell for plain requests.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

type httpReq struct {
	method, path string
	query        map[string]string
	header       map[string]string // lower-case keys
	body         io.Reader         // exactly Content-Length bytes
	length       int64
}

// readRequest parses the request line + headers from r.
func readRequest(r *bufio.Reader) (*httpReq, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return nil, fmt.Errorf("bad request line")
	}
	q := &httpReq{method: parts[0], header: map[string]string{}, query: map[string]string{}}
	q.path, q.query = splitQuery(parts[1])
	for {
		h, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok {
			q.header[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	q.length, _ = strconv.ParseInt(q.header["content-length"], 10, 64)
	q.body = io.LimitReader(r, q.length)
	return q, nil
}

func splitQuery(target string) (string, map[string]string) {
	m := map[string]string{}
	path, qs, _ := strings.Cut(target, "?")
	for _, kv := range strings.Split(qs, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = unescape(v)
		}
	}
	return unescape(path), m
}

func unescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '+':
			b.WriteByte(' ')
		case s[i] == '%' && i+2 < len(s):
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
			b.WriteByte('%')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// writeResponse sends a status + headers + a body of known length.
func writeResponse(w io.Writer, status int, ctype string, length int64, extra string, body io.Reader) error {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n%s\r\n",
		status, statusText(status), ctype, length, extra)
	if body != nil {
		_, err := io.Copy(w, body)
		return err
	}
	return nil
}

func writeText(w io.Writer, status int, s string) {
	writeResponse(w, status, "text/plain; charset=utf-8", int64(len(s)), "", strings.NewReader(s))
}

func statusText(c int) string {
	switch c {
	case 200:
		return "OK"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 411:
		return "Length Required"
	case 413:
		return "Payload Too Large"
	case 429:
		return "Too Many Requests"
	}
	return "Internal Server Error"
}

// ---- client side ------------------------------------------------------------

// httpDo sends one request and returns status, headers, and a body reader
// limited to Content-Length. Caller must Close() the returned conn.
type dialFunc func(addr string) (net.Conn, error)

func httpDo(dial dialFunc, addr, method, target string, headers map[string]string, body io.Reader, length int64) (int, map[string]string, io.Reader, net.Conn, error) {
	conn, err := dial(addr)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	var hb strings.Builder
	fmt.Fprintf(&hb, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", method, target, addr)
	for k, v := range headers {
		fmt.Fprintf(&hb, "%s: %s\r\n", k, v)
	}
	if body != nil {
		// Ask the hub to accept/refuse (size cap, rate limit) BEFORE we stream the bytes.
		fmt.Fprintf(&hb, "Content-Length: %d\r\nExpect: 100-continue\r\n", length)
	}
	hb.WriteString("\r\n")
	if _, err := io.WriteString(conn, hb.String()); err != nil {
		conn.Close()
		return 0, nil, nil, nil, err
	}
	br := bufio.NewReaderSize(conn, 32<<10)
	readStatus := func() (int, error) {
		line, err := br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0, fmt.Errorf("bad status line %q", strings.TrimSpace(line))
		}
		st, _ := strconv.Atoi(f[1])
		return st, nil
	}
	status, err := readStatus()
	if err != nil {
		conn.Close()
		return 0, nil, nil, nil, err
	}
	if body != nil && status == 100 {
		for { // skip the (empty) 100 headers
			h, err := br.ReadString('\n')
			if err != nil || strings.TrimRight(h, "\r\n") == "" {
				break
			}
		}
		if _, err := io.Copy(conn, body); err != nil {
			conn.Close()
			return 0, nil, nil, nil, err
		}
		if status, err = readStatus(); err != nil {
			conn.Close()
			return 0, nil, nil, nil, err
		}
	}
	hdr := map[string]string{}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return 0, nil, nil, nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok {
			hdr[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	n, _ := strconv.ParseInt(hdr["content-length"], 10, 64)
	var rd io.Reader = br
	if hdr["content-length"] != "" {
		rd = io.LimitReader(br, n)
	}
	return status, hdr, rd, conn, nil
}
