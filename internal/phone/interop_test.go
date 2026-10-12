package phone

import (
	"crypto/ecdh"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSwiftInterop runs the iPhone app's crypto (CryptoKit, in testdata/phone.swift) against this package.
// It needs swift, so it runs only with CHROME_RELAY_SWIFT_INTEROP=1.
func TestSwiftInterop(t *testing.T) {
	if os.Getenv("CHROME_RELAY_SWIFT_INTEROP") != "1" {
		t.Skip("set CHROME_RELAY_SWIFT_INTEROP=1 (needs swift)")
	}
	state := filepath.Join(t.TempDir(), "state.json")
	swift := func(args ...string) []byte {
		out, err := exec.Command("swift", append([]string{"testdata/phone.swift"}, args...)...).Output()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return out
	}
	o, _ := NewOffer("https://ntfy.example", "Mac")
	p, err := o.Accept(swift("pair", o.URL(), state))
	if err != nil {
		t.Fatal("Go can't accept CryptoKit's pairing:", err)
	}
	r := Request{ID: "abc_-123", Mac: "Mac", Reason: "let Claude Code fill a password", Created: time.Now().UTC().Truncate(time.Second)}
	r.Expires = r.Created.Add(time.Minute)
	msg, plain, err := p.sealRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ecdh.P256().NewPrivateKey(p.RelayKey)
	if err := p.check(swift("answer", b64u.EncodeToString(k.PublicKey().Bytes()), string(msg), state), r.ID, plain); err != nil {
		t.Fatal("CryptoKit's signed answer didn't verify:", err)
	}
}
