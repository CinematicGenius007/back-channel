package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadRequestRejectsOversizedHeaders(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("GET / HTTP/1.1\r\nX-Large: " + strings.Repeat("x", maxHTTPLine) + "\r\n\r\n"))
	if _, err := readRequest(r); err == nil {
		t.Fatal("expected oversized header to be rejected")
	}
}

func TestReadRequestRejectsMalformedLengthAndEncoding(t *testing.T) {
	for _, raw := range []string{
		"GET / HTTP/1.1\r\nContent-Length: nope\r\n\r\n",
		"GET / HTTP/1.1\r\nContent-Length: -1\r\n\r\n",
		"GET / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n",
	} {
		r := bufio.NewReader(strings.NewReader(raw))
		if _, err := readRequest(r); err == nil {
			t.Fatalf("expected request to be rejected: %q", raw)
		}
	}
}

func TestReadRequestAcceptsBoundedRequest(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("PUT /up?name=a.txt HTTP/1.1\r\nContent-Length: 3\r\n\r\nabc"))
	req, err := readRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if req.method != "PUT" || req.path != "/up" || req.length != 3 {
		t.Fatalf("unexpected request: %#v", req)
	}
}
