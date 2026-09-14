package main

// Tests for end-to-end encrypted channels: the crypto primitives in isolation, and the
// full hub-relayed key exchange (create -> invite -> join -> keyreq -> keyshare ->
// encrypted message), verified against the raw bytes the hub actually stores and
// broadcasts — never plaintext, never the key itself.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestE2ECryptoRoundTrip(t *testing.T) {
	key, err := newChannelKey()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryptText(key, "design", 1, "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, "hello") {
		t.Fatal("ciphertext contains the plaintext")
	}
	pt, err := decryptText(key, "design", 1, ct)
	if err != nil || pt != "hello world" {
		t.Fatalf("decrypt: %q, %v", pt, err)
	}
	// wrong channel (AAD mismatch) must fail closed, not just decrypt garbage
	if _, err := decryptText(key, "other", 1, ct); err == nil {
		t.Fatal("decrypted under the wrong channel name")
	}
	if _, err := decryptText(key, "design", 2, ct); err == nil {
		t.Fatal("decrypted under the wrong epoch")
	}
	wrongKey, err := newChannelKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptText(wrongKey, "design", 1, ct); err == nil {
		t.Fatal("decrypted with the wrong key")
	}

	file := bytes.Repeat([]byte("file bytes "), 1000)
	fct, err := encryptFile(key, "design", 1, file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(fct, []byte("file bytes")) {
		t.Fatal("file ciphertext contains plaintext")
	}
	fpt, err := decryptFile(key, "design", 1, fct)
	if err != nil || !bytes.Equal(fpt, file) {
		t.Fatalf("file round trip failed: %v", err)
	}
	fct[len(fct)-1] ^= 0xFF // tamper with the auth tag
	if _, err := decryptFile(key, "design", 1, fct); err == nil {
		t.Fatal("tampered file ciphertext accepted")
	}
}

func TestE2EKeyWrapRoundTrip(t *testing.T) {
	alicePriv, err := genDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	bobPriv, err := genDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	alicePub, bobPub := pubFromPriv(alicePriv), pubFromPriv(bobPriv)
	key, err := newChannelKey()
	if err != nil {
		t.Fatal(err)
	}

	wrapped, err := wrapChannelKey(alicePriv, bobPub, "design", 1, key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wrapped, string(key)) {
		t.Fatal("wrapped blob contains the raw key")
	}
	got, err := unwrapChannelKey(bobPriv, alicePub, "design", 1, wrapped)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("unwrap: %v", err)
	}
	// a third party's key must not unwrap it
	evePriv, _ := genDeviceKey()
	if _, err := unwrapChannelKey(evePriv, alicePub, "design", 1, wrapped); err == nil {
		t.Fatal("eve unwrapped alice's share to bob")
	}
	// wrong channel or epoch must fail
	if _, err := unwrapChannelKey(bobPriv, alicePub, "other", 1, wrapped); err == nil {
		t.Fatal("unwrapped under the wrong channel")
	}
}

func TestKeyFingerprintStable(t *testing.T) {
	priv, _ := genDeviceKey()
	pub := pubFromPriv(priv)
	if keyFingerprint(pub) != keyFingerprint(pub) {
		t.Fatal("fingerprint not deterministic")
	}
	other, _ := genDeviceKey()
	if keyFingerprint(pub) == keyFingerprint(pubFromPriv(other)) {
		t.Fatal("two different keys produced the same fingerprint")
	}
}

func TestCreatedE2EChannelGetsInitialKey(t *testing.T) {
	c := &client{dir: t.TempDir()}
	c.initCreatedE2EKeys([]ChanInfo{{Name: "secret", E2E: true, Epoch: 1}})
	key, ok := c.chanKey("secret", 1)
	if !ok || len(key) != 32 {
		t.Fatalf("created channel key missing or wrong length: %d, %v", len(key), ok)
	}
}

// ---- full hub-relayed flow ----------------------------------------------------------
//
// These drive the raw `tc` test connection directly with keyreq/keyshare/msg frames —
// exactly what a real client sends — rather than instantiating a full *client, so the
// assertions are against the actual bytes the hub relays and stores.

