package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// what `op account list --format json` prints (keys checked against op 2.x)
const opAccountsJSON = `[
  {"url": "acme.1password.com", "email": "sam@acme.com", "user_uuid": "U1", "account_uuid": "WORK"},
  {"url": "my.1password.com", "email": "sam@example.com", "user_uuid": "U2", "account_uuid": "HOME"}
]`

func TestParseOpAccounts(t *testing.T) {
	accts, err := parseOpAccounts([]byte(opAccountsJSON))
	if err != nil || len(accts) != 2 {
		t.Fatal(accts, err)
	}
	if a := accts[1]; a.UUID != "HOME" || a.Email != "sam@example.com" || a.URL != "my.1password.com" || a.label() != "sam@example.com" {
		t.Errorf("%+v", a)
	}
	if _, err := parseOpAccounts([]byte("not json")); err == nil {
		t.Error("bad JSON is an error")
	}
}

func TestParseChoice(t *testing.T) {
	if got, err := parseChoice("3, 1,3", 3, true); err != nil || len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Error(got, err)
	}
	for _, bad := range []string{"", "0", "4", "x", "1,2"} {
		if _, err := parseChoice(bad, 3, false); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func testCLI(t *testing.T, config string) (*cli, *bytes.Buffer, *bytes.Buffer) {
	home := t.TempDir()
	if config != "" {
		os.WriteFile(filepath.Join(home, "config.json"), []byte(config), 0o644)
	}
	var out, errw bytes.Buffer
	c := &cli{instance: newInstance(true, env(map[string]string{"CHROME_RELAY_HOME": home}), home, filepath.Join(home, "chrome-relay")), out: &out, errw: &errw}
	return c, &out, &errw
}

func TestVaultListOlderRelay(t *testing.T) {
	d := vaultSummary{Vault: "Agents", Items: []vaultItem{{Title: "GitHub", Websites: []string{"github.com"}, Username: "bot", OTP: true}}}
	got := formatVaultList(d, []source{{Vaults: []string{"Agents"}}})
	if !strings.Contains(got, "  Agents (service account)\n    GitHub ") || strings.Contains(got, `""`) {
		t.Error(got)
	}
}

func TestCheckDuplicates(t *testing.T) {
	home := source{Key: "HOME", Label: "me@home", Vaults: []string{"Shared (personal)"}}
	work := source{Key: "WORK", Label: "me@work", Vaults: []string{"Shared (acme)"}}
	if err := checkDuplicates([]source{home, work}, "Agents"); err != nil {
		t.Error(err)
	}
	clash := source{Key: "WORK", Label: "me@work", Vaults: []string{"shared (PERSONAL)"}}
	err := checkDuplicates([]source{home, clash}, "")
	if err == nil || !strings.Contains(err.Error(), `(service account in me@home and service account in me@work)`) {
		t.Errorf("case-insensitive clash across accounts: %v", err)
	}
}

func TestVaultListGrouping(t *testing.T) {
	d := vaultSummary{Vaults: []string{"Shared (personal)", "Shared (acme)"}, Items: []vaultItem{
		{Vault: "Shared (acme)", Account: "me@work", Title: "Okta", Websites: []string{"https://x.okta.com"}, Username: "me@work", Password: true, OTP: true},
		{Vault: "Shared (personal)", Account: "me@home", Title: "GitHub", Websites: []string{"https://github.com"}, Username: "me", Password: true, Passkey: "github.com"},
	}}
	sources := []source{{Key: "HOME", Label: "me@home", Vaults: []string{"Shared (personal)"}}, {Key: "WORK", Label: "me@work", Vaults: []string{"Shared (acme)"}}}
	got := formatVaultList(d, sources)
	for _, want := range []string{"  Shared (personal) (service account in me@home)\n    GitHub", "password, passkey for github.com", "  Shared (acme) (service account in me@work)\n    Okta", "password, one-time code"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestParseSetupArgs(t *testing.T) {
	if a, err := parseSetupArgs(nil); err != nil || a.service != "" {
		t.Error("no arguments: ask", a, err)
	}
	for _, bad := range [][]string{{"--vaults", "A"}, {"--desktop"}, {"--approve", "off"}, {"--account"}, {"A", "B"}, {"--paste"}, {"--nope"}} {
		if _, err := parseSetupArgs(bad); err == nil {
			t.Errorf("%v should be refused", bad)
		}
	}
}
