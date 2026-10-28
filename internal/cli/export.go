package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/aindeev/agent-chrome-relay/internal/device"
	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

// exportReason is the Touch ID prompt: "chrome-relay is trying to <reason>".
const exportReason = "Move Chrome Relay's passkeys and 1Password access to S4 Code"

// exportFormat names the JSON S4 Code's import reads; a change to its shape needs a new version.
const exportFormat = "chrome-relay-export/v1"

// exportSecrets is what export reads from this Mac: Touch ID, the Keychain and the Secure Enclave, or a test double.
type exportSecrets interface {
	Authenticate(reason string) error
	KeychainAccounts(service string) ([]string, error)
	KeychainGet(service, account string) ([]byte, error)
	Token(instance, key, reason string) (vault.Token, error)
	DeviceID() string
}

type macSecrets struct{}

func (macSecrets) Authenticate(reason string) error { return macos.Authenticate(reason) }
func (macSecrets) KeychainAccounts(service string) ([]string, error) {
	return macos.KeychainAccounts(service)
}
func (macSecrets) KeychainGet(service, account string) ([]byte, error) {
	return macos.KeychainGet(service, account)
}
func (macSecrets) Token(instance, key, reason string) (vault.Token, error) {
	return vault.ReadToken(instance, key, reason)
}
func (macSecrets) DeviceID() string { return device.ID() }

type exportKey struct {
	Ref    string `json:"ref"`
	Handle string `json:"handle"`
}

type exportAccount struct {
	Account string `json:"account"`
	Label   string `json:"label"`
	Vault   string `json:"vault"`
	Token   string `json:"token"`
	Approve string `json:"approve"`
}

type exportSettings struct {
	PasskeysEnabled     bool `json:"passkeysEnabled"`
	PasskeyReuseSeconds int  `json:"passkeyReuseSeconds"`
}

type exportData struct {
	Format          string                     `json:"format"`
	DeviceID        string                     `json:"deviceId"`
	PasskeyKeys     []exportKey                `json:"passkeyKeys"`
	ServiceAccounts []exportAccount            `json:"serviceAccounts"`
	Settings        exportSettings             `json:"settings"`
	DeciderKey      *string                    `json:"deciderKey"`
	Flows           map[string]json.RawMessage `json:"flows"`
}

// export writes everything another app on this Mac needs to take over from this install, as one JSON object
// on stdout, after Touch ID:
//
//	chrome-relay export --for s4    the passkeys' Secure Enclave handles, the 1Password service accounts with
//	                                their tokens, passkey settings, the TypeSafe key and learned flows, for S4 Code
//
// It changes nothing here, so Chrome Relay keeps working afterwards. Only stdout carries secrets: stderr gets
// one line of counts. S4 Code runs it itself and reads stdout through a pipe; it refuses to print to a terminal.
func (c *cli) export() error {
	if c.arg(2) != "--for" || c.arg(3) != "s4" || len(c.args) > 4 {
		return fmt.Errorf("usage: %s export --for s4", c.Name)
	}
	if f, ok := c.out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return errors.New("this prints secrets: S4 Code runs it and reads them itself (Settings > Chrome > Import from Chrome Relay)")
	}
	sec := c.secrets
	if sec == nil {
		if !macos.Linked {
			return errors.New("this chrome-relay was built without Xcode's command line tools, so it keeps no passkeys or 1Password tokens to export")
		}
		sec = macSecrets{}
	}
	if err := sec.Authenticate(exportReason); err != nil {
		return err
	}
	d, skipped, err := c.collectExport(sec)
	if err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.out, "%s\n", b); err != nil {
		return err
	}
	decider := "no TypeSafe key"
	if d.DeciderKey != nil {
		decider = "the TypeSafe key"
	}
	note := ""
	if len(skipped) > 0 {
		note = fmt.Sprintf(" (no token stored for %s)", strings.Join(skipped, ", "))
	}
	fmt.Fprintf(c.errw, "Exported %d passkey keys, %d service accounts%s, %d learned flows and %s for S4 Code.\n",
		len(d.PasskeyKeys), len(d.ServiceAccounts), note, len(d.Flows), decider)
	return nil
}

