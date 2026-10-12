package phone

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
)

const keychainService = "Chrome Relay"

func keychainAccount(instance string) string {
	if instance == "" {
		return "iPhone approvals"
	}
	return "iPhone approvals (" + instance + ")"
}

// Save keeps the pairing in Chrome Relay's own Keychain item: only this signed program reads or replaces it
// without macOS asking, so an agent can't swap in a phone of its own.
func Save(instance string, p *Pairing) error {
	b, _ := json.Marshal(p)
	return macos.KeychainSet(keychainService, keychainAccount(instance), b)
}

// Load: the paired phone, or nil when there is none.
func Load(instance string) (*Pairing, error) {
	b, err := macos.KeychainGet(keychainService, keychainAccount(instance))
	if errors.Is(err, macos.ErrNotFound) || errors.Is(err, macos.ErrUnsupported) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pairing
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func Forget(instance string) error {
	return macos.KeychainDelete(keychainService, keychainAccount(instance))
}

func apnsAccount(instance string) string {
	if instance == "" {
		return "APNs key"
	}
	return "APNs key (" + instance + ")"
}

// SaveAPNs keeps the person's APNs auth key in Chrome Relay's own Keychain item.
func SaveAPNs(instance string, a *APNs) error {
	b, _ := json.Marshal(a)
	return macos.KeychainSet(keychainService, apnsAccount(instance), b)
}

// LoadAPNs: the APNs key, or nil when there is none (notifications then go through ntfy).
func LoadAPNs(instance string) *APNs {
	b, err := macos.KeychainGet(keychainService, apnsAccount(instance))
	if err != nil {
		return nil
	}
	var a APNs
	if json.Unmarshal(b, &a) != nil || a.KeyID == "" {
		return nil
	}
	return &a
}

// ForgetAPNs removes the APNs key: notifications go back to ntfy.
func ForgetAPNs(instance string) error {
	return macos.KeychainDelete(keychainService, apnsAccount(instance))
}

// Ask is how the Mac reaches the phone: the latest push address (saved when it changed), and Apple push when
// there is an APNs key.
func Ask(ctx context.Context, instance string, p *Pairing, d Details, ttl time.Duration) error {
	t := Ntfy{Server: p.Server}
	refresh, cancel := context.WithTimeout(ctx, 5*time.Second)
	if p.Refresh(refresh, t) {
		Save(instance, p)
	}
	cancel()
	return p.Approve(ctx, t, LoadAPNs(instance), MacName(), d, ttl)
}

// MacName is how the phone (and 1Password) name this Mac: its name in System Settings > General > About.
func MacName() string {
	if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return strings.TrimSpace(string(out))
	}
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}

// Either asks Touch ID on the Mac and the paired phone at once; the first approval wins and the other prompt goes
// away. Pressing Cancel at the Mac, or Deny on the phone, refuses. A Mac prompt that times out (nobody there)
// leaves the phone to answer.
func Either(instance string, p *Pairing, d Details, ttl time.Duration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mac, phone := make(chan error, 1), make(chan error, 1)
	go func() { mac <- macos.AuthenticateContext(ctx, d.Reason) }()
	go func() { phone <- Ask(ctx, instance, p, d, ttl) }()
	var last error
	for range 2 {
		select {
		case err := <-mac:
			if err == nil || errors.Is(err, macos.ErrDeclined) {
				return err
			}
			last = err // timed out or unavailable: the phone may still answer
		case err := <-phone:
			if err == nil || errors.Is(err, ErrDenied) {
				return err
			}
			last = err
		}
	}
	return last
}