func TestE2EFullFlow(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	ok := owner.login("owner", th.pass)
	ownerPriv, _ := genDeviceKey()
	ownerPub := pubFromPriv(ownerPriv)
	// re-hello with a device pub so the hub's directory has it (login() doesn't send one)
	owner = th.dial(t)
	owner.send(Msg{T: "hello", V: 2, Device: "test", Auth: &Auth{Session: ok.Session}, Pub: ownerPub})
	owner.recvT("ok")

	res := mustOK(t, owner.cmd("create", "", map[string]string{"name": "secret", "e2e": "on"}))
	if !res.Channels[0].E2E || res.Channels[0].Epoch != 1 {
		t.Fatalf("create e2e: %+v", res.Channels[0])
	}
	owner.sub("secret")
	channelKey, err := newChannelKey()
	if err != nil {
		t.Fatal(err)
	}

	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "bob", "pass": "bobpassword"}))
	bobPriv, _ := genDeviceKey()
	bobPub := pubFromPriv(bobPriv)
	bob := th.dial(t)
	bob.send(Msg{T: "hello", V: 2, Device: "bobpc", Auth: &Auth{User: "bob", Pass: "bobpassword"}, Pub: bobPub})
	bobOK := bob.recvT("ok")
	if bobOK.T != "ok" {
		t.Fatalf("bob login: %+v", bobOK)
	}
	mustOK(t, owner.cmd("add", "secret", map[string]string{"user": "bob"}))
	bob.recvT("invited")
	bob.sub("secret")
	owner.recvT("join")

	// bob asks for the key; owner (who has it) answers via the hub
	bob.send(Msg{T: "keyreq", Ch: "secret", Epoch: 1})
	req := owner.recvT("keyreq")
	if req.FromUser != "bob" || req.FromDev != "bobpc" || req.FromPub != bobPub {
		t.Fatalf("keyreq relay: %+v", req)
	}
	wrapped, err := wrapChannelKey(ownerPriv, req.FromPub, "secret", 1, channelKey)
	if err != nil {
		t.Fatal(err)
	}
	owner.send(Msg{T: "keyshare", Ch: "secret", Epoch: 1, ToUser: "bob", ToDevice: "bobpc", FromPub: ownerPub, Wrapped: wrapped})
	share := bob.recvT("keyshare")
	if share.ToUser != "bob" || share.ToDevice != "bobpc" {
		t.Fatalf("keyshare relay: %+v", share)
	}
	got, err := unwrapChannelKey(bobPriv, share.FromPub, "secret", 1, share.Wrapped)
	if err != nil || !bytes.Equal(got, channelKey) {
		t.Fatalf("bob unwrap: %v", err)
	}

	// a non-member must never receive a keyshare aimed at bob, even if it asks
	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "eve", "pass": "evepassword1"}))
	eve := th.dial(t)
	eve.login("eve", "evepassword1")
	if m := eve.cmd("e2epeers", "secret", nil); m.OK {
		t.Fatal("non-member ran e2epeers")
	}

	// owner posts an encrypted message; bob (who now has the key) can read it, the hub
	// log holds only ciphertext
	ct, err := encryptText(channelKey, "secret", 1, "the launch codes are safe here")
	if err != nil {
		t.Fatal(err)
	}
	owner.send(Msg{T: "msg", Ch: "secret", Text: ct, Epoch: 1})
	m1 := owner.recvT("msg")
	if m1.Epoch != 1 || strings.Contains(m1.Text, "launch") {
		t.Fatalf("plaintext leaked to the wire: %+v", m1)
	}
	m2 := bob.recvT("msg")
	pt, err := decryptText(channelKey, "secret", m2.Epoch, m2.Text)
	if err != nil || pt != "the launch codes are safe here" {
		t.Fatalf("bob decrypt: %q %v", pt, err)
	}

	// the on-disk log must never contain the plaintext either
	th.h.mu.Lock()
	th.h.compactLog()
	th.h.mu.Unlock()
	raw, err := readFileBytes(t, filepath.Join(th.dir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("launch codes")) {
		t.Fatal("plaintext found in messages.jsonl")
	}
	var logged Msg
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if json.Unmarshal(line, &logged) == nil && logged.Ch == "secret" && logged.T == "msg" {
			if logged.Text != ct {
				t.Fatalf("stored text != what was sent: %q vs %q", logged.Text, ct)
			}
		}
	}

	// eve (not a member) gets the standard no-such-channel treatment, never a peek
	eve.send(Msg{T: "sub", Ch: "secret", RID: "x"})
	if e := eve.recv(); e.Code != codeNoChan {
		t.Fatalf("eve saw #secret: %+v", e)
	}
}

func TestE2ERotateOnlyModPlus(t *testing.T) {
	th := startHub(t)
	owner := th.dial(t)
	owner.login("owner", th.pass)
	mustOK(t, owner.cmd("create", "", map[string]string{"name": "vault", "e2e": "on"}))
	mustOK(t, owner.cmd("useradd", "", map[string]string{"name": "carl", "pass": "carlpass11"}))
	carl := th.dial(t)
	carl.login("carl", "carlpass11")
	mustOK(t, owner.cmd("add", "vault", map[string]string{"user": "carl"}))
	carl.recvT("invited")

	if m := carl.cmd("e2erotate", "vault", nil); m.OK || m.Code != codePerm {
		t.Fatalf("member rotated: %+v", m)
	}
	res := mustOK(t, owner.cmd("e2erotate", "vault", nil))
	if len(res.Channels) == 0 || res.Channels[0].Epoch != 2 {
		t.Fatalf("rotate: %+v", res)
	}
	// a non-e2e channel refuses rotation outright
	mustOK(t, owner.cmd("create", "", map[string]string{"name": "plain"}))
	if m := owner.cmd("e2erotate", "plain", nil); m.OK {
		t.Fatal("rotated a non-e2e channel")
	}
}

func readFileBytes(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(path)
}