// collectExport reads, never writes. skipped: configured service accounts whose token isn't in the Keychain.
func (c *cli) collectExport(sec exportSecrets) (exportData, []string, error) {
	d := exportData{Format: exportFormat, DeviceID: sec.DeviceID(), PasskeyKeys: []exportKey{},
		ServiceAccounts: []exportAccount{}, Flows: map[string]json.RawMessage{}}

	accounts, err := sec.KeychainAccounts(keychainService)
	if err != nil {
		return d, nil, fmt.Errorf("Keychain: %v", err)
	}
	for _, ref := range passkeyRefs(accounts, c.vaultInstance()) {
		handle, err := sec.KeychainGet(keychainService, chipAccount(ref, c.vaultInstance()))
		if err != nil {
			return d, nil, fmt.Errorf("passkey key %s: %v", ref, err)
		}
		d.PasskeyKeys = append(d.PasskeyKeys, exportKey{Ref: ref, Handle: base64.StdEncoding.EncodeToString(handle)})
	}

	var skipped []string
	for _, s := range configuredSources(c.config()) {
		t, err := sec.Token(c.vaultInstance(), s.Key, exportReason)
		if errors.Is(err, macos.ErrNotFound) {
			skipped = append(skipped, cmpOr(s.Label, s.Vaults[0]))
			continue
		}
		if err != nil {
			return d, nil, fmt.Errorf("the %s: %v", s.label(), err)
		}
		d.ServiceAccounts = append(d.ServiceAccounts, exportAccount{Account: s.Key, Label: s.Label, Vault: s.Vaults[0],
			Token: t.Value, Approve: t.Approve})
	}

	d.Settings = c.exportedPasskeySettings()

	key, err := sec.KeychainGet(keychainService, jevAccount)
	switch {
	case err == nil && strings.TrimSpace(string(key)) != "":
		k := strings.TrimSpace(string(key))
		d.DeciderKey = &k
	case err != nil && !errors.Is(err, macos.ErrNotFound):
		return d, nil, fmt.Errorf("TypeSafe key: %v", err)
	}

	files, _ := filepath.Glob(filepath.Join(c.Home, "flows", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || !json.Valid(b) {
			continue
		}
		d.Flows[strings.TrimSuffix(filepath.Base(f), ".json")] = json.RawMessage(b)
	}
	return d, skipped, nil
}

// exportedPasskeySettings: as the relay reads them (on unless turned off; reuse at most 300 seconds).
func (c *cli) exportedPasskeySettings() exportSettings {
	out := exportSettings{PasskeysEnabled: true}
	pk, _ := c.config().get("passkeys").(*object)
	if pk == nil {
		return out
	}
	if on, ok := pk.get("enabled").(bool); ok {
		out.PasskeysEnabled = on
	}
	n, ok := pk.get("approvalReuseSeconds").(json.Number)
	if !ok {
		n, ok = pk.get("touchIdReuseSeconds").(json.Number)
	}
	if ok {
		if v, err := n.Float64(); err == nil && v > 0 {
			out.PasskeyReuseSeconds = int(min(v, 300))
		}
	}
	return out
}

// chipAccount is the Keychain account holding a relay-approved passkey's handle (relay.chip.account).
func chipAccount(ref, instance string) string {
	if instance != "" {
		return "passkey key " + ref + " (" + instance + ")"
	}
	return "passkey key " + ref
}

var passkeyAccount = regexp.MustCompile(`^passkey key ([0-9a-f]+)(?: \((.+)\))?$`)

// passkeyRefs: this install's passkey refs among the Keychain accounts, sorted.
func passkeyRefs(accounts []string, instance string) []string {
	var refs []string
	for _, a := range accounts {
		if m := passkeyAccount.FindStringSubmatch(a); m != nil && m[2] == instance {
			refs = append(refs, m[1])
		}
	}
	sort.Strings(refs)
	return slices.Compact(refs)
}
