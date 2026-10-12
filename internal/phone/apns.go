package phone

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// APNs sends Apple push notifications straight from the Mac, with the person's own APNs auth key (Apple
// Developer > Keys): no server in between. The push says only that an approval is waiting; the request itself
// still travels encrypted, and only the phone's Face ID signature approves.
type APNs struct {
	KeyID  string `json:"keyId"`
	TeamID string `json:"teamId"`
	PEM    string `json:"key"` // the .p8 file

	mu     sync.Mutex
	jwt    string
	jwtAt  time.Time
	Client *http.Client              `json:"-"`
	Base   func(sandbox bool) string `json:"-"` // tests
}

// Device is where a phone receives pushes: its token, its app (the topic) and Apple's sandbox or production.
type Device struct {
	Token   string `json:"token"` // hex
	Topic   string `json:"topic"` // the app's bundle id
	Sandbox bool   `json:"sandbox"`
}

// appleID: Apple's 10-character key and team IDs (uppercase letters and digits).
func appleID(s string) bool {
	if len(s) != 10 {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Validate checks the key ID and team ID look like Apple's, and that the key is a .p8 that signs a provider
// token, before it is saved.
func (a *APNs) Validate() error {
	if !appleID(a.KeyID) {
		return fmt.Errorf("the key ID %q is not an Apple key ID (10 letters and digits, as in AuthKey_<KEYID>.p8)", a.KeyID)
	}
	if !appleID(a.TeamID) {
		return fmt.Errorf("the team ID %q is not an Apple team ID (10 letters and digits, Apple Developer > Membership details)", a.TeamID)
	}
	if _, err := a.key(); err != nil {
		return fmt.Errorf("the APNs key can't be read: %w", err)
	}
	_, err := a.token()
	return err
}

func (a *APNs) key() (*ecdsa.PrivateKey, error) {
	b, _ := pem.Decode([]byte(a.PEM))
	if b == nil {
		return nil, errors.New("the APNs key is not a .p8 file")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the APNs key is not an EC key")
	}
	return ek, nil
}

// token: Apple's provider JWT (ES256), reused for 50 minutes as Apple asks.
func (a *APNs) token() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jwt != "" && time.Since(a.jwtAt) < 50*time.Minute {
		return a.jwt, nil
	}
	k, err := a.key()
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.KeyID})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iss": a.TeamID, "iat": now.Unix()})
	signing := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS wants r||s, 32 bytes each
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	a.jwt, a.jwtAt = signing+"."+enc.EncodeToString(sig), now
	return a.jwt, nil
}

// Push sends an alert to d. id lets the app open straight to the request.
func (a *APNs) Push(ctx context.Context, d Device, title, body, id string) error {
	tok, err := a.token()
	if err != nil {
		return err
	}
	base := "https://api.push.apple.com"
	if d.Sandbox {
		base = "https://api.sandbox.push.apple.com"
	}
	if a.Base != nil {
		base = a.Base(d.Sandbox)
	}
	payload, _ := json.Marshal(map[string]any{
		"aps": map[string]any{"alert": map[string]string{"title": title, "body": body}, "sound": "default",
			"interruption-level": "time-sensitive"},
		"request": id,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+d.Token, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+tok)
	req.Header.Set("apns-topic", d.Topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-expiration", fmt.Sprint(time.Now().Add(5*time.Minute).Unix()))
	c := a.Client
	if c == nil {
		c = http.DefaultClient // HTTP/2 over TLS, as APNs requires
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e struct{ Reason string }
		b, _ := io.ReadAll(res.Body)
		json.Unmarshal(b, &e)
		if e.Reason == "" {
			e.Reason = res.Status
		}
		return fmt.Errorf("Apple push: %s", e.Reason)
	}
	return nil
}
