package vault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	op "github.com/1password/onepassword-sdk-go"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
)

// The service account token lives in the login Keychain, in an item only this signed program reads freely, and
// even that holds it only locked to this Mac's Secure Enclave: opening it takes the chip and the person's Touch ID.
const keychainService = "Chrome Relay"

func keychainAccount(instance string) string {
	if instance == "" {
		return "1Password service account (locked to this Mac)"
	}
	return "1Password service account (locked to this Mac, " + instance + ")"
}

// SaveToken locks token to the Secure Enclave and stores it. Asks nothing of the person (opening it will).
func SaveToken(instance, token string) error {
	locked, err := macos.Lock([]byte(token))
	if err != nil {
		return err
	}
	return macos.KeychainSet(keychainService, keychainAccount(instance), locked)
}

// HasToken reports whether a token is stored (without opening it).
func HasToken(instance string) bool {
	_, err := macos.KeychainGet(keychainService, keychainAccount(instance))
	return err == nil
}

// ForgetToken removes the stored token.
func ForgetToken(instance string) error {
	return macos.KeychainDelete(keychainService, keychainAccount(instance))
}

// TokenStamp identifies the stored token without opening it (no prompt: the item is this program's own).
func TokenStamp(instance string) string {
	locked, err := macos.KeychainGet(keychainService, keychainAccount(instance))
	if err != nil {
		return ""
	}
	h := sha256.Sum256(locked)
	return hex.EncodeToString(h[:8])
}

// MacPerson is the person at this Mac: Touch ID through the Secure Enclave and LocalAuthentication.
type MacPerson struct{ Instance, CLI string }

func (p MacPerson) Unlock(reason string) (string, error) {
	locked, err := macos.KeychainGet(keychainService, keychainAccount(p.Instance))
	if err == macos.ErrNotFound {
		return "", fmt.Errorf("no 1Password service account stored: ask the person to run `%s vault setup <vault>`", p.CLI)
	}
	if err != nil {
		return "", err
	}
	token, err := macos.Unlock(locked, reason)
	return string(token), err
}
func (p MacPerson) Approve(reason string) error { return macos.Authenticate(reason) }

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
