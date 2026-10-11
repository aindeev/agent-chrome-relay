package cli

import (
	"bytes"
	"context"
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

func TestCheckDuplicates(t *testing.T) {
	home := source{Account: "HOME", Label: "me@home", Vaults: []string{"Private", "Shared"}}
	work := source{Account: "WORK", Label: "me@work", Vaults: []string{"Employee"}}
	if err := checkDuplicates([]source{home, work}, "Agents"); err != nil {
		t.Error(err)
	}
	clash := source{Account: "WORK", Label: "me@work", Vaults: []string{"private"}}
	err := checkDuplicates([]source{home, clash}, "")
	if err == nil || !strings.Contains(err.Error(), `"private" (me@home and me@work)`) {
		t.Errorf("case-insensitive clash across accounts: %v", err)
	}
	err = checkDuplicates([]source{home}, "Shared")
	if err == nil || !strings.Contains(err.Error(), "me@home and service account") {
		t.Errorf("clash with the service account's vault: %v", err)
	}
}

func TestApplyDesktopKeepsTheRest(t *testing.T) {
	v, _ := parseJSON([]byte(`{"extensionIds":["abc"],"onepassword":{"vault":"Agents","op":"/opt/homebrew/bin/op","approve":"sign-in"},"other":1}`))
	cfg := v.(*object)
	sources := []source{{Account: "HOME", Label: "me@home", Vaults: []string{"Private"}}}
	applyDesktop(cfg, sources, "Agents", "")
	b, _ := marshal(cfg)
	want := `{"extensionIds":["abc"],"onepassword":{"vault":"Agents","approve":"sign-in","accounts":[{"account":"HOME","label":"me@home","vaults":["Private"]}]},"other":1}`
	if string(b) != want {
		t.Errorf("\n%s\nwant\n%s", b, want)
	}
	applyDesktop(cfg, sources, "", "session") // the person dropped the service account
	b, _ = marshal(cfg)
	want = `{"extensionIds":["abc"],"onepassword":{"approve":"session","accounts":[{"account":"HOME","label":"me@home","vaults":["Private"]}]},"other":1}`
	if string(b) != want {
		t.Errorf("\n%s\nwant\n%s", b, want)
	}
	// and the relay's reading of it comes back the same
	got := configuredSources(cfg)
	if len(got) != 1 || got[0].Account != "HOME" || got[0].label() != "me@home" || strings.Join(got[0].Vaults, ",") != "Private" {
		t.Errorf("%+v", got)
	}
}

func TestOkLine(t *testing.T) {
	got := okLine([]source{
		{Account: "HOME", Label: "sam@example.com", Vaults: []string{"Private", "Shared with Chrome Relay"}},
		{Account: "WORK", Label: "sam@acme.com", Vaults: []string{"Employee"}},
	}, "")
	want := `ok: agents can use "Private", "Shared with Chrome Relay" (sam@example.com) and "Employee" (sam@acme.com). Nothing to restart.`
	if got != want {
		t.Errorf("\n%s\n%s", got, want)
	}
	if got := okLine(nil, "Agents"); got != `ok: agents can use "Agents" (service account). Nothing to restart.` {
		t.Error(got)
	}
}

func TestVaultListGrouping(t *testing.T) {
	d := vaultSummary{Vaults: []string{"Private", "Employee", "Agents"}, Items: []vaultItem{
		{Vault: "Employee", Account: "WORK", Title: "Okta", Websites: []string{"https://x.okta.com"}, Username: "me@work", Password: true, OTP: true},
		{Vault: "Private", Account: "HOME", Title: "GitHub", Websites: []string{"https://github.com"}, Username: "me", Password: true, Passkey: "github.com"},
		{Vault: "Private", Account: "HOME", Title: "Note"},
	}}
	sources := []source{
		{Account: "HOME", Label: "me@home", Vaults: []string{"Private"}},
		{Account: "WORK", Label: "me@work", Vaults: []string{"Employee"}},
		{Vaults: []string{"Agents"}},
	}
	want := `1Password vaults agents may use: "Private", "Employee", "Agents". Agents fill a field with a reference, never the value:
  agent-browser fill @eN "op://<vault>/<item>/username"     (also: password, otp, or any field's name)

  Private (me@home)
    GitHub                         https://github.com                       me                           password, passkey for github.com
    Note                           (no website: cannot be filled)
  Employee (me@work)
    Okta                           https://x.okta.com                       me@work                      password, one-time code
  Agents (service account)
    (empty)
`
	if got := formatVaultList(d, sources); got != want {
		t.Errorf("\n%s\nwant\n%s", got, want)
	}
}

func TestParseSetupArgs(t *testing.T) {
	a, err := parseSetupArgs([]string{"--account", "HOME", "--vaults", "Private, Shared", "--account=WORK", "--vaults=Employee", "--approve", "off"})
	if err != nil || len(a.sources) != 2 || strings.Join(a.sources[0].Vaults, "|") != "Private|Shared" || a.sources[1].Account != "WORK" || a.approve != "off" {
		t.Errorf("%+v %v", a, err)
	}
	if a, err := parseSetupArgs([]string{"Agents"}); err != nil || a.service != "Agents" {
		t.Errorf("service account: %+v %v", a, err)
	}
	for _, bad := range [][]string{
		{"--vaults", "A"}, {"--account", "X"}, {"--approve", "always"}, {"--account"}, {"Agents", "--account", "X", "--vaults", "A"}, {"--nope"},
	} {
		if _, err := parseSetupArgs(bad); err == nil {
			t.Errorf("%v should be refused", bad)
		}
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

func fakeEnv(t *testing.T) desktopEnv {
	return desktopEnv{
		accounts: func() []opAccount { a, _ := parseOpAccounts([]byte(opAccountsJSON)); return a },
		vaults: func(ctx context.Context, account string) ([]string, error) {
			return map[string][]string{"HOME": {"Private", "Shared with Chrome Relay", "Old"}, "WORK": {"Employee", "Private"}}[account], nil
		},
	}
}

func TestDesktopSetupInteractive(t *testing.T) {
	c, out, _ := testCLI(t, `{"extensionIds":["abc"]}`)
	var screen bytes.Buffer
	answers := "\n2,1\n1,2\n1\n\n"
	if err := c.desktopSetup(newPrompter(strings.NewReader(answers), &screen), fakeEnv(t), true); err != nil {
		t.Fatal(err, screen.String())
	}
	transcript := screen.String() + out.String()
	t.Log("\n" + transcript)
	for _, s := range []string{
		"  1) sam@acme.com  acme.1password.com\n",
		"1Password will ask you to allow Chrome Relay for sam@example.com…\n",
		"Vaults in sam@acme.com:\n  1) Employee\n  2) Private\n",
		`ok: agents can use "Private", "Shared with Chrome Relay" (sam@example.com) and "Employee" (sam@acme.com). Nothing to restart.`,
	} {
		if !strings.Contains(transcript, s) {
			t.Errorf("transcript lacks %q", s)
		}
	}
	b, _ := os.ReadFile(filepath.Join(c.Home, "config.json"))
	want := `{
  "extensionIds": [
    "abc"
  ],
  "onepassword": {
    "accounts": [
      {
        "account": "HOME",
        "label": "sam@example.com",
        "vaults": [
          "Private",
          "Shared with Chrome Relay"
        ]
      },
      {
        "account": "WORK",
        "label": "sam@acme.com",
        "vaults": [
          "Employee"
        ]
      }
    ],
    "approve": "session"
  }
}`
	if string(b) != want {
		t.Errorf("config.json:\n%s", b)
	}
}

func TestDesktopSetupRefusesDuplicates(t *testing.T) {
	c, _, _ := testCLI(t, "")
	var screen bytes.Buffer
	answers := "2,1\n1\n2\n\n" // Private from both accounts
	err := c.desktopSetup(newPrompter(strings.NewReader(answers), &screen), fakeEnv(t), false)
	if err == nil || !strings.Contains(err.Error(), `called "Private"`) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.Home, "config.json")); err == nil {
		t.Error("nothing is written when refused")
	}
}

func TestDesktopSetupFlags(t *testing.T) {
	c, out, _ := testCLI(t, `{"onepassword":{"vault":"Agents","approve":"sign-in"}}`)
	a, _ := parseSetupArgs([]string{"--account", "sam@example.com", "--vaults", "private"})
	if err := c.desktopSetupFlags(a, fakeEnv(t)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `ok: agents can use "Private" (sam@example.com) and "Agents" (service account). Nothing to restart.` {
		t.Error(got)
	}
	got := configuredSources(c.config())
	if len(got) != 2 || got[0].Account != "HOME" || got[0].Vaults[0] != "Private" || got[1].Vaults[0] != "Agents" || c.config().obj("onepassword").str("approve") != "sign-in" {
		t.Errorf("%+v", got)
	}
	a, _ = parseSetupArgs([]string{"--account", "HOME", "--vaults", "Nope"})
	if err := c.desktopSetupFlags(a, fakeEnv(t)); err == nil || !strings.Contains(err.Error(), `has no vault "Nope"`) {
		t.Errorf("got %v", err)
	}
}

func TestVaultListOlderRelay(t *testing.T) {
	d := vaultSummary{Vault: "Agents", Items: []vaultItem{{Title: "GitHub", Websites: []string{"github.com"}, Username: "bot", OTP: true}}}
	got := formatVaultList(d, []source{{Vaults: []string{"Agents"}}})
	if !strings.Contains(got, "  Agents (service account)\n    GitHub ") || strings.Contains(got, `""`) {
		t.Error(got)
	}
}
