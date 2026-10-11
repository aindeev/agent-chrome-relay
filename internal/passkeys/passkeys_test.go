package passkeys

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// softChip stands in for the Secure Enclave: keys kept in memory, the "handle" an index. Records each reason.
type softChip struct {
	keys    []*ecdsa.PrivateKey
	reasons []string
	deny    bool
}

func (c *softChip) Create() ([]byte, []byte, error) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c.keys = append(c.keys, k)
	return []byte{byte(len(c.keys) - 1)}, pub(k), nil
}

// pub: the uncompressed point, as the Secure Enclave gives it
func pub(k *ecdsa.PrivateKey) []byte { b, _ := k.PublicKey.Bytes(); return b }

func (c *softChip) Sign(handle, data []byte, reason string) ([]byte, error) {
	c.reasons = append(c.reasons, reason)
	if c.deny {
		return nil, errors.New("the person did not approve with Touch ID")
	}
	h := sha256.Sum256(data)
	return ecdsa.SignASN1(rand.Reader, c.keys[handle[0]], h[:])
}

func dec(t *testing.T, s any) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s.(string))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func creation(t *testing.T, rpID string, algs ...int) CreationOptions {
	var pk CreationOptions
	params := []map[string]any{}
	for _, a := range algs {
		params = append(params, map[string]any{"type": "public-key", "alg": a})
	}
	raw, _ := json.Marshal(map[string]any{"challenge": map[string]string{"$b": "AAECAw"}, "rp": map[string]string{"id": rpID, "name": "Example"},
		"user": map[string]any{"id": map[string]string{"$b": "AQID"}, "name": "bot@example.com"}, "pubKeyCredParams": params, "extensions": map[string]any{"credProps": true}})
	if err := json.Unmarshal(raw, &pk); err != nil {
		t.Fatal(err)
	}
	return pk
}

func TestCreatedPasskeySignsInAndVerifies(t *testing.T) {
	chip := &softChip{}
	origin := "https://login.example.com"
	key, cred, err := Register(creation(t, "example.com", -257, -7), origin, "login.example.com", chip)
	if err != nil {
		t.Fatal(err)
	}
	var cd map[string]any
	json.Unmarshal(dec(t, cred.Response["clientDataJSON"]), &cd)
	if cd["type"] != "webauthn.create" || cd["origin"] != origin || cd["challenge"] != "AAECAw" || cd["crossOrigin"] != false {
		t.Fatalf("clientData %v", cd)
	}
	ad := dec(t, cred.Response["authenticatorData"])
	rp := sha256.Sum256([]byte("example.com"))
	credID := dec(t, cred.ID)
	if !bytes.Equal(ad[:32], rp[:]) || ad[32] != 0x45 || int(ad[53])<<8|int(ad[54]) != len(credID) || !bytes.Equal(ad[55:55+len(credID)], credID) {
		t.Fatalf("authenticator data %x", ad)
	}
	pub, err := x509.ParsePKIXPublicKey(dec(t, cred.Response["publicKey"]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dec(t, cred.Response["attestationObject"]), []byte("none")) || cred.ClientExtensionResults["credProps"] == nil {
		t.Fatal("attestation none and credProps")
	}
	if key.KeyHandle == "" || key.UserHandle != "AQID" || key.UserName != "bot@example.com" {
		t.Fatalf("stored passkey %+v", key)
	}

	var req RequestOptions
	json.Unmarshal([]byte(`{"challenge":{"$b":"BAUG"},"rpId":"example.com"}`), &req)
	got, err := Authenticate(req, key, origin, chip, "let Claude Code sign in to example.com as bot@example.com")
	if err != nil {
		t.Fatal(err)
	}
	ad2, cd2 := dec(t, got.Response["authenticatorData"]), dec(t, got.Response["clientDataJSON"])
	h := sha256.Sum256(cd2)
	signed := sha256.Sum256(append(append([]byte{}, ad2...), h[:]...))
	if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), signed[:], dec(t, got.Response["signature"])) {
		t.Fatal("the site cannot verify the sign-in")
	}
	if got.ID != cred.ID || got.Response["userHandle"] != "AQID" || ad2[32]&0x05 != 0x05 {
		t.Fatalf("assertion %+v", got)
	}
	if chip.reasons[0] != "let Claude Code sign in to example.com as bot@example.com" {
		t.Fatalf("Touch ID reason %q", chip.reasons)
	}
}

func TestRefusals(t *testing.T) {
	chip := &softChip{}
	if _, _, err := Register(creation(t, "example.com", -257), "https://example.com", "example.com", chip); err.(*Error).Name != "NotSupportedError" {
		t.Fatal(err)
	}
	if _, _, err := Register(creation(t, "other.com", -7), "https://example.com", "example.com", chip); err.(*Error).Name != "SecurityError" {
		t.Fatal(err)
	}
	key, _, _ := Register(creation(t, "example.com", -7), "https://example.com", "example.com", chip)
	chip.deny = true
	if _, err := Authenticate(RequestOptions{}, key, "https://example.com", chip, "x"); err == nil || err.(*Error).Name != "NotAllowedError" {
		t.Fatal("declined Touch ID must refuse", err)
	}
	for _, c := range []struct {
		rp, host string
		ok       bool
	}{{"example.com", "example.com", true}, {"example.com", "a.example.com", true}, {"localhost", "localhost", true},
		{"com", "example.com", false}, {"other.com", "example.com", false}, {"a.example.com", "example.com", false}} {
		if RPIDAllowed(c.rp, c.host) != c.ok {
			t.Errorf("RPIDAllowed(%q, %q)", c.rp, c.host)
		}
	}
}
