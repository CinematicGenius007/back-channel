package main

// End-to-end encrypted channels (optional, off by default — see ENCRYPTION.md for the
// full design, protocol and threat model). Summary:
//
//   - Every device holds a long-term X25519 identity keypair, generated on first use and
//     kept in the device's own config.json. The public half is registered with the hub.
//   - A channel created with e2e=on has a random 32-byte AES-256 key. It is generated on
//     whichever device creates the channel and never sent to the hub in the clear.
//   - Handing the key to another device is done by wrapping it (ECDH + AES-256-GCM) to
//     that device's public key and relaying the wrapped bytes through the hub, which
//     cannot decrypt them: it only ever sees random-looking blobs, whoever they're for.
//   - Messages and files posted to such a channel are sealed client-side with the
//     channel's key before they are ever sent to the hub.
//
// Everything here is standard library: crypto/ecdh (X25519, Go 1.20+) and crypto/aes +
// crypto/cipher (AES-256-GCM). No golang.org/x/crypto is needed.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// genDeviceKey creates a new X25519 identity for this device.
func genDeviceKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

func encodeKey(b []byte) string          { return base64.RawStdEncoding.EncodeToString(b) }
func decodeKey(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }

func pubFromPriv(priv *ecdh.PrivateKey) string { return encodeKey(priv.PublicKey().Bytes()) }

func parsePub(s string) (*ecdh.PublicKey, error) {
	b, err := decodeKey(s)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(b)
}

func parsePriv(s string) (*ecdh.PrivateKey, error) {
	b, err := decodeKey(s)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(b)
}

// aad binds ciphertext to the channel and key epoch it belongs to. Without this, a
// wrapped key or a message from one channel/epoch could be replayed into another and
// would still decrypt — binding it turns that into an authentication failure instead.
func aad(ch string, epoch int) []byte {
	b := make([]byte, len(ch)+8)
	copy(b, ch)
	binary.BigEndian.PutUint64(b[len(ch):], uint64(epoch))
	return b
}

func gcmSeal(key, plaintext, ad []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, plaintext, ad)
	return base64.StdEncoding.EncodeToString(out), nil
}

func gcmOpen(key []byte, payload string, ad []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, ad)
}

// encryptText / decryptText seal a UTF-8 message body or a file name for one channel epoch.
func encryptText(key []byte, ch string, epoch int, plaintext string) (string, error) {
	return gcmSeal(key, []byte(plaintext), aad(ch, epoch))
}

func decryptText(key []byte, ch string, epoch int, payload string) (string, error) {
	pt, err := gcmOpen(key, payload, aad(ch, epoch))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// encryptFile / decryptFile seal whole file contents in one AES-256-GCM call. AES-GCM's
// safe plaintext limit for one nonce (~64 GB) is far above any realistic upload, so a
// single seal is used rather than a chunked stream cipher — simpler and still correct,
// at the cost of holding the whole file in memory on both ends. See ENCRYPTION.md.
func encryptFile(key []byte, ch string, epoch int, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, aad(ch, epoch)), nil
}

func decryptFile(key []byte, ch string, epoch int, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, aad(ch, epoch))
}

// deriveWrapKey turns an ECDH shared secret into an AES-256 key for wrapping one
// channel's key material. Binding it to the channel name means the same two devices
// wrapping keys for two different channels never reuse a wrapping key.
func deriveWrapKey(shared []byte, ch string) []byte {
	h := sha256.New()
	h.Write(shared)
	h.Write([]byte("backchannel-e2e-wrap:"))
	h.Write([]byte(ch))
	return h.Sum(nil)
}

// wrapChannelKey lets the holder of `priv` hand a channel key to whoever holds
// `peerPubB64`, via ECDH followed by AES-256-GCM. ECDH is symmetric, so the recipient
// computes the identical wrapping key from their own private key and this device's
// public key — see unwrapChannelKey.
func wrapChannelKey(priv *ecdh.PrivateKey, peerPubB64, ch string, epoch int, key []byte) (string, error) {
	peerPub, err := parsePub(peerPubB64)
	if err != nil {
		return "", fmt.Errorf("bad peer key: %w", err)
	}
	shared, err := priv.ECDH(peerPub)
	if err != nil {
		return "", err
	}
	return gcmSeal(deriveWrapKey(shared, ch), key, aad(ch, epoch))
}

func unwrapChannelKey(priv *ecdh.PrivateKey, peerPubB64, ch string, epoch int, wrapped string) ([]byte, error) {
	peerPub, err := parsePub(peerPubB64)
	if err != nil {
		return nil, fmt.Errorf("bad peer key: %w", err)
	}
	shared, err := priv.ECDH(peerPub)
	if err != nil {
		return nil, err
	}
	return gcmOpen(deriveWrapKey(shared, ch), wrapped, aad(ch, epoch))
}

// newChannelKey generates a fresh random 32-byte AES-256 key for a channel epoch.
func newChannelKey() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}

// keyFingerprint renders a device public key the way a person compares it out of band:
// short, grouped hex — the same idea as the TLS certificate fingerprint pin.
func keyFingerprint(pubB64 string) string {
	b, err := decodeKey(pubB64)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(b)
	s := fmt.Sprintf("%x", sum[:8])
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}
