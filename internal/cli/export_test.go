package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aindeev/agent-chrome-relay/internal/macos"
	"github.com/aindeev/agent-chrome-relay/internal/vault"
)

type fakeSecrets struct {
	approve error
	items   map[string][]byte      // Keychain "Chrome Relay" items by account
	tokens  map[string]vault.Token // by "<instance>/<key>"
	asked   []string
}

func (f *fakeSecrets) Authenticate(reason string) error {
	f.asked = append(f.asked, reason)
	return f.approve
}
func (f *fakeSecrets) KeychainAccounts(service string) ([]string, error) {
	var out []string
	for a := range f.items {
		out = append(out, a)
	}
	return out, nil
}
func (f *fakeSecrets) KeychainGet(service, account string) ([]byte, error) {
	if b, ok := f.items[account]; ok {
		return b, nil
	}
	return nil, macos.ErrNotFound
}
func (f *fakeSecrets) Token(instance, key, reason string) (vault.Token, error) {
	if t, ok := f.tokens[instance+"/"+key]; ok {
		return t, nil
	}
	return vault.Token{}, macos.ErrNotFound
}
func (f *fakeSecrets) DeviceID() string { return "dev123" }

func exportCLI(t *testing.T, sec *fakeSecrets, dev bool) (*cli, *bytes.Buffer, *bytes.Buffer) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
  "onepassword": {"approve": "sign-in", "serviceAccounts": [
    {"account": "WORK", "label": "sam@acme.com", "vault": "Shared with Chrome Relay (acme)"},
    {"account": "HOME", "label": "sam@example.com", "vault": "Shared with Chrome Relay (personal)"},
    {"account": "GONE", "label": "old@example.com", "vault": "Old"}]},
  "passkeys": {"enabled": true, "approvalReuseSeconds": 120}}`), 0o600)
	os.MkdirAll(filepath.Join(home, "flows"), 0o700)
	os.WriteFile(filepath.Join(home, "flows", "github.com.json"), []byte(`{"merge {pr}": {"site": "github.com", "steps": []}}`), 0o600)
	os.WriteFile(filepath.Join(home, "flows", "broken.json"), []byte(`{not json`), 0o600)
	var out, errw bytes.Buffer
	return &cli{instance: instance{Name: "chrome-relay", Home: home, Dev: dev}, args: []string{"chrome-relay", "export", "--for", "s4"},
		out: &out, errw: &errw, secrets: sec}, &out, &errw
}

func TestExportForS4(t *testing.T) {
	before := map[string][]byte{
		"passkey key 0a1b":            []byte("handle-one"),
		"passkey key ffee":            []byte("handle-two"),
		"passkey key 9999 (dev-mode)": []byte("dev-handle"),
		"TypeSafe API key":            []byte("jev_key\n"),
		"1Password service account (locked to this Mac) WORK": []byte("locked"),
	}
	sec := &fakeSecrets{items: before, tokens: map[string]vault.Token{
		"/WORK": {Value: "ops_work", Approve: "sign-in"},
		"/HOME": {Value: "ops_home", Approve: "session"},
	}}
	c, out, errw := exportCLI(t, sec, false)
	cfgBefore, _ := os.ReadFile(c.configPath())
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	if len(sec.asked) != 1 || sec.asked[0] != "Move Chrome Relay's passkeys and 1Password access to S4 Code" {
		t.Fatal("Touch ID first, with the reason:", sec.asked)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal("one JSON object on stdout:", err, out.String())
	}
	want := `{"format":"chrome-relay-export/v1","deviceId":"dev123",` +
		`"passkeyKeys":[{"ref":"0a1b","handle":"aGFuZGxlLW9uZQ=="},{"ref":"ffee","handle":"aGFuZGxlLXR3bw=="}],` +
		`"serviceAccounts":[{"account":"WORK","label":"sam@acme.com","vault":"Shared with Chrome Relay (acme)","token":"ops_work","approve":"sign-in"},` +
		`{"account":"HOME","label":"sam@example.com","vault":"Shared with Chrome Relay (personal)","token":"ops_home","approve":"session"}],` +
		`"settings":{"passkeysEnabled":true,"passkeyReuseSeconds":120},"deciderKey":"jev_key",` +
		`"flows":{"github.com":{"merge {pr}":{"site":"github.com","steps":[]}}}}` + "\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
	said := errw.String()
	if strings.Contains(said, "ops_") || strings.Contains(said, "jev_key") || strings.Contains(said, "aGFu") {
		t.Fatal("no secret on stderr:", said)
	}
	if said != "Exported 2 passkey keys, 2 service accounts (no token stored for old@example.com), 1 learned flows and the TypeSafe key for S4 Code.\n" {
		t.Fatal(said)
	}
	if cfgAfter, _ := os.ReadFile(c.configPath()); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatal("the config is unchanged")
	}
}

func TestExportDevModeOnlyItsOwnKeys(t *testing.T) {
	sec := &fakeSecrets{items: map[string][]byte{"passkey key 0a1b": []byte("normal"), "passkey key 9999 (dev-mode)": []byte("dev")},
		tokens: map[string]vault.Token{"dev-mode/WORK": {Value: "ops_dev", Approve: "off"}}}
	c, out, _ := exportCLI(t, sec, true)
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	var d exportData
	json.Unmarshal(out.Bytes(), &d)
	if len(d.PasskeyKeys) != 1 || d.PasskeyKeys[0].Ref != "9999" || d.DeciderKey != nil {
		t.Fatal(d.PasskeyKeys, d.DeciderKey)
	}
	if len(d.ServiceAccounts) != 1 || d.ServiceAccounts[0].Token != "ops_dev" || d.ServiceAccounts[0].Approve != "off" {
		t.Fatal(d.ServiceAccounts)
	}
}

func TestExportRefusedTouchIDWritesNothing(t *testing.T) {
	sec := &fakeSecrets{approve: macos.ErrDeclined, items: map[string][]byte{"passkey key 0a1b": []byte("h")}}
	c, out, _ := exportCLI(t, sec, false)
	if err := c.run(); err != macos.ErrDeclined {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("nothing on stdout:", out.String())
	}
}

func TestExportEmptyAndUsage(t *testing.T) {
	var out, errw bytes.Buffer
	c := &cli{instance: instance{Name: "chrome-relay", Home: t.TempDir()}, args: []string{"chrome-relay", "export", "--for", "s4"},
		out: &out, errw: &errw, secrets: &fakeSecrets{}}
	if err := c.run(); err != nil {
		t.Fatal(err)
	}
	want := `{"format":"chrome-relay-export/v1","deviceId":"dev123","passkeyKeys":[],"serviceAccounts":[],` +
		`"settings":{"passkeysEnabled":true,"passkeyReuseSeconds":0},"deciderKey":null,"flows":{}}` + "\n"
	if out.String() != want {
		t.Fatal(out.String())
	}
	c.args = []string{"chrome-relay", "export"}
	if err := c.run(); err == nil || !strings.Contains(err.Error(), "export --for s4") {
		t.Fatal(err)
	}
}
