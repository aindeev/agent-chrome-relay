package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
)

// a throwaway key in Apple's .p8 shape (PKCS#8 PEM); never a real one
func p8(t *testing.T, ec bool) []byte {
	t.Helper()
	var k any
	var err error
	if ec {
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	} else {
		k, err = rsa.GenerateKey(rand.Reader, 1024)
	}
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestParsePush(t *testing.T) {
	good, rsaKey := p8(t, true), p8(t, false)
	files := map[string][]byte{
		"/dl/AuthKey_ABCDE12345.p8": good,
		"/dl/apns.p8":               good,
		"/dl/AuthKey_SHORT.p8":      good,
		"/dl/AuthKey_RSAKEY0001.p8": rsaKey,
		"/dl/AuthKey_GARBAGE001.p8": []byte("not a key"),
	}
	read := func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	cases := []struct {
		args          []string
		keyID, teamID string
		err           string // a substring of the error; "" = ok
	}{
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8", "--team-id", "A1B2C3D4E5"}, "ABCDE12345", "A1B2C3D4E5", ""},
		{[]string{"--key=/dl/AuthKey_ABCDE12345.p8", "--team-id=a1b2c3d4e5"}, "ABCDE12345", "A1B2C3D4E5", ""},
		{[]string{"--key", "/dl/apns.p8", "--key-id", "ZZZZZ99999", "--team-id", "A1B2C3D4E5"}, "ZZZZZ99999", "A1B2C3D4E5", ""},
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8", "--key-id", "ZZZZZ99999", "--team-id", "A1B2C3D4E5"}, "ZZZZZ99999", "A1B2C3D4E5", ""},
		{[]string{"--key", "/dl/apns.p8", "--team-id", "A1B2C3D4E5"}, "", "", "--key-id is required"},
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8"}, "", "", "--team-id is required"},
		{[]string{"--team-id", "A1B2C3D4E5"}, "", "", "--key"},
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8", "--team-id"}, "", "", "needs a value"},
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8", "--team-id", "A1B2C3D4E5", "--sandbox"}, "", "", "unknown argument"},
		{[]string{"--key", "/dl/AuthKey_SHORT.p8", "--team-id", "A1B2C3D4E5"}, "", "", "not an Apple key ID"},
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8", "--team-id", "TEAM"}, "", "", "not an Apple team ID"},
		{[]string{"--key", "/dl/AuthKey_ABCDE12345.p8", "--team-id", "A1B2C3D4E!"}, "", "", "not an Apple team ID"},
		{[]string{"--key", "/dl/AuthKey_RSAKEY0001.p8", "--team-id", "A1B2C3D4E5"}, "", "", "not an EC key"},
		{[]string{"--key", "/dl/AuthKey_GARBAGE001.p8", "--team-id", "A1B2C3D4E5"}, "", "", "not a .p8 file"},
		{[]string{"--key", "/dl/AuthKey_MISSING001.p8", "--team-id", "A1B2C3D4E5"}, "", "", "not exist"},
	}
	for _, c := range cases {
		a, path, err := parsePush(c.args, read)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("parsePush(%v): err %v, want one containing %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePush(%v): %v", c.args, err)
			continue
		}
		if a.KeyID != c.keyID || a.TeamID != c.teamID || a.PEM != string(good) || !strings.HasSuffix(path, ".p8") {
			t.Errorf("parsePush(%v) = key %s team %s path %s, want key %s team %s", c.args, a.KeyID, a.TeamID, path, c.keyID, c.teamID)
		}
	}
}
