package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/phone"
)

// The service account token lives in the login Keychain, in an item only this signed program reads freely, and
// even that holds it only locked to this Mac's Secure Enclave: a copy of the item is useless on any other Mac.
// Opening it asks nothing of the person; each sign-in is approved instead (Touch ID here, or the paired iPhone).
const keychainService = "Chrome Relay"

func keychainAccount(instance, key string) string {
	a := "1Password service account (locked to this Mac)"
	if instance != "" {
		a = "1Password service account (locked to this Mac, " + instance + ")"
	}
	if key != "" { // one per 1Password account; "" is the first kind, from before several were possible
		a += " " + key
	}
	return a
}

// Token is what the Keychain item holds, locked: the service account token, and how often Chrome Relay asks
// before agents sign in with it. They're kept together so no file an agent could edit loosens the approvals:
// writing a new item takes the token itself, and changing the setting (`chrome-relay vault approve`) takes the
// person's approval.
type Token struct {
	Value   string `json:"token"`
	Approve string `json:"approve"` // "sign-in", "session" or "off"
}

// SaveToken locks t to this Mac's Secure Enclave and stores it. Asks nothing of the person.
func SaveToken(instance, key string, t Token) error {
	b, _ := json.Marshal(t)
	locked, err := macos.Lock(b, true)
	if err != nil {
		return err
	}
	return macos.KeychainSet(keychainService, keychainAccount(instance, key), locked)
}

// HasToken reports whether a token is stored (without opening it).
func HasToken(instance, key string) bool {
	_, err := macos.KeychainGet(keychainService, keychainAccount(instance, key))
	return err == nil
}

// ForgetToken removes the stored token.
func ForgetToken(instance, key string) error {
	return macos.KeychainDelete(keychainService, keychainAccount(instance, key))
}

// TokenStamp identifies the stored token without opening it (no prompt: the item is this program's own).
func TokenStamp(instance, key string) string {
	locked, err := macos.KeychainGet(keychainService, keychainAccount(instance, key))
	if err != nil {
		return ""
	}
	h := sha256.Sum256(locked)
	return hex.EncodeToString(h[:8])
}

// MacPerson is the person at this Mac: Touch ID through the Secure Enclave and LocalAuthentication.
type MacPerson struct{ Instance, CLI string }

func (p MacPerson) Unlock(key, reason string) (Token, error) {
	locked, err := macos.KeychainGet(keychainService, keychainAccount(p.Instance, key))
	if err == macos.ErrNotFound {
		return Token{}, fmt.Errorf("no 1Password service account stored: ask the person to run `%s vault setup <vault>`", p.CLI)
	}
	if err != nil {
		return Token{}, err
	}
	plain, err := macos.Unlock(locked, reason)
	if err != nil {
		return Token{}, err
	}
	var t Token
	if json.Unmarshal(plain, &t) != nil || t.Value == "" {
		t = Token{Value: string(plain), Approve: "sign-in"} // an older Chrome Relay stored the bare token
	}
	if !macos.Silent(locked) || t.Approve == "" {
		// saved by an older Chrome Relay, behind Touch ID: this was its last prompt
		if t.Approve == "" {
			t.Approve = "sign-in"
		}
		SaveToken(p.Instance, key, t)
	}
	return t, nil
}

// Approve asks for Touch ID, and, with a paired iPhone (`chrome-relay phone pair`), Face ID on the phone at the
// same time: the first approval wins.
func (p MacPerson) Approve(d phone.Details) error {
	ph, err := phone.Load(p.Instance)
	if err != nil || ph == nil {
		return macos.Authenticate(d.Prompt())
	}
	return phone.Either(p.Instance, ph, d, 2*time.Minute)
}

// Check that token opens the named vault; returns the vault's exact name. Used by `chrome-relay vault setup`.
func Check(ctx context.Context, token, name, version string) (string, error) {
	if !strings.HasPrefix(token, "ops_") {
		return "", fmt.Errorf("that is not a service account token (they start with ops_)")
	}
	c, err := op.NewClient(ctx, op.WithServiceAccountToken(token), op.WithIntegrationInfo("Chrome Relay", version))
	if err != nil {
		return "", err
	}
	vaults, err := c.Vaults().List(ctx)
	if err != nil {
		return "", err
	}
	var names []string
	for _, v := range vaults {
		if strings.EqualFold(v.Title, name) {
			return v.Title, nil
		}
		names = append(names, v.Title)
	}
	return "", fmt.Errorf("the service account cannot open %q (it can open: %s)", name, strings.Join(names, ", "))
}
