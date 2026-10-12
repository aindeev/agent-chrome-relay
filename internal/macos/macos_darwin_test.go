//go:build darwin && cgo

package macos

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
)

// TestSilentKeysOnThisMac uses the real Secure Enclave: silent keys sign and unlock with no prompt, and keep
// working while the screen is locked (the person is away). It runs only with CHROME_RELAY_MAC_TEST set:
//
//	CHROME_RELAY_MAC_TEST=1       make a key and a locked secret, and use them, now
//	CHROME_RELAY_MAC_TEST=make    make them (screen unlocked) and keep them in $CHROME_RELAY_MAC_TEST_STATE
//	CHROME_RELAY_MAC_TEST=use     use the kept ones (e.g. with the screen locked)
func TestSilentKeysOnThisMac(t *testing.T) {
	phase, state := os.Getenv("CHROME_RELAY_MAC_TEST"), os.Getenv("CHROME_RELAY_MAC_TEST_STATE")
	if phase == "" {
		t.Skip("set CHROME_RELAY_MAC_TEST=1 on a Mac with a Secure Enclave")
	}
	var s struct{ Handle, Pub, Locked []byte }
	if phase != "use" {
		var err error
		if s.Handle, s.Pub, err = PasskeyCreate(true); err != nil {
			t.Fatal("make a silent key:", err)
		}
		if s.Locked, err = Lock([]byte("ops_secret"), true); err != nil || !Silent(s.Locked) {
			t.Fatal("lock silently:", err)
		}
		if phase == "make" {
			b, _ := json.Marshal(s)
			os.WriteFile(state, b, 0o600)
			return
		}
	} else {
		b, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(b, &s)
	}
	data := []byte("authenticator data + client data hash")
	sig, err := PasskeySign(s.Handle, data, "", "", 0)
	if err != nil {
		t.Fatal("a silent key signs without Touch ID:", err)
	}
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), s.Pub)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	if !ecdsa.VerifyASN1(k, h[:], sig) {
		t.Fatal("bad signature")
	}
	plain, err := Unlock(s.Locked, "should not be asked")
	if err != nil || !bytes.Equal(plain, []byte("ops_secret")) {
		t.Fatal("a silent lock opens without Touch ID:", err)
	}
}

// TestKeychainOnThisMac: Chrome Relay's own Keychain item, written and read back (CHROME_RELAY_MAC_TEST=keychain),
// e.g. with the screen locked.
func TestKeychainOnThisMac(t *testing.T) {
	if os.Getenv("CHROME_RELAY_MAC_TEST") != "keychain" {
		t.Skip("set CHROME_RELAY_MAC_TEST=keychain")
	}
	if err := KeychainSet("Chrome Relay test", "probe", []byte("x")); err != nil {
		t.Fatal("write:", err)
	}
	defer KeychainDelete("Chrome Relay test", "probe")
	if b, err := KeychainGet("Chrome Relay test", "probe"); err != nil || string(b) != "x" {
		t.Fatal("read:", err)
	}
}
