// Package passkeys builds WebAuthn messages for Chrome Relay's authenticator. Chrome refuses WebAuthn in a tab
// without focus (every agent tab), so agent pages hand navigator.credentials.create/get to the relay, which
// answers as the authenticator; the keys themselves are made by this Mac's Secure Enclave and sign there, each
// signature approved with Touch ID (a Chip). Nothing here ever holds a private key. ES256, attestation "none".
package passkeys

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Chip creates and uses keys that never leave it (macos.PasskeyCreate / PasskeySign, or a test double).
type Chip interface {
	Create() (handle, publicKey []byte, err error)
	Sign(handle, data []byte, reason string) ([]byte, error)
}

// Error carries the DOMException name the page receives (NotAllowedError, SecurityError, ...).
type Error struct{ Name, Message string }

func (e *Error) Error() string                 { return e.Message }
func fail(name, format string, a ...any) error { return &Error{name, fmt.Sprintf(format, a...)} }

// Bytes is a BufferSource from the page: the shim sends {"$b": base64url}.
type Bytes []byte

func (b *Bytes) UnmarshalJSON(data []byte) error {
	var v struct {
		B string `json:"$b"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	d, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(v.B, "="))
	*b = d
	return err
}

type Descriptor struct {
	Type string `json:"type"`
	ID   Bytes  `json:"id"`
}

// CreationOptions is navigator.credentials.create's publicKey; RequestOptions is get's.
type CreationOptions struct {
	Challenge Bytes `json:"challenge"`
	RP        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"rp"`
	User struct {
		ID          Bytes  `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
	PubKeyCredParams []struct {
		Type string `json:"type"`
		Alg  int    `json:"alg"`
	} `json:"pubKeyCredParams"`
	ExcludeCredentials []Descriptor   `json:"excludeCredentials"`
	Extensions         map[string]any `json:"extensions"`
}

type RequestOptions struct {
	Challenge        Bytes        `json:"challenge"`
	RPID             string       `json:"rpId"`
	AllowCredentials []Descriptor `json:"allowCredentials"`
}

// IDs of the credentials a request names (allowCredentials / excludeCredentials), base64url.
func IDs(ds []Descriptor) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = b64u(d.ID)
	}
	return out
}

// Passkey is what the vault keeps for a passkey Chrome Relay made: no private key, only the public key and the
// chip's handle for it (useless on any other Mac). All binary values base64url.
type Passkey struct {
	RPID, CredentialID, UserHandle, UserName, PublicKey, KeyHandle string
	Item, ItemID, Device                                           string
}

// Credential is the PublicKeyCredential the page gets (rebuilt into real-looking objects by the page shim).
type Credential struct {
	ID                      string         `json:"id"`
	Type                    string         `json:"type"`
	AuthenticatorAttachment string         `json:"authenticatorAttachment"`
	ClientExtensionResults  map[string]any `json:"clientExtensionResults"`
	Response                map[string]any `json:"response"`
}

func b64u(b []byte) string   { return base64.RawURLEncoding.EncodeToString(b) }
func digest(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// RPIDAllowed: may a page on host act for relying party rpID? The WebAuthn rule: rpID is the host or a parent
// domain of it, and never a bare top-level domain.
func RPIDAllowed(rpID, host string) bool {
	rpID, host = strings.ToLower(rpID), strings.ToLower(host)
	if rpID == "" || (!strings.Contains(rpID, ".") && rpID != "localhost") {
		return false
	}
	return host == rpID || strings.HasSuffix(host, "."+rpID)
}

// CBOR: the little WebAuthn needs (ints, byte and text strings, maps in the given order).
type entry struct{ k, v any }

func cbor(v any) []byte {
	head := func(major byte, n int) []byte {
		switch {
		case n < 24:
			return []byte{major<<5 | byte(n)}
		case n < 256:
			return []byte{major<<5 | 24, byte(n)}
		case n < 65536:
			return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
		}
		b := []byte{major<<5 | 26, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(b[1:], uint32(n))
		return b
	}
	switch x := v.(type) {
	case []byte:
		return append(head(2, len(x)), x...)
	case int:
		if x >= 0 {
			return head(0, x)
		}
		return head(1, -1-x)
	case string:
		return append(head(3, len(x)), x...)
	case []entry:
		out := head(5, len(x))
		for _, e := range x {
			out = append(append(out, cbor(e.k)...), cbor(e.v)...)
		}
		return out
	}
	panic(fmt.Sprintf("cbor: cannot encode %T", v))
}

// Flags: user present + verified (Touch ID); on creation also AT. Not backup-eligible: the key never leaves this Mac.
const flagsGet, flagsCreate = 0x01 | 0x04, 0x01 | 0x04 | 0x40

var spkiP256 = []byte{0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01, 0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00}

func clientData(typ string, challenge []byte, origin string) []byte {
	// field order matters to nobody, but keep the conventional one
	b, _ := json.Marshal(struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}{typ, b64u(challenge), origin, false})
	return b
}

// Register answers navigator.credentials.create: a new key in the chip. No signature is needed to register
// (attestation "none"), so this asks nothing of the person.
func Register(pk CreationOptions, origin, host string, chip Chip) (Passkey, Credential, error) {
	rpID := pk.RP.ID
	if rpID == "" {
		rpID = host
	}
	if !RPIDAllowed(rpID, host) {
		return Passkey{}, Credential{}, fail("SecurityError", "%s may not create passkeys for %s", host, rpID)
	}
	if !slices.ContainsFunc(pk.PubKeyCredParams, func(p struct {
		Type string `json:"type"`
		Alg  int    `json:"alg"`
	}) bool {
		return p.Alg == -7
	}) {
		return Passkey{}, Credential{}, fail("NotSupportedError", "Chrome Relay passkeys are ES256 only, and this site does not accept ES256")
	}
	handle, point, err := chip.Create()
	if err != nil {
		return Passkey{}, Credential{}, fail("NotAllowedError", "%v", err)
	}
	if len(point) != 65 {
		return Passkey{}, Credential{}, fail("NotAllowedError", "the chip returned an unexpected public key")
	}
	credID := make([]byte, 16)
	rand.Read(credID)
	cose := cbor([]entry{{1, 2}, {3, -7}, {-1, 1}, {-2, point[1:33]}, {-3, point[33:65]}})
	var authData bytes.Buffer
	authData.Write(digest([]byte(rpID)))
	authData.WriteByte(flagsCreate)
	authData.Write(make([]byte, 4))  // counter: always 0
	authData.Write(make([]byte, 16)) // AAGUID
	binary.Write(&authData, binary.BigEndian, uint16(len(credID)))
	authData.Write(credID)
	authData.Write(cose)
	cd := clientData("webauthn.create", pk.Challenge, origin)
	ext := map[string]any{}
	if pk.Extensions["credProps"] == true {
		ext["credProps"] = map[string]any{"rk": true}
	}
	key := Passkey{RPID: rpID, CredentialID: b64u(credID), UserHandle: b64u(pk.User.ID), UserName: pk.User.Name, PublicKey: b64u(point), KeyHandle: b64u(handle)}
	att := cbor([]entry{{"fmt", "none"}, {"attStmt", []entry{}}, {"authData", authData.Bytes()}})
	return key, Credential{ID: b64u(credID), Type: "public-key", AuthenticatorAttachment: "platform", ClientExtensionResults: ext,
		Response: map[string]any{"clientDataJSON": b64u(cd), "attestationObject": b64u(att), "authenticatorData": b64u(authData.Bytes()),
			"publicKey": b64u(append(slices.Clone(spkiP256), point...)), "publicKeyAlgorithm": -7, "transports": []string{"internal"}}}, nil
}

// Authenticate answers navigator.credentials.get with a stored passkey: the chip signs after Touch ID for reason.
func Authenticate(pk RequestOptions, key Passkey, origin string, chip Chip, reason string) (Credential, error) {
	authData := append(digest([]byte(key.RPID)), flagsGet, 0, 0, 0, 0)
	cd := clientData("webauthn.get", pk.Challenge, origin)
	handle, err := base64.RawURLEncoding.DecodeString(key.KeyHandle)
	if err != nil {
		return Credential{}, fail("NotAllowedError", "the vault's passkey for %s is damaged", key.RPID)
	}
	sig, err := chip.Sign(handle, append(slices.Clone(authData), digest(cd)...), reason)
	if err != nil {
		return Credential{}, fail("NotAllowedError", "%v", err)
	}
	var userHandle any
	if key.UserHandle != "" {
		userHandle = key.UserHandle
	}
	return Credential{ID: key.CredentialID, Type: "public-key", AuthenticatorAttachment: "platform", ClientExtensionResults: map[string]any{},
		Response: map[string]any{"clientDataJSON": b64u(cd), "authenticatorData": b64u(authData), "signature": b64u(sig), "userHandle": userHandle}}, nil
}
