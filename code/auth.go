package main

// Passwords and sessions.
//
// Passwords are hashed with PBKDF2-HMAC-SHA256 (600 000 iterations, 16-byte salt) from
// the standard library (Go 1.24+). argon2id would be marginally better against GPU
// attackers but needs golang.org/x/crypto; PBKDF2 at this cost is the OWASP-recommended
// stdlib-only choice and costs ~0.3 s per login, which is itself a brute-force brake.
//
// Clients never store the password. A successful login returns a random 256-bit session
// token; the hub stores only its SHA-256, so a leaked state.json can't be replayed.

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

var hashIter = 600_000 // lowered in tests

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	hex.Decode(salt, []byte(randHex(16)))
	key, _ := pbkdf2.Key(sha256.New, pw, salt, hashIter, 32)
	return fmt.Sprintf("$pbkdf2-sha256$%d$%s$%s", hashIter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 5 || parts[1] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// newSessionToken returns the raw token (given to the client once) and its hash (stored).
func newSessionToken() (raw, hash string) {
	raw = randHex(32)
	return raw, sessHash(raw)
}

func sessHash(raw string) string {
	s := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(s[:])
}

// shortSess is the id users see in /sessions (first 8 hex chars of the hash).
func shortSess(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// fnvID gives guests a stable negative pseudo user id derived from their nick, so
// presence, clipboard routing and rate limits work for them without a stored account.
func guestID(nick string) int64 {
	var h uint32 = 2166136261
	for i := 0; i < len(nick); i++ {
		h ^= uint32(nick[i])
		h *= 16777619
	}
	return -int64(h&0x7fffffff) - 1
}
